package ingress

import (
	"context"
	"encoding/json"
	"strings"

	tenancyv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	networkingv1lister "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	"github.com/kuadrant/kcp-glbc/pkg/dns"
	"github.com/kuadrant/kcp-glbc/pkg/superClient"
	"github.com/kuadrant/kcp-glbc/pkg/traffic"

	"github.com/kcp-dev/logicalcluster/v2"

	certman "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	certmaninformer "github.com/jetstack/cert-manager/pkg/client/informers/externalversions"
	certmanlister "github.com/jetstack/cert-manager/pkg/client/listers/certmanager/v1"

	kuadrantInformer "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/informers/externalversions"
	basereconciler "github.com/kuadrant/kcp-glbc/pkg/reconciler"
	"github.com/kuadrant/kcp-glbc/pkg/tls"
)

const (
	defaultControllerName = "kcp-glbc-ingress"
)

// NewController returns a new Controller which reconciles Ingress.
func NewController(config *ControllerConfig) *Controller {
	controllerName := config.GetName(defaultControllerName)
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName)

	hostResolver := config.HostResolver
	switch impl := hostResolver.(type) {
	case *dns.ConfigMapHostResolver:
		impl.Client = config.SuperClient.WorkspaceClient(tenancyv1alpha1.RootCluster)
	}

	hostResolver = dns.NewSafeHostResolver(hostResolver)

	base := basereconciler.NewController(controllerName, queue)
	c := &Controller{
		Controller:              base,
		superClient:             config.SuperClient,
		certProvider:            config.CertProvider,
		sharedInformerFactory:   config.KCPSharedInformerFactory,
		glbcInformerFactory:     config.GlbcInformerFactory,
		domain:                  config.Domain,
		hostResolver:            hostResolver,
		hostsWatcher:            dns.NewHostsWatcher(&base.Logger, hostResolver, dns.DefaultInterval),
		certInformerFactory:     config.CertificateInformer,
		KuadrantInformerFactory: config.KuadrantInformer,
	}
	c.Process = c.process
	c.hostsWatcher.OnChange = c.Enqueue
	c.certificateLister = c.certInformerFactory.Certmanager().V1().Certificates().Lister()
	c.indexer = c.sharedInformerFactory.Networking().V1().Ingresses().Informer().GetIndexer()
	c.ingressLister = c.sharedInformerFactory.Networking().V1().Ingresses().Lister()

	// Watch Ingresses in the GLBC Virtual Workspace
	c.sharedInformerFactory.Networking().V1().Ingresses().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ingress := obj.(*networkingv1.Ingress)
			c.Logger.V(3).Info("enqueue ingress new ingress added", "cluster", logicalcluster.From(ingress), "namespace", ingress.Namespace, "name", ingress.Name)
			ingressObjectTotal.Inc()
			c.Enqueue(obj)
		},
		UpdateFunc: func(old, obj interface{}) {
			if old.(metav1.Object).GetResourceVersion() != obj.(metav1.Object).GetResourceVersion() {
				ingress := obj.(*networkingv1.Ingress)
				c.Logger.V(3).Info("enqueue ingress ingress updated", "cluster", logicalcluster.From(ingress), "namespace", ingress.Namespace, "name", ingress.Name)
				c.Enqueue(obj)
			}
		},
		DeleteFunc: func(obj interface{}) {
			ingress := obj.(*networkingv1.Ingress)
			c.Logger.V(3).Info("enqueue ingress deleted ", "cluster", logicalcluster.From(ingress), "namespace", ingress.Namespace, "name", ingress.Name)
			ingressObjectTotal.Dec()
			c.Enqueue(obj)
		},
	})

	// Watch DomainVerifications in the GLBC Virtual Workspace
	c.KuadrantInformerFactory.Kuadrant().V1().DomainVerifications().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueueIngresses(c.ingressesFromDomainVerification),
		UpdateFunc: c.enqueueIngressesFromUpdate(c.ingressesFromDomainVerification),
		DeleteFunc: c.enqueueIngresses(c.ingressesFromDomainVerification),
	})

	// Watch Certificates in the GLBC Workspace
	// This is getting events relating to certificates in the glbc deployments workspace/namespace.
	// When more than one ingress controller is started, both will receive the same events, but only the one with the
	// appropriate indexer for the corresponding virtual workspace where the ingress is accessible will be able to
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
			if _, ok := certificate.Annotations[traffic.ANNOTATION_TRAFFIC_KEY]; ok {
				return true
			}
			return true
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				certificate := obj.(*certman.Certificate)
				_, err := c.getIngressByKey(certificate.Annotations[traffic.ANNOTATION_TRAFFIC_KEY])
				if k8serrors.IsNotFound(err) {
					c.Logger.V(3).Info("cert is not for an ingress", "cert", certificate.Name, "obj key", certificate.Annotations[traffic.ANNOTATION_TRAFFIC_KEY])
					//not connected to an ingress, do not handle events
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
				ingress, err := c.getIngressByKey(newCert.Annotations[traffic.ANNOTATION_TRAFFIC_KEY])
				if k8serrors.IsNotFound(err) {
					c.Logger.V(3).Info("cert is not for an ingress", "cert", newCert.Name, "obj key", newCert.Annotations[traffic.ANNOTATION_TRAFFIC_KEY])
					//not connected to an ingress, do not handle events
					return
				}

				enq := traffic.CertificateUpdatedHandler(oldCert, newCert)
				if enq {
					c.Logger.V(3).Info("requeueing ingress certificate updated", "certificate", newCert.Name, "ingress", ingress.Name)
					c.Enqueue(ingress)
				}
			},
			DeleteFunc: func(obj interface{}) {
				certificate := obj.(*certman.Certificate)
				ingress, err := c.getIngressByKey(certificate.Annotations[traffic.ANNOTATION_TRAFFIC_KEY])
				if k8serrors.IsNotFound(err) {
					c.Logger.V(3).Info("cert is not for an ingress", "cert", certificate.Name, "obj key", certificate.Annotations[traffic.ANNOTATION_TRAFFIC_KEY])
					//not connected to an ingress, do not handle events
					return
				}
				// handle metric requeue ingress if the cert is deleted and the ingress still exists
				// covers a manual deletion of cert and will ensure a new cert is created
				traffic.CertificateDeletedHandler(certificate)
				c.Logger.V(3).Info("requeueing ingress certificate deleted", "certificate", certificate.Name, "ingress", ingress.Name)
				c.Enqueue(ingress)
			},
		},
	})

	// Watch TLS Secrets in the GLBC Workspace
	// This is getting events relating to secrets in the glbc deployments workspace/namespace.
	// When more than one ingress controller is started, both will receive the same events, but only the one with the
	// appropriate indexer for the corresponding virtual workspace where the ingress is accessible will be able to
	// process the request.
	c.glbcInformerFactory.Core().V1().Secrets().Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: traffic.CertificateSecretFilter,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				secret := obj.(*corev1.Secret)
				issuer := secret.Annotations[tls.TlsIssuerAnnotation]
				tlsCertificateSecretCount.WithLabelValues(issuer).Inc()
				ingressKey := secret.Annotations[traffic.ANNOTATION_TRAFFIC_KEY]
				c.Logger.V(3).Info("reqeuing ingress certificate tls secret created", "secret", secret.Name, "ingresskey", ingressKey)
				c.enqueueIngressByKey(ingressKey)
			},
			UpdateFunc: func(old, obj interface{}) {
				newSecret := obj.(*corev1.Secret)
				oldSecret := obj.(*corev1.Secret)
				if oldSecret.ResourceVersion != newSecret.ResourceVersion {
					// we only care if the secret data changed
					if !equality.Semantic.DeepEqual(oldSecret.Data, newSecret.Data) {
						ingressKey := newSecret.Annotations[traffic.ANNOTATION_TRAFFIC_KEY]
						c.Logger.V(3).Info("reqeuing ingress certificate tls secret updated", "secret", newSecret.Name, "ingresskey", ingressKey)
						c.enqueueIngressByKey(ingressKey)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				secret := obj.(*corev1.Secret)
				issuer := secret.Annotations[tls.TlsIssuerAnnotation]
				tlsCertificateSecretCount.WithLabelValues(issuer).Dec()
				ingressKey := secret.Annotations[traffic.ANNOTATION_TRAFFIC_KEY]
				c.Logger.V(3).Info("reqeuing ingress certificate tls secret deleted", "secret", secret.Name, "ingresskey", ingressKey)
				c.enqueueIngressByKey(ingressKey)
			},
		},
	})

	// Watch DNSRecords in the GLBC Virtual Workspace
	c.KuadrantInformerFactory.Kuadrant().V1().DNSRecords().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			//when a dns record is deleted we requeue the ingress (currently owner refs don't work in KCP)
			dnsRecords := obj.(*kuadrantv1.DNSRecord)
			if dnsRecords.Annotations == nil {
				return
			}
			// if we have a ingress key stored we can re queue the ingresss
			if ingressKey, ok := dnsRecords.Annotations[traffic.ANNOTATION_TRAFFIC_KEY]; ok {
				c.Logger.V(3).Info("reqeuing ingress dns record deleted", "cluster", logicalcluster.From(dnsRecords), "namespace", dnsRecords.Namespace, "name", dnsRecords.Name, "ingresskey", ingressKey)
				c.enqueueIngressByKey(ingressKey)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newdns := newObj.(*kuadrantv1.DNSRecord)
			olddns := oldObj.(*kuadrantv1.DNSRecord)
			if olddns.ResourceVersion != newdns.ResourceVersion {
				ingressKey := newObj.(*kuadrantv1.DNSRecord).Annotations[traffic.ANNOTATION_TRAFFIC_KEY]
				c.Logger.V(3).Info("reqeuing ingress dns record deleted", "cluster", logicalcluster.From(newdns), "namespace", newdns.Namespace, "name", newdns.Name, "ingresskey", ingressKey)
				c.enqueueIngressByKey(ingressKey)
			}
		},
	})

	return c
}

type ControllerConfig struct {
	*basereconciler.ControllerConfig
	SuperClient              superClient.Interface
	KCPSharedInformerFactory informers.SharedInformerFactory
	CertificateInformer      certmaninformer.SharedInformerFactory
	GlbcInformerFactory      informers.SharedInformerFactory
	KuadrantInformer         kuadrantInformer.SharedInformerFactory
	Domain                   string
	CertProvider             tls.Provider
	HostResolver             dns.HostResolver
	GLBCWorkspace            logicalcluster.Name
}

type Controller struct {
	*basereconciler.Controller
	superClient             superClient.Interface
	sharedInformerFactory   informers.SharedInformerFactory
	indexer                 cache.Indexer
	ingressLister           networkingv1lister.IngressLister
	certificateLister       certmanlister.CertificateLister
	certProvider            tls.Provider
	domain                  string
	hostResolver            dns.HostResolver
	hostsWatcher            *dns.HostsWatcher
	certInformerFactory     certmaninformer.SharedInformerFactory
	glbcInformerFactory     informers.SharedInformerFactory
	KuadrantInformerFactory kuadrantInformer.SharedInformerFactory
}

func (c *Controller) enqueueIngressByKey(key string) bool {
	ingress, err := c.getIngressByKey(key)
	//no need to handle not found as the ingress is gone
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		runtime.HandleError(err)
		return false
	}
	c.Enqueue(ingress)
	return true
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
		// The Ingress has been deleted, so we remove any Ingress to Service tracking.
		return nil
	}

	currentState := object.(*networkingv1.Ingress)
	targetState := currentState.DeepCopy()
	targetStateReadWriter := traffic.NewIngress(targetState)
	currentStateReader := traffic.NewIngress(currentState)
	err = c.reconcile(ctx, targetStateReadWriter)
	if err != nil {
		return err
	}

	if !equality.Semantic.DeepEqual(currentState, targetState) {
		// our ingress object is now in the correct state, before we commit lets apply any changes via a transform
		if err := targetStateReadWriter.Transform(currentStateReader); err != nil {
			return err
		}
		c.Logger.V(3).Info("attempting update of changed ingress ", "ingress key ", key)
		_, err = c.superClient.WorkspaceClient(logicalcluster.From(targetState)).NetworkingV1().Ingresses(targetState.Namespace).Update(ctx, targetState, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	if targetStateReadWriter.TMCEnabed() {
		if !equality.Semantic.DeepEqual(currentState.Status, targetState.Status) {
			c.Logger.V(3).Info("attempting update of status for ingress ", "ingress key ", key)
			_, err = c.superClient.WorkspaceClient(logicalcluster.From(targetState)).NetworkingV1().Ingresses(targetState.Namespace).UpdateStatus(ctx, targetState, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Controller) getIngressByKey(key string) (*networkingv1.Ingress, error) {
	i, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, k8serrors.NewNotFound(networkingv1.Resource("ingress"), key)
	}
	return i.(*networkingv1.Ingress), nil
}

func (c *Controller) ingressesFromDomainVerification(obj interface{}) ([]*networkingv1.Ingress, error) {
	dv := obj.(*kuadrantv1.DomainVerification)
	domain := strings.ToLower(strings.TrimSpace(dv.Spec.Domain))

	// find all ingresses in this workspace with pending hosts that contain this domains
	ingressList, err := c.ingressLister.Ingresses("").List(labels.SelectorFromSet(labels.Set{
		traffic.LABEL_HAS_PENDING_HOSTS: "true",
	}))
	if err != nil {
		return nil, err
	}

	ingressesToEnqueue := []*networkingv1.Ingress{}

	// TODO this can be removed once advanced scheduling is the norm
	for _, ingress := range ingressList {
		accessor := traffic.NewIngress(ingress)
		if accessor.TMCEnabed() {
			// look at the spec
			for _, rule := range ingress.Spec.Rules {
				if HostMatches(rule.Host, domain) {
					ingressesToEnqueue = append(ingressesToEnqueue, ingress)
				}
			}
			return ingressesToEnqueue, nil
		}
		//TODO(cbrookes) below can be removed once tmc tansformations and advanced scheduling is the default
		pendingRulesAnnotation, ok := ingress.Annotations[traffic.ANNOTATION_PENDING_CUSTOM_HOSTS]
		if !ok {
			continue
		}

		var pendingRules traffic.Pending
		if err := json.Unmarshal([]byte(pendingRulesAnnotation), &pendingRules); err != nil {
			return nil, err
		}

		for _, potentialPending := range pendingRules.Rules {
			if !HostMatches(strings.ToLower(strings.TrimSpace(potentialPending.Host)), domain) {
				continue
			}

			ingressesToEnqueue = append(ingressesToEnqueue, ingress)
		}
	}

	return ingressesToEnqueue, nil
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

func (c *Controller) createOrUpdateIngress(_ context.Context, _ traffic.Interface) error {
	return nil
}

func (c *Controller) deleteRoute(_ context.Context, _ traffic.Interface) error {
	return nil
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
