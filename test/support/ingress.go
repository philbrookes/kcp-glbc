/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package support

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/onsi/gomega"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	"github.com/onsi/gomega/types"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"

	workload "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"

	"github.com/kuadrant/kcp-glbc/pkg/dns"
	"github.com/kuadrant/kcp-glbc/pkg/traffic"

	"github.com/kcp-dev/logicalcluster/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func GetIngress(t Test, namespace *corev1.Namespace, name string) *traffic.Ingress {
	t.T().Helper()
	return Ingress(t, namespace, name)(t)
}

func Ingress(t Test, namespace *corev1.Namespace, name string) func(g gomega.Gomega) *traffic.Ingress {
	return func(g gomega.Gomega) *traffic.Ingress {
		ingress, err := t.Client().Core().Cluster(logicalcluster.From(namespace)).NetworkingV1().Ingresses(namespace.Name).Get(t.Ctx(), name, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return traffic.NewIngress(ingress)
	}
}

func IngressEndpoints(t Test, ingress traffic.Interface, res dns.HostResolver) []types.GomegaMatcher {
	host := ingress.GetAnnotations()[traffic.ANNOTATION_HCG_HOST]
	targets, err := ingress.GetDNSTargets()
	t.Expect(err).NotTo(gomega.HaveOccurred())
	matchers := []types.GomegaMatcher{}
	for _, target := range targets {
		matchers = append(matchers, MatchFieldsP(IgnoreExtras,
			Fields{
				"DNSName":          Equal(host),
				"Targets":          ConsistOf(target.Value),
				"RecordType":       Equal("A"),
				"RecordTTL":        Equal(kuadrantv1.TTL(60)),
				"SetIdentifier":    Equal(target.Value),
				"ProviderSpecific": ConsistOf(kuadrantv1.ProviderSpecific{{Name: "aws/weight", Value: "120"}}),
			}))

	}
	return matchers

}

func ValidateTransformedIngress(expectedSpec networkingv1.IngressSpec, transformed *traffic.Ingress, expectRulesPatch, expectTLSPatch bool) error {
	st := transformed.GetSyncTargets()
	for _, target := range st {
		// ensure each target has a transform value set and it is correct
		if _, ok := transformed.Annotations[workload.ClusterSpecDiffAnnotationPrefix+target]; !ok {
			return fmt.Errorf("expected a transformation for sync target " + target)
		}
		transforms := transformed.Annotations[workload.ClusterSpecDiffAnnotationPrefix+target]
		patches := []struct {
			Path  string                   `json:"path"`
			Op    string                   `json:"op"`
			Value []map[string]interface{} `json:"value"`
		}{}
		if err := json.Unmarshal([]byte(transforms), &patches); err != nil {
			return fmt.Errorf("failed to unmarshal patch %s", err)
		}
		//ensure there is a rules and tls patch and they have the correct value
		rulesPatch := false
		tlsPatch := false
		for _, p := range patches {
			if p.Path == "/rules" {
				rulesPatch = true
				rules := []networkingv1.IngressRule{}
				b, err := json.Marshal(p.Value)
				if err != nil {
					return fmt.Errorf("failed to marshal rules %s", err)
				}
				if err := json.Unmarshal(b, &rules); err != nil {
					return err
				}
				if !equality.Semantic.DeepEqual(rules, expectedSpec.Rules) {
					return fmt.Errorf("expected the rules in the transform to match the rules in transformed ingress")
				}
			}
			if p.Path == "/tls" {
				tlsPatch = true
				tls := []networkingv1.IngressTLS{}
				b, err := json.Marshal(p.Value)
				if err != nil {
					return fmt.Errorf("failed to marshal rules %s", err)
				}
				if err := json.Unmarshal(b, &tls); err != nil {
					return err
				}
				if !equality.Semantic.DeepEqual(tls, expectedSpec.TLS) {
					return fmt.Errorf("expected the tls section in the transform to match the tls in transformed ingress")
				}
			}
		}
		if !rulesPatch && expectRulesPatch {
			return fmt.Errorf("expected to find a rules patch but one was missing")
		}
		if !tlsPatch && expectTLSPatch {
			return fmt.Errorf("expected to find a tls patch but one was missing")
		}

	}
	return nil
}

// IngressTransformedSpec will look at the transforms applied and compare them to the expected spec. cbrookes TODO(look at whether we could take the ingress apply configuration)
func IngressTransformedSpec(test Test, expectedSpec networkingv1.IngressSpec, expectRulesDiff, expectTLSDiff bool) func(ingress *traffic.Ingress) bool {
	test.T().Log("Validating transformed spec for ingress")
	return func(ingress *traffic.Ingress) bool {
		if err := ValidateTransformedIngress(expectedSpec, ingress, expectRulesDiff, expectTLSDiff); err != nil {
			test.T().Log("transformed spec is not valid", err)
			return false

		}
		test.T().Log("transformed spec is valid")
		return true
	}
}

func GetIngresses(t Test, namespace *corev1.Namespace, labelSelector string) []networkingv1.Ingress {
	t.T().Helper()
	return Ingresses(t, namespace, labelSelector)(t)
}

func Ingresses(t Test, namespace *corev1.Namespace, labelSelector string) func(g gomega.Gomega) []networkingv1.Ingress {
	return func(g gomega.Gomega) []networkingv1.Ingress {
		ingresses, err := t.Client().Core().Cluster(logicalcluster.From(namespace)).NetworkingV1().Ingresses(namespace.Name).List(t.Ctx(), metav1.ListOptions{LabelSelector: labelSelector})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return ingresses.Items
	}
}

func IngressLoadBalancerIngresses(ingress *traffic.Ingress) []corev1.LoadBalancerIngress {
	for a, v := range ingress.Annotations {
		if strings.Contains(a, workload.InternalClusterStatusAnnotationPrefix) {
			ingressStatus := networkingv1.IngressStatus{}
			err := json.Unmarshal([]byte(v), &ingressStatus)
			if err != nil {
				return []corev1.LoadBalancerIngress{}
			}
			return ingressStatus.LoadBalancer.Ingress
		}
	}
	return []corev1.LoadBalancerIngress{}

}

func IngressTLS(ingress *networkingv1.Ingress) []networkingv1.IngressTLS {
	return ingress.Spec.TLS
}

// HostsEqualsToGeneratedHost checks Ingress hosts are the same as the generated hosts
func HostsEqualsToGeneratedHost(ingress *traffic.Ingress) bool {
	equals := true
	for _, rule := range ingress.Spec.Rules {
		if rule.Host != Annotations(ingress)[traffic.ANNOTATION_HCG_HOST] {
			equals = false
		}
	}
	return equals
}

func IngressLBHostEqualToGeneratedHost(ingress *traffic.Ingress, record *kuadrantv1.DNSRecord) bool {
	equals := true
	for _, i := range ingress.Status.LoadBalancer.Ingress {
		if i.Hostname != Annotations(record)[traffic.ANNOTATION_HCG_HOST] {
			equals = false
		}
	}
	return equals
}

func IngressOriginalSpecUnchanged(t Test, originalSpec *networkingv1.IngressSpec) func(ingress *traffic.Ingress) bool {
	t.T().Log("validating original spec is unchanged")
	return func(ingress *traffic.Ingress) bool {
		if !equality.Semantic.DeepEqual(ingress.Spec.Rules, originalSpec.Rules) {
			return false
		}
		if !equality.Semantic.DeepEqual(ingress.Spec.TLS, originalSpec.TLS) {
			return false
		}
		return true
	}
}

func GetDefaultIngressSpec(host, tlsSecretName, serviceName string) networkingv1.IngressSpec {
	defaultPathType := networkingv1.PathTypePrefix
	return networkingv1.IngressSpec{
		TLS: []networkingv1.IngressTLS{
			{
				Hosts:      []string{host},
				SecretName: tlsSecretName,
			},
		},
		Rules: []networkingv1.IngressRule{
			{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{
								Path:     "/",
								PathType: &defaultPathType,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: serviceName,
										Port: networkingv1.ServiceBackendPort{
											Name: "http",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// IngressHosts returns each unique host used in the rules
func IngressHosts(ingress *traffic.Ingress) map[string]string {
	hosts := map[string]string{}
	for _, rule := range ingress.Spec.Rules {
		hosts[rule.Host] = rule.Host
	}
	return hosts
}

// IngressPendingHosts returns each unique host in the pending rules annotation
func IngressPendingHosts(ingress *traffic.Ingress) map[string]string {
	hosts := map[string]string{}
	pendingRules := traffic.Pending{}
	pendingRulesAnnotation, ok := ingress.Annotations[traffic.ANNOTATION_PENDING_CUSTOM_HOSTS]
	if !ok {
		return hosts
	}
	if err := json.Unmarshal([]byte(pendingRulesAnnotation), &pendingRules); err != nil {
		return hosts
	}

	for _, rule := range pendingRules.Rules {
		hosts[rule.Host] = rule.Host
	}
	return hosts
}

func HasTLSSecretForGeneratedHost(secret string) func(ingress *traffic.Ingress) bool {
	return func(ingress *traffic.Ingress) bool {
		hostname := ingress.Annotations[traffic.ANNOTATION_HCG_HOST]
		for _, tls := range ingress.Spec.TLS {
			if len(tls.Hosts) == 1 && tls.Hosts[0] == hostname && tls.SecretName == secret {
				return true
			}
		}
		return false
	}
}
