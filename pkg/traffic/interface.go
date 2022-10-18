package traffic

import (
	"context"
	"strings"

	"github.com/kcp-dev/logicalcluster/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	"github.com/kuadrant/kcp-glbc/pkg/dns"
)

type ReconcileStatus int

const (
	ReconcileStatusStop ReconcileStatus = iota
	ReconcileStatusContinue

	ANNOTATION_TRAFFIC_KEY              = "kuadrant.dev/traffic-key"
	ANNOTATION_TRAFFIC_KIND             = "kuadrant.dev/traffic-kind"
	ANNOTATION_CERTIFICATE_STATE        = "kuadrant.dev/certificate-status"
	ANNOTATION_HCG_HOST                 = "kuadrant.dev/host.generated"
	ANNOTATION_HEALTH_CHECK_PREFIX      = "kuadrant.experimental/health-"
	ANNOTATION_HCG_CUSTOM_HOST_REPLACED = "kuadrant.dev/custom-hosts.replaced"
	ANNOTATION_PENDING_CUSTOM_HOSTS     = "kuadrant.dev/pendingCustomHosts"
	LABEL_HAS_PENDING_HOSTS             = "kuadrant.dev/hasPendingCustomHosts"
	FINALIZER_CASCADE_CLEANUP           = "kuadrant.dev/cascade-cleanup"
)

type dnsLookupFunc func(ctx context.Context, host string) ([]dns.HostAddress, error)
type CreateOrUpdateTraffic func(ctx context.Context, i Interface) error
type DeleteTraffic func(ctx context.Context, i Interface) error

type Interface interface {
	runtime.Object
	metav1.Object
	GetKind() string
	GetHosts() []string
	GetTargets(ctx context.Context, dnsLookup dnsLookupFunc) (map[logicalcluster.Name]map[string]dns.Target, error)
	GetLogicalCluster() logicalcluster.Name
	GetNamespaceName() types.NamespacedName
	AddTLS(host string, secret *corev1.Secret)
	RemoveTLS(host []string)
	ReplaceCustomHosts(managedHost string) []string
	ProcessCustomHosts(context.Context, *v1.DomainVerificationList, CreateOrUpdateTraffic, DeleteTraffic) error
}

type Pending struct {
	Rules []networkingv1.IngressRule `json:"rules"`
}

// IsDomainVerified will take the host and recursively remove subdomains searching for a matching domainverification
// that is verified. Until either a match is found, or the subdomains run out.
func IsDomainVerified(host string, dvs []v1.DomainVerification) bool {
	for _, dv := range dvs {
		if dv.Spec.Domain == host && dv.Status.Verified {
			return true
		}
	}
	parentHostParts := strings.SplitN(host, ".", 2)
	//we've run out of sub-domains
	if len(parentHostParts) < 2 {
		return false
	}

	//recurse up the subdomains
	return IsDomainVerified(parentHostParts[1], dvs)
}
