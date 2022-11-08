//go:build smoke || performance

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

package smoke

import (
	"fmt"
	"sync"

	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kcp-dev/logicalcluster/v2"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	"github.com/kuadrant/kcp-glbc/pkg/traffic"
	. "github.com/kuadrant/kcp-glbc/test/support"
)

func createTestIngress(t Test, namespace *corev1.Namespace, serviceName string) *networkingv1.Ingress {
	name := GenerateName("test-ing-")

	ingress, err := t.Client().Core().Cluster(logicalcluster.From(namespace)).NetworkingV1().Ingresses(namespace.Name).
		Apply(t.Ctx(), IngressConfiguration(namespace.Name, name, serviceName, "test.glbc.com"), ApplyOptions)
	t.Expect(err).NotTo(HaveOccurred())

	t.T().Cleanup(func() {
		deleteTestIngress(t, namespace, ingress)
	})

	return ingress
}

func deleteTestIngress(t Test, namespace *corev1.Namespace, ingress *networkingv1.Ingress) {
	err := t.Client().Core().Cluster(logicalcluster.From(namespace)).NetworkingV1().Ingresses(namespace.Name).
		Delete(t.Ctx(), ingress.Name, metav1.DeleteOptions{})
	t.Expect(err).NotTo(HaveOccurred())
}

func assertIngressCleanup(t Test, namespace *corev1.Namespace) {
	// Assert Ingresses, DNSRecord and TLS Secret deletion success
	t.Eventually(Ingresses(t, namespace, "")).Should(HaveLen(0))
	t.Eventually(DNSRecords(t, namespace, "")).Should(HaveLen(0))
	t.Eventually(Secrets(t, namespace, "kuadrant.dev/hcg.managed=true")).Should(HaveLen(0))
}

func TestIngressBasic(t Test, ingressCount int, zoneID, glbcDomain string) {

	// Create the test workspace
	workspace := t.NewTestWorkspace()

	// Create GLBC APIBinding in workspace
	t.CreateGLBCAPIBindings(workspace, GLBCWorkspace, GLBCExportName)
	t.CreatePlacements(workspace)

	// Create a namespace
	namespace := t.NewTestNamespace(InWorkspace(workspace), WithLabel("kuadrant.dev/cluster-type", "glbc-ingresses"))
	t.Expect(namespace).NotTo(BeNil())

	name := "test-echo"
	// Create test Deployment
	_, err := t.Client().Core().Cluster(logicalcluster.From(namespace)).AppsV1().Deployments(namespace.Name).
		Apply(t.Ctx(), DeploymentConfiguration(namespace.Name, name), ApplyOptions)
	t.Expect(err).NotTo(HaveOccurred())

	// Create test Service
	_, err = t.Client().Core().Cluster(logicalcluster.From(namespace)).CoreV1().Services(namespace.Name).
		Apply(t.Ctx(), ServiceConfiguration(namespace.Name, name, map[string]string{}), ApplyOptions)
	t.Expect(err).NotTo(HaveOccurred())

	// Assertion that is run in the cleanup pahse to ensure all ingresses are removed
	t.T().Cleanup(func() {
		assertIngressCleanup(t, namespace)
	})

	t.T().Log(fmt.Sprintf("Creating %d Ingresses in %s", ingressCount, workspace.Name))

	// Create Ingresses store them as soon as they return from the create call to preserve spec
	var createdIngresses []*networkingv1.Ingress
	wg := sync.WaitGroup{}
	for i := 1; i <= ingressCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ingress := createTestIngress(t, namespace, name)
			t.Expect(ingress).NotTo(BeNil())
			createdIngresses = append(createdIngresses, ingress)
		}()
	}
	wg.Wait()
	// Retrieve Ingresses
	ingresses := GetIngresses(t, namespace, "")
	t.Expect(ingresses).Should(HaveLen(ingressCount))
	// Assert Ingresses reconcile success
	for _, ingress := range createdIngresses {

		t.Eventually(Ingress(t, namespace, ingress.Name)).WithTimeout(TestTimeoutShort).Should(And(
			WithTransform(Labels, And(
				HaveKey(traffic.LABEL_HAS_PENDING_HOSTS),
			)),
			// ensure the original spec has not changed
			Satisfy(IngressOriginalSpecUnchanged(t, &ingress.Spec)),
		))

	}
	t.T().Log("ingress in correct state")

	// Retrieve TLS Secrets
	t.Eventually(Secrets(t, namespace, "kuadrant.dev/hcg.managed=true")).WithTimeout(
		TestTimeoutLong).Should(HaveLen(ingressCount))

	t.T().Log("tls in correct state")
	// Retrieve DNSRecords
	t.Eventually(DNSRecords(t, namespace, "")).Should(HaveLen(ingressCount))
	dnsRecords := GetDNSRecords(t, namespace, "")
	t.Expect(dnsRecords).Should(HaveLen(ingressCount))

	// Assert DNSRecords reconcile success
	for _, record := range dnsRecords {
		t.Eventually(DNSRecord(t, namespace, record.Name)).Should(And(
			WithTransform(DNSRecordEndpointsCount, BeNumerically(">=", 1)),
			WithTransform(Annotations, And(
				HaveKey(traffic.ANNOTATION_HCG_HOST),
			)),
			WithTransform(DNSRecordCondition(zoneID, kuadrantv1.DNSRecordFailedConditionType), MatchFieldsP(IgnoreExtras,
				Fields{
					"Status":  Equal("False"),
					"Reason":  Equal("ProviderSuccess"),
					"Message": Equal("The DNS provider succeeded in ensuring the record"),
				})),
		))
	}

	t.T().Log("DNS record in correct state")
	ingresses = GetIngresses(t, namespace, "")
	t.Expect(ingresses).Should(HaveLen(ingressCount))
	// Assert Ingresses reconcile success now that cert and dns in place so the lb should be set to the generated host
	for _, ingress := range ingresses {
		tlsSecretName := fmt.Sprintf("hcg-tls-ingress-%s", ingress.Name)
		generatedHost := GetDNSRecord(t, namespace, ingress.Name).Annotations[traffic.ANNOTATION_HCG_HOST]
		t.T().Log("tls secret name ", tlsSecretName, "generated host ", generatedHost)
		t.Eventually(Ingress(t, namespace, ingress.Name)).WithTimeout(TestTimeoutMedium).Should(And(
			WithTransform(Labels, And(
				HaveKey(traffic.LABEL_HAS_PENDING_HOSTS),
			)),
			WithTransform(IngressLoadBalancerIngresses, HaveLen(1)),
			//No custom hosts approved so only expect our default spec in the transform
			Satisfy(IngressTransformedSpec(t, GetDefaultIngressSpec(generatedHost, tlsSecretName, name), true, true)),
		))
		record := GetDNSRecord(t, namespace, ingress.Name)
		trafficIngress := traffic.NewIngress(&ingress)
		if !IngressLBHostEqualToGeneratedHost(trafficIngress, record) {
			t.T().Fatalf("Generated host label on the ingress %s does not match load balancer host name %s", record.Annotations[traffic.ANNOTATION_HCG_HOST], trafficIngress.Status.LoadBalancer.Ingress[0].Hostname)
		}
	}

	t.T().Log("Ingress reached correct final state")

}
