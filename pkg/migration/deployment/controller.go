package deployment

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/go-logr/logr"
	"github.com/kcp-dev/logicalcluster/v2"

	"github.com/kuadrant/kcp-glbc/pkg/migration/workload"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler"
	"github.com/kuadrant/kcp-glbc/pkg/superClient"
)

const defaultControllerName = "kcp-glbc-deployment"

// NewController returns a new Controller which reconciles Deployment.
func NewController(config *ControllerConfig) (*Controller, error) {
	controllerName := config.GetName(defaultControllerName)
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName)
	c := &Controller{
		Controller:            reconciler.NewController(controllerName, queue),
		superClient:           config.SuperClient,
		sharedInformerFactory: config.SharedInformerFactory,
	}
	c.Process = c.process
	c.migrationHandler = workload.Migrate

	c.sharedInformerFactory.Apps().V1().Deployments().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.Enqueue(obj) },
		UpdateFunc: func(_, obj interface{}) { c.Enqueue(obj) },
		DeleteFunc: func(obj interface{}) { c.Enqueue(obj) },
	})

	c.indexer = c.sharedInformerFactory.Apps().V1().Deployments().Informer().GetIndexer()
	c.deploymentLister = c.sharedInformerFactory.Apps().V1().Deployments().Lister()
	c.serviceLister = c.sharedInformerFactory.Core().V1().Services().Lister()

	return c, nil
}

type ControllerConfig struct {
	*reconciler.ControllerConfig
	SuperClient           superClient.Interface
	SharedInformerFactory informers.SharedInformerFactory
}

type Controller struct {
	*reconciler.Controller
	sharedInformerFactory informers.SharedInformerFactory
	superClient           superClient.Interface
	indexer               cache.Indexer
	deploymentLister      appsv1listers.DeploymentLister
	serviceLister         corev1listers.ServiceLister
	migrationHandler      func(obj metav1.Object, queue workqueue.RateLimitingInterface, logger logr.Logger)
}

func (c *Controller) process(ctx context.Context, key string) error {
	object, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		return err
	}

	if !exists {
		c.Logger.Info("Deployment was deleted", "key", key)
		return nil
	}

	current := object.(*appsv1.Deployment)
	target := current.DeepCopy()

	if err = c.reconcile(ctx, target); err != nil {
		return err
	}

	// If the object being reconciled changed as a result, update it.
	if !equality.Semantic.DeepEqual(target, current) {
		_, err := c.superClient.WorkspaceClient(logicalcluster.From(target)).AppsV1().Deployments(target.Namespace).Update(ctx, target, metav1.UpdateOptions{})
		return err
	}

	return nil
}
