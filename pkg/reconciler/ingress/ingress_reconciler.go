package ingress

import (
	"context"
	"k8s.io/apimachinery/pkg/util/runtime"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/kuadrant/kcp-glbc/pkg/util/metadata"
	"github.com/kuadrant/kcp-glbc/pkg/util/workloadMigration"
	utilserrors "k8s.io/apimachinery/pkg/util/errors"
)

type reconcileStatus int

const (
	reconcileStatusStop reconcileStatus = iota
	reconcileStatusContinue
	cascadeCleanupFinalizer  = "kcp.dev/cascade-cleanup"
	GeneratedRulesAnnotation = "kuadrant.dev/custom-hosts.generated"
)

type reconciler interface {
	reconcile(ctx context.Context, ingress *networkingv1.Ingress) (reconcileStatus, error)
}

func (c *Controller) reconcile(ctx context.Context, ingress *networkingv1.Ingress) error {
	c.Logger.V(3).Info("starting reconcile of ingress ", ingress.Name, ingress.Namespace)
	if ingress.DeletionTimestamp == nil {
		metadata.AddFinalizer(ingress, cascadeCleanupFinalizer)
	}
	//TODO evaluate where this actually belongs
	workloadMigration.Process(ingress, c.Queue, c.Logger)

	reconcilers := []reconciler{
		//hostReconciler is first as the others depends on it for the host to be set on the ingress
		&hostReconciler{
			managedDomain:      c.domain,
			log:                c.Logger,
			customHostsEnabled: c.customHostsEnabled,
			kuadrantClient:     c.kuadrantClient,
		},
		&certificateReconciler{
			createCertificate:    c.certProvider.Create,
			deleteCertificate:    c.certProvider.Delete,
			getCertificateSecret: c.certProvider.GetCertificateSecret,
			updateCertificate:    c.certProvider.Update,
			getCertificateStatus: c.certProvider.GetCertificateStatus,
			copySecret:           c.copySecret,
			deleteSecret:         c.deleteTLSSecret,
			log:                  c.Logger,
		},
		&dnsReconciler{
			deleteDNS:        c.deleteDNS,
			DNSLookup:        c.hostResolver.LookupIPAddr,
			getDNS:           c.getDNS,
			createDNS:        c.createDNS,
			updateDNS:        c.updateDNS,
			watchHost:        c.hostsWatcher.StartWatching,
			forgetHost:       c.hostsWatcher.StopWatching,
			listHostWatchers: c.hostsWatcher.ListHostRecordWatchers,
			log:              c.Logger,
		},
	}
	var errs []error

	for _, r := range reconcilers {
		status, err := r.reconcile(ctx, ingress)
		if err != nil {
			errs = append(errs, err)
		}
		if status == reconcileStatusStop {
			break
		}
	}

	if len(errs) == 0 {
		if ingress.DeletionTimestamp != nil && !ingress.DeletionTimestamp.IsZero() {
			metadata.RemoveFinalizer(ingress, cascadeCleanupFinalizer)
			c.hostsWatcher.StopWatching(ingressKey(ingress), "")
			//in 0.5.0 these are never cleaned up properly
			for _, f := range ingress.Finalizers {
				if strings.Contains(f, workloadMigration.SyncerFinalizer) {
					metadata.RemoveFinalizer(ingress, f)
				}
			}
		}
	}
	c.Logger.V(3).Info("ingress reconcile complete", len(errs), ingress.Namespace, ingress.Name)
	return utilserrors.NewAggregate(errs)
}

func ingressKey(ingress *networkingv1.Ingress) interface{} {
	key, _ := cache.MetaNamespaceKeyFunc(ingress)
	return cache.ExplicitKey(key)
}

// enqueueIngresses creates an event handler function given a function that
// returns a list of ingresses to enqueue, or an error. If an error is returned,
// no ingresses are enqueued.
//
// This allows to easierly unit test the logic of the function that returns
// the ingresses to enqueue
func (c *Controller) enqueueIngresses(getIngresses func(obj interface{}) ([]*networkingv1.Ingress, error)) func(obj interface{}) {
	return func(obj interface{}) {
		ingresses, err := getIngresses(obj)
		if err != nil {
			runtime.HandleError(err)
			return
		}

		for _, ingress := range ingresses {
			ingressKey, err := cache.MetaNamespaceKeyFunc(ingress)
			if err != nil {
				runtime.HandleError(err)
				continue
			}

			c.Queue.Add(ingressKey)
		}
	}
}

func (c *Controller) enqueueIngressesFromUpdate(getIngresses func(obj interface{}) ([]*networkingv1.Ingress, error)) func(oldObj, newObj interface{}) {
	return func(oldObj, newObj interface{}) {
		c.enqueueIngresses(getIngresses)(newObj)
	}
}
