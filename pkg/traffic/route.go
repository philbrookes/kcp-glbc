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

	"github.com/kuadrant/kcp-glbc/pkg/_internal/log"
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
	generatedHost string
}

func (a *Route) GetKind() string {
	return "Route"
}

func (a *Route) GetHosts() []string {
	return []string{
		a.Route.Spec.Host,
	}
}

func (a *Route) GetSpec() interface{} {
	return a.Spec
}

func (a *Route) TMCEnabed() bool {
	// check the annotations for status
	if tmcEnabled(a) {
		return true
	}
	enabled := true
	//once the status gets set to something other than the glbc provided host we are sure it is not advanced scheduling
	if len(a.Status.Ingress) == 1 {
		if a.Status.Ingress[0].Host != "" {
			host := a.GetHCGHost()
			enabled = a.Status.Ingress[0].Host == host
		}
	}
	return enabled
}

func (a *Route) GetSyncTargets() []string {
	return getSyncTargets(a)
}

func (a *Route) SetDNSLBHost(lbHost string) {
	a.Status.Ingress = []routev1.RouteIngress{
		{
			Host: lbHost,
		},
	}
}

func (a *Route) SetHCGHost(s string) {
	a.generatedHost = s
}

func (a *Route) Transform(previous Interface) error {
	hostPatch := patch{
		OP:    "replace",
		Path:  "/host",
		Value: a.Spec.Host,
	}
	tlsPatch := patch{
		OP:    "replace",
		Path:  "/tls",
		Value: a.Spec.TLS,
	}
	patches := []patch{hostPatch, tlsPatch}
	if err := applyTransformPatches(patches, a); err != nil {
		return err
	}
	// ensure we don't modify the actual spec (TODO TMC once transforms are default remove this check)
	if a.TMCEnabed() {
		oldSpec, ok := previous.GetSpec().(routev1.RouteSpec)
		if !ok {
			return fmt.Errorf("expected the spec to be an RouteSpec %v", previous.GetSpec())
		}
		a.Spec = oldSpec
	}
	return nil
}

func (a *Route) GetHCGHost() string {
	return a.generatedHost
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
		if a.Route.Spec.Host != host {
			//and if so, remove it
			a.Route.Spec.TLS = &routev1.TLSConfig{}
		}
	}
}

func (a *Route) GetDNSTargets() ([]dns.Target, error) {
	dnsTargets := []dns.Target{}
	statuses, err := a.getStatuses()
	if err != nil {
		return dnsTargets, err
	}
	for cluster, status := range statuses {
		for _, ingress := range status.Ingress {
			host := ""
			// with a Route it is always a host
			if ingress.RouterCanonicalHostname != "" {
				host = ingress.RouterCanonicalHostname
			} else if ingress.Host != "" {
				host = ingress.Host
			} else {
				return nil, fmt.Errorf("no usable host value on route (%v) status", a.Name)
			}
			target := dns.Target{Value: host, TargetType: dns.TargetTypeHost, Cluster: cluster.String()}
			dnsTargets = append(dnsTargets, target)
		}
	}
	return dnsTargets, nil
}

func (a *Route) ProcessCustomHosts(ctx context.Context, dvs *v1.DomainVerificationList, createOrUpdate CreateOrUpdateTraffic, delete DeleteTraffic) error {
	generatedHost := a.GetHCGHost()
	if generatedHost == "" && a.DeletionTimestamp == nil {
		return ErrGeneratedHostMissing
	}
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
		log.Logger.Info("updating finalizers on route due to shadow creation")
		metadata.AddFinalizer(a, SHADOW_FINALIZER)
		//  - remove pending hosts label and annotation
		metadata.RemoveLabel(a, LABEL_HAS_PENDING_HOSTS)
		metadata.RemoveAnnotation(a, ANNOTATION_PENDING_CUSTOM_HOSTS)
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
