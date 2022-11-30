package traffic

import (
	"context"
	"encoding/json"
	"fmt"
	routev1 "github.com/openshift/api/route/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workload "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"
	"github.com/kuadrant/kcp-glbc/pkg/_internal/log"
	"github.com/kuadrant/kcp-glbc/pkg/_internal/slice"
	v1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	"github.com/kuadrant/kcp-glbc/pkg/dns"
)

type validatedDNSClient struct {
	createCalled int
	updateCalled int
	getCalled    int
}

func (ve *validatedDNSClient) create(validate func(dns *v1.DNSRecord) error) func(ctx context.Context, dns *v1.DNSRecord) (*v1.DNSRecord, error) {
	return func(ctx context.Context, dns *v1.DNSRecord) (*v1.DNSRecord, error) {
		ve.createCalled++
		err := validate(dns)
		return dns, err
	}

}

func (ve *validatedDNSClient) update(validate func(dns *v1.DNSRecord) error) func(ctx context.Context, dns *v1.DNSRecord) (*v1.DNSRecord, error) {
	return func(ctx context.Context, dns *v1.DNSRecord) (*v1.DNSRecord, error) {
		ve.updateCalled++
		err := validate(dns)
		return dns, err
	}

}

func (ve *validatedDNSClient) get(getfunc func(ctx context.Context, accessor Interface) (*v1.DNSRecord, error)) func(ctx context.Context, accessor Interface) (*v1.DNSRecord, error) {
	ve.getCalled++
	return getfunc
}

func TestDNSReconciler(t *testing.T) {
	managedHost := "test.cb.example.com"

	// sets up 2 ingresses one with status in the advanced scheduling annotation and one in the regular status block
	setupAccessors := func(ingressStatus networkingv1.IngressStatus) []Interface {
		var accessors []Interface
		for i := 0; i <= 1; i++ {
			ing := &networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("ingress-%d", i),
					Annotations: map[string]string{
						"kcp.dev/cluster": "somecluster",
					},
				},
			}
			rule := networkingv1.IngressRule{
				Host: managedHost,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{},
				},
			}
			ing.Spec.Rules = append(ing.Spec.Rules, rule)
			if i == 0 {
				// add to annotation
				status, _ := json.Marshal(ingressStatus)
				ing.Annotations[workload.InternalClusterStatusAnnotationPrefix+"/somecluster"] = string(status)
			} else {
				ing.Status = ingressStatus
			}
			accessor := NewIngress(ing)
			accessor.SetHCGHost(managedHost)
			accessors = append(accessors, accessor)
		}

		return accessors
	}

	fakewatcher := func(k interface{}) []dns.RecordWatcher {
		return []dns.RecordWatcher{}
	}

	commonDNSValidate := func(expectedIPs []string) func(dns *v1.DNSRecord) error {
		return func(dns *v1.DNSRecord) error {
			if dns == nil {
				return fmt.Errorf("did not expect a nil dns record")
			}
			if len(expectedIPs) != len(dns.Spec.Endpoints) {
				return fmt.Errorf("expected %d endpoints but got %d ", len(expectedIPs), len(dns.Spec.Endpoints))
			}
			for _, ep := range dns.Spec.Endpoints {
				if len(ep.Targets) != len(expectedIPs) {
					return fmt.Errorf("expected only 1 dns Target but got %d", len(ep.Targets))
				}
				if ep.RecordType != "A" {
					return fmt.Errorf("expected an A record but got %s", ep.RecordType)
				}
				for _, ip := range expectedIPs {
					if !slice.ContainsString(ep.Targets, ip) {
						return fmt.Errorf("ip %s not in targets ", ip)
					}
				}
			}
			return nil
		}
	}

	cases := []struct {
		Name           string
		getDNS         func(ctx context.Context, accessor Interface) (*v1.DNSRecord, error)
		validateResult func(status ReconcileStatus, dnsClient *validatedDNSClient, err error) error
		ingressStatus  networkingv1.IngressStatus
		expectedIPs    []string
		DNSLookup      func(ctx context.Context, host string) ([]dns.HostAddress, error)
	}{
		{
			Name: "test DNSRecord is created when it doesn't exist with no endpoints",
			getDNS: func(ctx context.Context, accessor Interface) (*v1.DNSRecord, error) {
				return nil, errors.NewNotFound(v1.Resource("dnsrecord"), accessor.GetName())
			},
			ingressStatus: networkingv1.IngressStatus{
				LoadBalancer: corev1.LoadBalancerStatus{
					Ingress: []corev1.LoadBalancerIngress{{
						IP: "192.168.33.2",
					},
					},
				},
			},
			DNSLookup: func(ctx context.Context, host string) ([]dns.HostAddress, error) {
				return nil, fmt.Errorf("DNSLookup should not have been called")
			},
			expectedIPs: []string{},
			validateResult: func(status ReconcileStatus, dnsClient *validatedDNSClient, err error) error {
				if status != ReconcileStatusContinue || err != nil {
					return fmt.Errorf("expected Reconcile status to be %v got %v. Expected err to be nil got %v", ReconcileStatusContinue, status, err)
				}
				if dnsClient.createCalled != 1 {
					return fmt.Errorf("expected create dns to be called 1 time but was called %d", dnsClient.createCalled)
				}
				if dnsClient.updateCalled != 0 {
					return fmt.Errorf("expected update dns to be called 0 times but was called %d", dnsClient.updateCalled)
				}
				return nil
			},
		},
		{
			Name: "test DNSRecord is created with correct values when it does exist",
			getDNS: func(ctx context.Context, accessor Interface) (*v1.DNSRecord, error) {
				return &v1.DNSRecord{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							ANNOTATION_HCG_HOST: managedHost},
					},
					Spec: v1.DNSRecordSpec{
						Endpoints: []*v1.Endpoint{
							{DNSName: "192.168.33.2", Targets: []string{"192.168.33.2"}},
						},
					},
				}, nil
			},
			validateResult: func(status ReconcileStatus, dnsClient *validatedDNSClient, err error) error {
				if status != ReconcileStatusContinue || err != nil {
					return fmt.Errorf("expected Reconcile status to be %v got %v. Expected err to be nil got %v", ReconcileStatusContinue, status, err)
				}
				if dnsClient.createCalled != 0 {
					return fmt.Errorf("expected create dns to be called 1 time but was called %d", dnsClient.createCalled)
				}
				if dnsClient.updateCalled != 1 {
					return fmt.Errorf("expected update dns to be called 0 times but was called %d", dnsClient.updateCalled)
				}
				return nil
			},
			ingressStatus: networkingv1.IngressStatus{
				LoadBalancer: corev1.LoadBalancerStatus{
					Ingress: []corev1.LoadBalancerIngress{{
						IP: "192.168.33.3",
					},
					},
				},
			},
			DNSLookup: func(ctx context.Context, host string) ([]dns.HostAddress, error) {
				return nil, fmt.Errorf("DNSLookup should not have been called")
			},
			expectedIPs: []string{"192.168.33.3"},
		},
		{
			Name: "test DNSRecord is created with correct values when it a host is returned",
			getDNS: func(ctx context.Context, accessor Interface) (*v1.DNSRecord, error) {
				return &v1.DNSRecord{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							ANNOTATION_HCG_HOST: managedHost},
					},
					Spec: v1.DNSRecordSpec{
						Endpoints: []*v1.Endpoint{},
					},
				}, nil
			},
			ingressStatus: networkingv1.IngressStatus{
				LoadBalancer: corev1.LoadBalancerStatus{
					Ingress: []corev1.LoadBalancerIngress{{
						Hostname: "test.example.com",
					},
					},
				},
			},
			DNSLookup: func(ctx context.Context, host string) ([]dns.HostAddress, error) {
				return []dns.HostAddress{{
					IP: net.ParseIP("192.168.33.2"),
				}}, nil
			},
			expectedIPs: []string{"192.168.33.2"},
			validateResult: func(status ReconcileStatus, dnsClient *validatedDNSClient, err error) error {
				if status != ReconcileStatusContinue || err != nil {
					return fmt.Errorf("expected Reconcile status to be %v got %v. Expected err to be nil got %v", ReconcileStatusContinue, status, err)
				}
				if dnsClient.createCalled != 0 {
					return fmt.Errorf("expected create dns to be called 1 time but was called %d", dnsClient.createCalled)
				}
				if dnsClient.updateCalled != 1 {
					return fmt.Errorf("expected update dns to be called 0 times but was called %d", dnsClient.updateCalled)
				}
				return nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			for _, acc := range setupAccessors(tc.ingressStatus) {
				fake := &validatedDNSClient{}
				rec := &DnsReconciler{
					GetDNS:           fake.get(tc.getDNS),
					CreateDNS:        fake.create(commonDNSValidate(tc.expectedIPs)),
					UpdateDNS:        fake.update(commonDNSValidate(tc.expectedIPs)),
					ListHostWatchers: fakewatcher,
					Log:              log.New(),
					DNSLookup:        tc.DNSLookup,
					WatchHost: func(ctx context.Context, key interface{}, host string) bool {
						return true
					},
				}
				result, err := rec.Reconcile(context.TODO(), acc)

				if err := tc.validateResult(result, fake, err); err != nil {
					t.Fatalf("unexpected error %s", err)
				}
			}
		})
	}

}

func Test_awsEndpointWeight(t *testing.T) {
	type args struct {
		numIPs int
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "single ip",
			args: args{
				numIPs: 1,
			},
			want: "120",
		},
		{
			name: "multiple ips 2",
			args: args{
				numIPs: 2,
			},
			want: "60",
		},
		{
			name: "multiple ips 3",
			args: args{
				numIPs: 3,
			},
			want: "40",
		},
		{
			name: "multiple ips 4",
			args: args{
				numIPs: 4,
			},
			want: "30",
		},
		{
			name: "60 ips",
			args: args{
				numIPs: 60,
			},
			want: "2",
		},
		{
			name: "61 ips",
			args: args{
				numIPs: 61,
			},
			want: "1",
		},
		{
			name: "ips equal to max weight (120)",
			args: args{
				numIPs: 120,
			},
			want: "1",
		},
		{
			name: "more IPs than max weight (121)",
			args: args{
				numIPs: 121,
			},
			want: "1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := awsEndpointWeight(tt.args.numIPs); got != tt.want {
				t.Errorf("awsEndpointWeight() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_objectKey(t *testing.T) {
	type args struct {
		obj runtime.Object
	}
	tests := []struct {
		name string
		args args
		want cache.ExplicitKey
	}{
		{
			name: "Ingress generated host",
			args: args{
				obj: Ingress{
					Ingress: &networkingv1.Ingress{
						ObjectMeta: metav1.ObjectMeta{Name: "echo"},
					},
				},
			},
			want: cache.ExplicitKey("echo"),
		},
		{
			name: "Route generated host",
			args: args{
				obj: Route{
					Route: &routev1.Route{
						ObjectMeta: metav1.ObjectMeta{Name: "someObject"},
					},
				},
			},
			want: cache.ExplicitKey("someObject"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := objectKey(tt.args.obj); got != tt.want {
				t.Errorf("objectKey() = %v, want %v", got, tt.want)
			}
		})
	}
}
