package route

import (
	"context"
	"fmt"
	"strings"

	certman "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	tenancyv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1"
	"github.com/kcp-dev/logicalcluster/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apiRuntime "k8s.io/apimachinery/pkg/util/runtime"
	runtimeUtils "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	routeapiv1 "github.com/openshift/api/route/v1"

	certmaninformer "github.com/jetstack/cert-manager/pkg/client/informers/externalversions"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	kuadrantInformer "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/informers/externalversions"
	"github.com/kuadrant/kcp-glbc/pkg/dns"
	basereconciler "github.com/kuadrant/kcp-glbc/pkg/reconciler"
	"github.com/kuadrant/kcp-glbc/pkg/superClient"
	"github.com/kuadrant/kcp-glbc/pkg/tls"
	"github.com/kuadrant/kcp-glbc/pkg/traffic"
)

const (
	controllerName = "kcp-glbc-route"
)

// NewController returns a new Controller which reconciles Routes.
func NewController(config *ControllerConfig) *Controller {
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName)

	hostResolver := config.HostResolver
	switch impl := hostResolver.(type) {
	case *dns.ConfigMapHostResolver:
		impl.Client = config.SuperClient.WorkspaceClient(tenancyv1alpha1.RootCluster)
	}
	hostResolver = dns.NewSafeHostResolver(hostResolver)

	base := basereconciler.NewController(controllerName, queue)
	c := &Controller{
		Controller:                   base,
		certProvider:                 config.CertProvider,
		sharedInformerFactory:        config.KCPSharedInformerFactory,
		dynamicSharedInformerFactory: config.KCPDynamicSharedInformerFactory,
		glbcInformerFactory:          config.GlbcInformerFactory,
		domain:                       config.Domain,
		glbcWorkspace:                config.GLBCWorkspace,
		hostResolver:                 hostResolver,
		hostsWatcher:                 dns.NewHostsWatcher(&base.Logger, hostResolver, dns.DefaultInterval),
		certInformerFactory:          config.CertificateInformer,
		KCPInformerFactory:           config.KCPInformer,
		superClient:                  config.SuperClient,
	}
	c.Process = c.process
	c.hostsWatcher.OnChange = c.Enqueue

	c.startWatches()

	return c
}

func (c *Controller) resourceExists() bool {
	routeResource := schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}
	_, err := c.superClient.WorkspaceClient(c.glbcWorkspace).Discovery().ServerResourcesForGroupVersion(routeResource.GroupVersion().String())
	return err == nil
}

func (c *Controller) startWatches() {
	if !c.resourceExists() {
		c.Logger.Info("no routes resource detected; not starting route event handlers")
		return
	}
	c.Logger.Info("starting route event handlers")
	routeResource := schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}
	c.indexer = c.dynamicSharedInformerFactory.ForResource(routeResource).Informer().GetIndexer()
	c.routeLister = c.dynamicSharedInformerFactory.ForResource(routeResource).Lister()

	c.dynamicSharedInformerFactory.ForResource(routeResource).Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.Logger.Info("add route event")
			u := obj.(*unstructured.Unstructured)
			route := &routeapiv1.Route{}
			_ = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, route)
			c.Enqueue(route)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.Logger.Info("update route event")
			u := newObj.(*unstructured.Unstructured)
			route := &routeapiv1.Route{}
			_ = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, route)
			c.Enqueue(route)
		},
		DeleteFunc: func(obj interface{}) {
			c.Logger.Info("delete route event")
			u := obj.(*unstructured.Unstructured)
			route := &routeapiv1.Route{}
			_ = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, route)
			c.Enqueue(route)
		},
	})

	// Watch DomainVerifications in the GLBC Virtual Workspace
	c.KCPInformerFactory.Kuadrant().V1().DomainVerifications().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueueRoutes(c.routesFromDomainVerification),
		UpdateFunc: c.enqueueRoutesFromUpdate(c.routesFromDomainVerification),
		DeleteFunc: c.enqueueRoutes(c.routesFromDomainVerification),
	})

	// Watch Certificates in the GLBC Workspace
	// This is getting events relating to certificates in the glbc deployments workspace/namespace.
	// When more than one route controller is started, both will receive the same events, but only the one with the
	// appropriate indexer for the corresponding virtual workspace where the route is accessible will be able to
	// process the request.
	c.certInformerFactory.Certmanager().V1().Certificates().Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			certificate, ok := obj.(*certman.Certificate)
			if !ok {
				return false
			}
			if certificate.Labels == nil {
				return false
			}
			if _, ok := certificate.Labels[basereconciler.LABEL_HCG_MANAGED]; !ok {
				return false
			}
			if _, ok := certificate.Annotations[traffic.ANNOTATION_TRAFFIC_KEY]; !ok {
				return false
			}
			return true
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				certificate := obj.(*certman.Certificate)
				_, err := c.getRouteByKey(certificate.Annotations[traffic.ANNOTATION_TRAFFIC_KEY])
				if k8serrors.IsNotFound(err) {
					//not connected to a route, do not handle events
					return
				}
				traffic.CertificateAddedHandler(certificate)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldCert := oldObj.(*certman.Certificate)
				newCert := newObj.(*certman.Certificate)
				if oldCert.ResourceVersion == newCert.ResourceVersion {
					return
				}
				route, err := c.getRouteByKey(newCert.Annotations[traffic.ANNOTATION_TRAFFIC_KEY])
				if k8serrors.IsNotFound(err) {
					//not connected to a route, do not handle events
					return
				}
				enq := traffic.CertificateUpdatedHandler(oldCert, newCert)
				if enq {
					c.Enqueue(route)
				}
			},
			DeleteFunc: func(obj interface{}) {
				certificate := obj.(*certman.Certificate)
				route, err := c.getRouteByKey(certificate.Annotations[traffic.ANNOTATION_TRAFFIC_KEY])
				if k8serrors.IsNotFound(err) {
					//not connected to a route, do not handle events
					return
				}
				// handle metric requeue route if the cert is deleted and the route still exists
				// covers a manual deletion of cert and will ensure a new cert is created
				traffic.CertificateDeletedHandler(certificate)
				trafficKey := certificate.Annotations[traffic.ANNOTATION_TRAFFIC_KEY]
				c.Logger.V(3).Info("requeueing route certificate deleted", "certificate", certificate.Name, "traffic key", trafficKey)
				c.Enqueue(route)
			},
		},
	})

	// Watch TLS Secrets in the GLBC Workspace
	// This is getting events relating to secrets in the glbc deployments workspace/namespace.
	// When more than one route controller is started, both will receive the same events, but only the one with the
	// appropriate indexer for the corresponding virtual workspace where the route is accessible will be able to
	// process the request.
	c.glbcInformerFactory.Core().V1().Secrets().Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: traffic.CertificateSecretFilter,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				secret := obj.(*corev1.Secret)
				//issuer := secret.Annotations[tls.TlsIssuerAnnotation]
				//tlsCertificateSecretCount.WithLabelValues(issuer).Inc()
				trafficKey := secret.Annotations[traffic.ANNOTATION_TRAFFIC_KEY]
				c.Logger.V(3).Info("reqeuing route certificate tls secret created", "secret", secret.Name, "traffic key", trafficKey)
				c.enqueueRouteByKey(trafficKey)
			},
			UpdateFunc: func(old, obj interface{}) {
				newSecret := obj.(*corev1.Secret)
				oldSecret := obj.(*corev1.Secret)
				if oldSecret.ResourceVersion != newSecret.ResourceVersion {
					// we only care if the secret data changed
					if !equality.Semantic.DeepEqual(oldSecret.Data, newSecret.Data) {
						trafficKey := newSecret.Annotations[traffic.ANNOTATION_TRAFFIC_KEY]
						c.Logger.V(3).Info("reqeuing route certificate tls secret updated", "secret", newSecret.Name, "traffic key", trafficKey)
						c.enqueueRouteByKey(trafficKey)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				secret := obj.(*corev1.Secret)
				//issuer := secret.Annotations[tls.TlsIssuerAnnotation]
				//tlsCertificateSecretCount.WithLabelValues(issuer).Dec()
				trafficKey := secret.Annotations[traffic.ANNOTATION_TRAFFIC_KEY]
				c.Logger.V(3).Info("reqeuing route certificate tls secret deleted", "secret", secret.Name, "traffic key", trafficKey)
				c.enqueueRouteByKey(trafficKey)
			},
		},
	})

	// Watch DNSRecords in the GLBC Virtual Workspace
	c.KCPInformerFactory.Kuadrant().V1().DNSRecords().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			//when a dns record is deleted we requeue the route (currently owner refs don't work in KCP)
			dnsRecord := obj.(*kuadrantv1.DNSRecord)
			if dnsRecord.Annotations == nil {
				return
			}
			// if we have a route key stored we can re queue the route
			if trafficKey, ok := dnsRecord.Annotations[traffic.ANNOTATION_TRAFFIC_KEY]; ok {
				c.Logger.V(3).Info("reqeueuing route dns record deleted", "cluster", logicalcluster.From(dnsRecord), "namespace", dnsRecord.Namespace, "name", dnsRecord.Name, "traffic key", trafficKey)
				c.enqueueRouteByKey(trafficKey)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newdns := newObj.(*kuadrantv1.DNSRecord)
			olddns := oldObj.(*kuadrantv1.DNSRecord)
			if olddns.ResourceVersion != newdns.ResourceVersion {
				trafficKey := newObj.(*kuadrantv1.DNSRecord).Annotations[traffic.ANNOTATION_TRAFFIC_KEY]
				c.Logger.V(3).Info("reqeuing route dns record deleted", "cluster", logicalcluster.From(newdns), "namespace", newdns.Namespace, "name", newdns.Name, "traffic key", trafficKey)
				c.enqueueRouteByKey(trafficKey)
			}
		},
	})
}

type ControllerConfig struct {
	*basereconciler.ControllerConfig
	SuperClient superClient.Interface
	// informer for
	KCPSharedInformerFactory        informers.SharedInformerFactory
	KCPDynamicSharedInformerFactory dynamicinformer.DynamicSharedInformerFactory
	CertificateInformer             certmaninformer.SharedInformerFactory
	GlbcInformerFactory             informers.SharedInformerFactory
	KCPInformer                     kuadrantInformer.SharedInformerFactory
	Domain                          string
	CertProvider                    tls.Provider
	HostResolver                    dns.HostResolver
	GLBCWorkspace                   logicalcluster.Name
}

type Controller struct {
	*basereconciler.Controller
	superClient                  superClient.Interface
	sharedInformerFactory        informers.SharedInformerFactory
	dynamicSharedInformerFactory dynamicinformer.DynamicSharedInformerFactory
	indexer                      cache.Indexer
	routeLister                  cache.GenericLister
	certProvider                 tls.Provider
	domain                       string
	hostResolver                 dns.HostResolver
	hostsWatcher                 *dns.HostsWatcher
	certInformerFactory          certmaninformer.SharedInformerFactory
	glbcInformerFactory          informers.SharedInformerFactory
	KCPInformerFactory           kuadrantInformer.SharedInformerFactory
	glbcWorkspace                logicalcluster.Name
}

func (c *Controller) enqueueRouteByKey(key string) {
	route, err := c.getRouteByKey(key)
	//no need to handle not found as the route is gone
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return
		}
		runtimeUtils.HandleError(err)
		return
	}
	c.Enqueue(route)
}

func (c *Controller) process(ctx context.Context, key string) error {
	object, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if !exists {
		return nil
	}

	u := object.(*unstructured.Unstructured)
	current := &routeapiv1.Route{}
	currentStateReader := traffic.NewRoute(current)
	_ = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, current)
	target := current.DeepCopy()
	targetStateReadWriter := traffic.NewRoute(target)

	err = c.reconcile(ctx, targetStateReadWriter)
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(current, target) {
		// our ingress object is now in the correct state, before we commit lets apply any changes via a transform
		if err := targetStateReadWriter.Transform(currentStateReader); err != nil {
			return err
		}
		c.Logger.V(3).Info("attempting update of changed route ", "route key ", key, "TMC Enabled? ", targetStateReadWriter.TMCEnabed())
		raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(target)
		u = &unstructured.Unstructured{}
		u.Object = raw
		routeResource := schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}
		_, err = c.superClient.WorkspaceDynamicClient(logicalcluster.From(target)).Resource(routeResource).Namespace(target.Namespace).Update(ctx, u, metav1.UpdateOptions{})
		return err
	}

	return nil
}

// enqueueRoutes creates an event handler function given a function that
// returns a list of routes to enqueue, or an error. If an error is returned,
// no routes are enqueued.
func (c *Controller) enqueueRoutes(getRoutes func(obj interface{}) ([]*routeapiv1.Route, error)) func(obj interface{}) {
	return func(obj interface{}) {
		routes, err := getRoutes(obj)
		if err != nil {
			apiRuntime.HandleError(err)
			return
		}

		for _, route := range routes {
			trafficKey, err := cache.MetaNamespaceKeyFunc(route)
			if err != nil {
				apiRuntime.HandleError(err)
				continue
			}

			c.Queue.Add(trafficKey)
		}
	}
}

func (c *Controller) enqueueRoutesFromUpdate(getRoutes func(obj interface{}) ([]*routeapiv1.Route, error)) func(oldObj, newObj interface{}) {
	return func(oldObj, newObj interface{}) {
		c.enqueueRoutes(getRoutes)(newObj)
	}
}

func (c *Controller) routesFromDomainVerification(obj interface{}) ([]*routeapiv1.Route, error) {
	dv := obj.(*kuadrantv1.DomainVerification)
	domain := strings.ToLower(strings.TrimSpace(dv.Spec.Domain))

	// find all routes in this workspace with pending hosts that contain this domains
	routeList, err := c.routeLister.List(labels.SelectorFromSet(labels.Set{
		traffic.LABEL_HAS_PENDING_HOSTS: "true",
	}))
	if err != nil {
		c.Logger.Info("error listing routes")
		return nil, err
	}

	var routesToEnqueue []*routeapiv1.Route

	for _, object := range routeList {
		u := object.(*unstructured.Unstructured)
		route := &routeapiv1.Route{}
		_ = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, route)
		pendingAnnotation, ok := route.Annotations[traffic.ANNOTATION_PENDING_CUSTOM_HOSTS]
		if !ok {
			continue
		}

		if !HostMatches(strings.ToLower(strings.TrimSpace(pendingAnnotation)), domain) {
			continue
		}

		routesToEnqueue = append(routesToEnqueue, route)
	}

	return routesToEnqueue, nil
}

func (c *Controller) getRouteByKey(key string) (*routeapiv1.Route, error) {
	object, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, k8serrors.NewNotFound(routeapiv1.Resource("route"), key)
	}

	u := object.(*unstructured.Unstructured)
	route := &routeapiv1.Route{}
	_ = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, route)
	return route, nil
}

func HostMatches(host, domain string) bool {
	if host == domain {
		return true
	}

	parentHostParts := strings.SplitN(host, ".", 2)
	if len(parentHostParts) < 2 {
		return false
	}
	return HostMatches(parentHostParts[1], domain)
}

func (c *Controller) GetDomainVerifications(ctx context.Context, accessor traffic.Interface) (*kuadrantv1.DomainVerificationList, error) {
	return c.superClient.WorkspaceKuadrantClient(logicalcluster.From(accessor)).KuadrantV1().DomainVerifications().List(ctx, metav1.ListOptions{})
}

func (c *Controller) GetSecret(ctx context.Context, name, namespace string, cluster logicalcluster.Name) (*corev1.Secret, error) {
	return c.superClient.WorkspaceClient(cluster).CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *Controller) DeleteTLSSecret(ctx context.Context, workspace logicalcluster.Name, namespace, name string) error {
	if err := c.superClient.WorkspaceClient(workspace).CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (c *Controller) CopySecret(ctx context.Context, workspace logicalcluster.Name, namespace string, secret *corev1.Secret) error {
	secret.ResourceVersion = ""
	secretClient := c.superClient.WorkspaceClient(workspace).CoreV1().Secrets(namespace)
	_, err := secretClient.Create(ctx, secret, metav1.CreateOptions{})
	if err != nil && k8serrors.IsAlreadyExists(err) {
		s, err := secretClient.Get(ctx, secret.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		s.Data = secret.Data
		if _, err := secretClient.Update(ctx, s, metav1.UpdateOptions{}); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	return nil
}
func (c *Controller) UpdateDNS(ctx context.Context, dns *kuadrantv1.DNSRecord) (*kuadrantv1.DNSRecord, error) {
	updated, err := c.superClient.WorkspaceKuadrantClient(logicalcluster.From(dns)).KuadrantV1().DNSRecords(dns.Namespace).Update(ctx, dns, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (c *Controller) DeleteDNS(ctx context.Context, accessor traffic.Interface) error {
	return c.superClient.WorkspaceKuadrantClient(logicalcluster.From(accessor)).KuadrantV1().DNSRecords(accessor.GetNamespace()).Delete(ctx, accessor.GetName(), metav1.DeleteOptions{})
}

func (c *Controller) GetDNS(ctx context.Context, accessor traffic.Interface) (*kuadrantv1.DNSRecord, error) {
	return c.superClient.WorkspaceKuadrantClient(logicalcluster.From(accessor)).KuadrantV1().DNSRecords(accessor.GetNamespace()).Get(ctx, accessor.GetName(), metav1.GetOptions{})
}
func (c *Controller) CreateDNS(ctx context.Context, dnsRecord *kuadrantv1.DNSRecord) (*kuadrantv1.DNSRecord, error) {
	return c.superClient.WorkspaceKuadrantClient(logicalcluster.From(dnsRecord)).KuadrantV1().DNSRecords(dnsRecord.Namespace).Create(ctx, dnsRecord, metav1.CreateOptions{})
}
func (c *Controller) DeleteRoute(ctx context.Context, o traffic.Interface) error {
	routeResource := schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}
	r := o.(*traffic.Route)
	err := c.superClient.WorkspaceDynamicClient(r.GetLogicalCluster()).Resource(routeResource).Namespace(r.GetNamespace()).Delete(ctx, r.GetName(), metav1.DeleteOptions{})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	return err
}
func (c *Controller) CreateOrUpdateRoute(ctx context.Context, o traffic.Interface) error {
	routeResource := schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}

	r := o.(*traffic.Route)
	u, err := c.superClient.WorkspaceDynamicClient(r.GetLogicalCluster()).Resource(routeResource).Namespace(r.GetNamespace()).Get(ctx, r.GetName(), metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) { //doesn't exist, create it
			r.Route.ResourceVersion = ""
			raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(r.Route)
			u = &unstructured.Unstructured{}
			u.Object = raw
			_, err = c.superClient.WorkspaceDynamicClient(r.GetLogicalCluster()).Resource(routeResource).Namespace(r.GetNamespace()).Create(ctx, u, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("shadow creation error: %v", err.Error())
			}
			return nil
		}
		//unknown error, report it
		return fmt.Errorf("error retrieving shadow: %v", err)
	}

	//Convert unstructured into route object and update it
	shadow := &routeapiv1.Route{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, shadow)
	if err != nil {
		return fmt.Errorf("error converting shadow from unstructured: %v", err)
	}
	shadow.Spec = *r.Route.Spec.DeepCopy()

	//send back to API
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&shadow)
	if err != nil {
		return fmt.Errorf("error converting shadow to unstructured: %v", err)
	}
	post := &unstructured.Unstructured{}
	post.Object = raw
	_, err = c.superClient.WorkspaceDynamicClient(r.GetLogicalCluster()).Resource(routeResource).Namespace(r.GetNamespace()).Update(ctx, post, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating shadow, err: %v", err)
	}
	return nil
}
