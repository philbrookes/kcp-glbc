package ingress

import (
	"context"
	"strconv"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilserrors "k8s.io/apimachinery/pkg/util/errors"
	apiRuntime "k8s.io/apimachinery/pkg/util/runtime"

	"k8s.io/client-go/tools/cache"

	"github.com/kuadrant/kcp-glbc/pkg/_internal/metadata"
	"github.com/kuadrant/kcp-glbc/pkg/migration/workload"
	"github.com/kuadrant/kcp-glbc/pkg/traffic"
)

func (c *Controller) reconcile(ctx context.Context, ingress traffic.Interface) error {
	if ingress.GetDeletionTimestamp() == nil {
		metadata.AddFinalizer(ingress, traffic.FINALIZER_CASCADE_CLEANUP)
	}
	workload.Migrate(ingress, c.Queue, c.Logger)

	reconcilers := []traffic.Reconciler{
		// DnsReconciler is first as it will set generatedHost field on the traffic object based on the DNSRecord it creates for each ingress
		&traffic.DnsReconciler{
			DeleteDNS:        c.DeleteDNS,
			GetDNS:           c.GetDNS,
			CreateDNS:        c.CreateDNS,
			UpdateDNS:        c.UpdateDNS,
			WatchHost:        c.hostsWatcher.StartWatching,
			ForgetHost:       c.hostsWatcher.StopWatching,
			ListHostWatchers: c.hostsWatcher.ListHostRecordWatchers,
			ManagedDomain:    c.domain,
			Log:              c.Logger,
			DNSLookup:        c.hostResolver.LookupIPAddr,
		},
		&traffic.HostReconciler{
			Log:                    c.Logger,
			GetDomainVerifications: c.GetDomainVerifications,
			CreateOrUpdateTraffic:  c.createOrUpdateIngress,
			DeleteTraffic:          c.deleteRoute,
		},
		&traffic.CertificateReconciler{
			CreateCertificate:    c.certProvider.Create,
			DeleteCertificate:    c.certProvider.Delete,
			GetCertificateSecret: c.certProvider.GetCertificateSecret,
			UpdateCertificate:    c.certProvider.Update,
			GetCertificateStatus: c.certProvider.GetCertificateStatus,
			CopySecret:           c.CopySecret,
			GetSecret:            c.GetSecret,
			DeleteSecret:         c.DeleteTLSSecret,
			Log:                  c.Logger,
		},
	}
	var errs []error
	for _, r := range reconcilers {
		status, err := r.Reconcile(ctx, ingress)
		if err != nil {
			c.Logger.Error(err, "reconciler error: ", "ingress", ingress, "reconciler", r.GetName())
			errs = append(errs, err)
		}
		if status == traffic.ReconcileStatusStop {
			break
		}
	}

	if len(errs) == 0 {
		if ingress.GetDeletionTimestamp() != nil && !ingress.GetDeletionTimestamp().IsZero() {
			c.Logger.Info("reconcile ingress deleted ", "ingress", ingress)
			metadata.RemoveFinalizer(ingress, traffic.FINALIZER_CASCADE_CLEANUP)
			c.hostsWatcher.StopWatching(objectKey(ingress), "")
			//in 0.5.0 these are never cleaned up properly
			for _, f := range ingress.GetFinalizers() {
				if strings.Contains(f, workload.SyncerFinalizer) {
					metadata.RemoveFinalizer(ingress, f)
				}
			}
		}
	}

	c.Logger.V(3).Info("ingress reconcile complete", "errors", strconv.Itoa(len(errs)), "ingress", ingress)
	return utilserrors.NewAggregate(errs)
}

func objectKey(obj runtime.Object) cache.ExplicitKey {
	key, _ := cache.MetaNamespaceKeyFunc(obj)
	return cache.ExplicitKey(key)
}

// enqueueIngresses creates an event handler function given a function that
// returns a list of ingresses to enqueue, or an error. If an error is returned,
// no ingresses are enqueued.
func (c *Controller) enqueueIngresses(getIngresses func(obj interface{}) ([]*networkingv1.Ingress, error)) func(obj interface{}) {
	return func(obj interface{}) {
		ingresses, err := getIngresses(obj)
		if err != nil {
			apiRuntime.HandleError(err)
			return
		}

		for _, ingress := range ingresses {
			ingressKey, err := cache.MetaNamespaceKeyFunc(ingress)
			if err != nil {
				apiRuntime.HandleError(err)
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
