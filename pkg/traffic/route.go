package traffic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kcp-dev/logicalcluster/v2"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workload "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"

	"github.com/kuadrant/kcp-glbc/pkg/_internal/metadata"
	v1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	"github.com/kuadrant/kcp-glbc/pkg/dns"
)

const (
	SHADOW_FINALIZER          = "kuadrant.dev/shadow_cleanup"
	ANNOTATION_IS_GLBC_SHADOW = "kuadrant.dev/is_shadow_route"
)

func NewRoute(r *routev1.Route) *Route {
	return &Route{Route: r}
}

type Route struct {
	*routev1.Route
}

func (a *Route) GetKind() string {
	return "Route"
}

func (a *Route) GetHosts() []string {
	return []string{
		a.Route.Spec.Host,
	}
}

func (a *Route) AddTLS(host string, secret *corev1.Secret) {
	if a.Route.Spec.TLS == nil {
		a.Route.Spec.TLS = &routev1.TLSConfig{}
	}
	if a.Route.Spec.Host == host {
		a.Route.Spec.TLS.Key = string(secret.Data[corev1.TLSPrivateKeyKey])
		a.Route.Spec.TLS.Certificate = string(secret.Data[corev1.TLSCertKey])
		a.Route.Spec.TLS.CACertificate = string(secret.Data[corev1.ServiceAccountRootCAKey])
	}
}

func (a *Route) RemoveTLS(hosts []string) {
	//check the passed in hosts contains the host this is ingress is for
	for _, host := range hosts {
		if a.Route.Spec.Host == host {
			//and if so, remove it
			a.Route.Spec.TLS = &routev1.TLSConfig{}
		}
	}
}

func (a *Route) ReplaceCustomHosts(managedHost string) []string {
	if a.Route.Spec.Host != managedHost {
		replaced := a.Route.Spec.Host
		a.Route.Spec.Host = managedHost

		a.RemoveTLS([]string{replaced})
		return []string{replaced}
	}
	return []string{}
}

func (a *Route) GetTargets(ctx context.Context, dnsLookup dnsLookupFunc) (map[logicalcluster.Name]map[string]dns.Target, error) {
	targets := map[logicalcluster.Name]map[string]dns.Target{}
	statuses, err := a.getStatuses()
	if err != nil {
		return targets, err
	}
	for cluster, status := range statuses {
		clusterTargets := map[string]dns.Target{}
		for _, ingress := range status.Ingress {
			host := ingress.RouterCanonicalHostname
			ips, err := dnsLookup(ctx, host)
			//couldn't find any IPs, just use the host
			if err != nil {
				ips = []dns.HostAddress{}
			}
			clusterTargets[host] = dns.Target{Value: []string{}, TargetType: dns.TargetTypeHost}
			for _, ip := range ips {
				t := clusterTargets[host]
				t.Value = append(clusterTargets[host].Value, ip.IP.String())
				clusterTargets[host] = t
			}
		}
		targets[cluster] = clusterTargets
	}

	return targets, nil
}

func (a *Route) ProcessCustomHosts(ctx context.Context, dvs *v1.DomainVerificationList, createOrUpdate CreateOrUpdateTraffic, delete DeleteTraffic) error {
	generatedHost := metadata.GetAnnotation(a.Route, ANNOTATION_HCG_HOST)

	//don't process custom hosts for shadows
	if metadata.HasAnnotation(a.Route, ANNOTATION_IS_GLBC_SHADOW) {
		//reset the host to our generated host
		a.Spec.Host = generatedHost
		return nil
	}

	if a.Route.GetDeletionTimestamp() != nil {
		shadow := a.Route.DeepCopy()
		shadow.Name = a.GetName() + "-shadow"
		shadowAccessor := NewRoute(shadow)
		err := delete(ctx, shadowAccessor)
		if err != nil {
			return fmt.Errorf("error deleting shadow: %v", err)
		}
		metadata.RemoveFinalizer(a.Route, SHADOW_FINALIZER)
	}

	//reset object before processing
	if a.Route.Spec.Host == generatedHost && metadata.GetAnnotation(a.Route, ANNOTATION_PENDING_CUSTOM_HOSTS) != "" {
		a.Route.Spec.Host = metadata.GetAnnotation(a.Route, ANNOTATION_PENDING_CUSTOM_HOSTS)
	}
	//is custom host verified now?
	verified := IsDomainVerified(a.Route.Spec.Host, dvs.Items) || a.Spec.Host == ""

	if !verified {
		//not verified
		//	- set custom host pending
		metadata.AddAnnotation(a.Route, ANNOTATION_PENDING_CUSTOM_HOSTS, a.Route.Spec.Host)
		metadata.AddLabel(a.Route, LABEL_HAS_PENDING_HOSTS, "true")

		//	- replace with generated host
		a.Route.Spec.Host = generatedHost
	} else {
		//yes
		//	- reconcile shadow route for generated host
		shadow := a.Route.DeepCopy()
		// prepopulate generatedhost in spec and annotation on the shadow
		shadow.Spec.Host = generatedHost
		shadow.Name = a.GetName() + "-shadow"
		metadata.AddAnnotation(shadow, ANNOTATION_HCG_HOST, generatedHost)
		metadata.AddAnnotation(shadow, ANNOTATION_IS_GLBC_SHADOW, "true")
		metadata.RemoveAnnotation(shadow, ANNOTATION_PENDING_CUSTOM_HOSTS)
		t := true
		shadow.OwnerReferences = append(shadow.OwnerReferences, metav1.OwnerReference{
			APIVersion:         a.APIVersion,
			Kind:               a.Kind,
			Name:               a.GetName(),
			UID:                a.GetUID(),
			BlockOwnerDeletion: &t,
		})
		shadowAccessor := NewRoute(shadow)
		err := createOrUpdate(ctx, shadowAccessor)
		if err != nil {
			return fmt.Errorf("error creating or updating shadow: %v", err)
		}
		metadata.AddFinalizer(a.Route, SHADOW_FINALIZER)
		//  - remove pending hosts label and annotation
		metadata.RemoveLabel(a.Route, LABEL_HAS_PENDING_HOSTS)
		metadata.RemoveAnnotation(a.Route, ANNOTATION_PENDING_CUSTOM_HOSTS)
	}
	return nil
}

func (a *Route) String() string {
	return fmt.Sprintf("logical cluster: %v, kind: %v, namespace/name: %v", a.GetLogicalCluster(), a.GetKind(), a.GetNamespaceName())
}

func (a *Route) GetLogicalCluster() logicalcluster.Name {
	return logicalcluster.From(a)
}

func (a *Route) GetNamespaceName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: a.Namespace,
		Name:      a.Name,
	}
}

func (a *Route) getStatuses() (map[logicalcluster.Name]routev1.RouteStatus, error) {
	statuses := map[logicalcluster.Name]routev1.RouteStatus{}
	for k, v := range a.Annotations {
		status := routev1.RouteStatus{}
		if !strings.Contains(k, workload.InternalClusterStatusAnnotationPrefix) {
			continue
		}
		annotationParts := strings.Split(k, "/")
		if len(annotationParts) < 2 {
			return nil, fmt.Errorf("advanced scheduling annotation malformed %s value %s", workload.InternalClusterStatusAnnotationPrefix, a.Annotations[k])
		}
		clusterName := annotationParts[1]
		err := json.Unmarshal([]byte(v), &status)
		if err != nil {
			return statuses, err
		}
		statuses[logicalcluster.New(clusterName)] = status
	}

	cluster := logicalcluster.From(a)
	statuses[cluster] = a.Status
	return statuses, nil
}
