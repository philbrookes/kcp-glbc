package certificate

import (
	"context"
	v1 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	certmanclient "github.com/jetstack/cert-manager/pkg/client/clientset/versioned/typed/certmanager/v1"
	"github.com/jetstack/cert-manager/pkg/client/informers/externalversions"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler"
	"github.com/kuadrant/kcp-glbc/pkg/tls"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const controllerName = "kcp-glbc-certificate"

// NewController returns a new Controller which reconciles Deployment.
func NewController(config *ControllerConfig) (*Controller, error) {
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName)

	c := &Controller{
		Controller:             reconciler.NewController(controllerName, queue),
		coreClient:             config.CoreClient,
		certClient:             config.CertClient,
		certManager:            config.CertManager,
		certInformerFactory:    config.CertInformer,
		CertificatePendingTime: prometheus.NewHistogram(prometheus.HistogramOpts{}),
	}
	c.Process = c.process

	// Watch for events related to Certificates
	c.certInformerFactory.Certmanager().V1().Certificates().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.Enqueue(obj)
		},
		UpdateFunc: func(_, obj interface{}) { c.Enqueue(obj) },
		DeleteFunc: func(obj interface{}) { c.Enqueue(obj) },
	})

	c.indexer = c.certInformerFactory.Certmanager().V1().Certificates().Informer().GetIndexer()

	return c, nil
}

type ControllerConfig struct {
	CoreClient                kubernetes.ClusterInterface
	CertClient                certmanclient.CertmanagerV1Interface
	CertManager               tls.Provider
	CertInformer              externalversions.SharedInformerFactory
	GLBCSharedInformerFactory dynamicinformer.DynamicSharedInformerFactory
}

type Controller struct {
	*reconciler.Controller
	glbcSharedInformerFactory dynamicinformer.DynamicSharedInformerFactory
	coreClient                kubernetes.ClusterInterface
	certClient                certmanclient.CertmanagerV1Interface
	certManager               tls.Provider
	indexer                   cache.Indexer
	certInformerFactory       externalversions.SharedInformerFactory
	CertificatePendingTime    prometheus.Histogram
}

func (c *Controller) process(ctx context.Context, key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		return err
	}

	if !exists {
		klog.Infof("Object with key %q was deleted", key)
		return nil
	}

	current := obj.(*v1.Certificate)

	previous := current.DeepCopy()

	if err := c.reconcile(ctx, current); err != nil {
		return err
	}

	// If the object being reconciled changed as a result, update it.
	if !equality.Semantic.DeepEqual(previous, current) {
		_, err := c.certClient.Certificates(current.Namespace).Update(ctx, current, metav1.UpdateOptions{})
		return err
	}

	return err
}
