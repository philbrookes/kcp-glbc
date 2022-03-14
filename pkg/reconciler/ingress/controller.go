package ingress

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	networkingv1lister "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned"
	"github.com/kuadrant/kcp-glbc/pkg/net"
	"github.com/kuadrant/kcp-glbc/pkg/tls"
)

const controllerName = "kcp-glbc-ingress"

// NewController returns a new Controller which splits new Ingress objects
// into N virtual Ingresses labeled for each Cluster that exists at the time
// the Ingress is created.
func NewController(config *ControllerConfig) *Controller {
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	hostResolver := config.HostResolver
	switch impl := hostResolver.(type) {
	case *net.ConfigMapHostResolver:
		impl.Client = config.KubeClient.Cluster("admin")
	}
	hostResolver = net.NewSafeHostResolver(hostResolver)

	c := &Controller{
		queue:                 queue,
		kubeClient:            config.KubeClient,
		certProvider:          config.CertProvider,
		sharedInformerFactory: config.SharedInformerFactory,
		dnsRecordClient:       config.DnsRecordClient,
		domain:                config.Domain,
		tracker:               newTracker(),
		tlsEnabled:            config.TLSEnabled,
		hostResolver:          hostResolver,
		hostsWatcher: net.NewHostsWatcher(
			hostResolver,
			net.DefaultInterval,
		),
		customHostsEnabled: config.CustomHostsEnabled,
	}
	c.hostsWatcher.OnChange = c.synchronisedEnque()

	// Watch for events related to Ingresses
	c.sharedInformerFactory.Networking().V1().Ingresses().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueue(obj) },
		UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
		DeleteFunc: func(obj interface{}) { c.enqueue(obj) },
	})

	// Watch for events related to Services
	c.sharedInformerFactory.Core().V1().Services().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, obj interface{}) { c.ingressesFromService(obj) },
		DeleteFunc: func(obj interface{}) { c.ingressesFromService(obj) },
	})

	c.indexer = c.sharedInformerFactory.Networking().V1().Ingresses().Informer().GetIndexer()
	c.lister = c.sharedInformerFactory.Networking().V1().Ingresses().Lister()

	return c
}

type ControllerConfig struct {
	KubeClient            kubernetes.ClusterInterface
	DnsRecordClient       kuadrantv1.ClusterInterface
	SharedInformerFactory informers.SharedInformerFactory
	Domain                *string
	TLSEnabled            bool
	CertProvider          tls.Provider
	HostResolver          net.HostResolver
	CustomHostsEnabled    *bool
}

type Controller struct {
	queue                 workqueue.RateLimitingInterface
	kubeClient            kubernetes.ClusterInterface
	sharedInformerFactory informers.SharedInformerFactory
	dnsRecordClient       kuadrantv1.ClusterInterface
	indexer               cache.Indexer
	lister                networkingv1lister.IngressLister
	certProvider          tls.Provider
	domain                *string
	tlsEnabled            bool
	tracker               tracker
	hostResolver          net.HostResolver
	hostsWatcher          *net.HostsWatcher
	customHostsEnabled    *bool
}

func (c *Controller) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	c.queue.AddRateLimited(key)
}

func (c *Controller) Start(ctx context.Context, numThreads int) {
	defer runtime.HandleCrash()
	defer c.queue.ShutDown()

	klog.InfoS("Starting workers", "controller", controllerName)
	defer klog.InfoS("Stopping workers", "controller", controllerName)

	for i := 0; i < numThreads; i++ {
		go wait.UntilWithContext(ctx, c.startWorker, time.Second)
	}

	<-ctx.Done()
}

func (c *Controller) startWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	// Wait until there is a new item in the working queue
	k, quit := c.queue.Get()
	if quit {
		return false
	}
	key := k.(string)

	// No matter what, tell the queue we're done with this key, to unblock
	// other workers.
	defer c.queue.Done(key)

	err := c.process(ctx, key)
	c.handleErr(err, key)
	return true
}

func (c *Controller) handleErr(err error, key string) {
	// Reconcile worked, nothing else to do for this workqueue item.
	if err == nil {
		c.queue.Forget(key)
		return
	}

	// Re-enqueue up to 5 times.
	num := c.queue.NumRequeues(key)
	if num < 5 {
		klog.Errorf("Error reconciling key %q, retrying... (#%d): %v", key, num, err)
		c.queue.AddRateLimited(key)
		return
	}

	// Give up and report error elsewhere.
	c.queue.Forget(key)
	runtime.HandleError(err)
	klog.Infof("Dropping key %q after failed retries: %v", key, err)
}

func (c *Controller) process(ctx context.Context, key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		return err
	}

	if !exists {
		klog.Infof("Object with key %q was deleted", key)
		// The ingress has been deleted, so we remove any ingress to service tracking.
		c.tracker.deleteIngress(key)
		return nil
	}
	current := obj.(*networkingv1.Ingress)

	previous := current.DeepCopy()

	rootName, isLeaf := getRootName(current)
	if isLeaf {
		err = c.reconcileLeaf(ctx, rootName, current)
	} else {
		err = c.reconcileRoot(ctx, current)
	}
	if err != nil {
		return err
	}

	// If the object being reconciled changed as a result, update it.
	if !equality.Semantic.DeepEqual(previous, current) {
		_, err := c.kubeClient.Cluster(current.ClusterName).NetworkingV1().Ingresses(current.Namespace).Update(ctx, current, metav1.UpdateOptions{})
		return err
	}

	return err
}

// ingressesFromService enqueues all the related ingresses for a given service.
func (c *Controller) ingressesFromService(obj interface{}) {
	service := obj.(*corev1.Service)

	serviceKey, err := cache.MetaNamespaceKeyFunc(service)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	// Does that Service has any Ingress associated to?
	ingresses := c.tracker.getIngressesForService(serviceKey)

	// One Service can be referenced by 0..n Ingresses, so we need to enqueue all the related ingreses.
	for _, ingress := range ingresses.List() {
		klog.Infof("tracked service %q triggered Ingress %q reconciliation", service.Name, ingress)
		c.queue.Add(ingress)
	}
}

// synchronisedEnque returns a function to be passed to the host watcher that
// enqueues the affected object to be reconciled by c, in a synchronized fashion
func (c *Controller) synchronisedEnque() func(obj interface{}) {
	var mu sync.Mutex
	return func(obj interface{}) {
		mu.Lock()
		defer mu.Unlock()
		c.enqueue(obj)
	}
}
