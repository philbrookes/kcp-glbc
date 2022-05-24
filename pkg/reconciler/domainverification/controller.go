package domainverification

import (
	"context"
	"time"

	"github.com/kcp-dev/apimachinery/pkg/logicalcluster"
	v1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned"
	"github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/informers/externalversions"
	kuadrantv1list "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/listers/kuadrant/v1"

	// Dont really like we import reconciler into a lower reconciler package
	basereconcile "github.com/kuadrant/kcp-glbc/pkg/reconciler"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	controllerName = "kcp-glbc-domain-validation"
	recheckDefault = time.Second * 5
)

// NewController returns a new Controller which reconciles DomainValidation.
func NewController(config *ControllerConfig) (*Controller, error) {
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName)
	c := &Controller{
		Controller:               basereconcile.NewController(controllerName, queue),
		domainVerificationClient: config.DomainVerificationClient,
		sharedInformerFactory:    config.SharedInformerFactory,
		dnsVerifier:              config.DNSVerifier,
	}
	c.Process = c.process

	c.sharedInformerFactory.Kuadrant().V1().DomainVerifications().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.Enqueue(obj) },
		UpdateFunc: func(_, obj interface{}) { c.Enqueue(obj) },
		DeleteFunc: func(obj interface{}) { c.Enqueue(obj) },
	})

	c.indexer = c.sharedInformerFactory.Kuadrant().V1().DomainVerifications().Informer().GetIndexer()
	c.sharedInformerFactory.Kuadrant().V1().DomainVerifications().Lister()
	return c, nil

}

type Controller struct {
	*basereconcile.Controller
	indexer                  cache.Indexer
	domainVerificationLister kuadrantv1list.DomainVerificationLister
	domainVerificationClient kuadrantv1.ClusterInterface
	sharedInformerFactory    externalversions.SharedInformerFactory
	dnsVerifier              dnsVerifier
}

type ControllerConfig struct {
	DomainVerificationClient kuadrantv1.ClusterInterface
	SharedInformerFactory    externalversions.SharedInformerFactory
	DNSVerifier              dnsVerifier
}

func (c *Controller) process(ctx context.Context, key string) error {
	domainVerfication, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		return err
	}

	if !exists {
		c.Logger.Info("DomainVerfication was deleted", "key", key)
		return nil
	}

	current := domainVerfication.(*v1.DomainVerification)
	previous := current.DeepCopy()

	if err = c.reconcile(ctx, current); err != nil {
		return err
	}

	if !equality.Semantic.DeepEqual(previous, current) {
		_, err := c.domainVerificationClient.Cluster(logicalcluster.From(current)).KuadrantV1().DomainVerifications().Update(ctx, current, metav1.UpdateOptions{})
		return err
	}

	return nil
}
