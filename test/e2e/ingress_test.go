//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workload "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"
	"github.com/kuadrant/kcp-glbc/pkg/traffic"

	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"

	"github.com/kcp-dev/logicalcluster/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	. "github.com/kuadrant/kcp-glbc/test/support"
)

func TestIngress(t *testing.T) {
	test := With(t)
	test.T().Parallel()

	// Create the test workspace
	workspace := test.NewTestWorkspace()

	// Create GLBC APIBinding in workspace
	test.CreateGLBCAPIBindings(workspace, GLBCWorkspace, GLBCExportName)
	test.CreatePlacements(workspace)

	// Create a namespace
	namespace := test.NewTestNamespace(InWorkspace(workspace), WithLabel("kuadrant.dev/cluster-type", "glbc-ingresses"))

	name := "echo"

	// Create the Deployment
	_, err := test.Client().Core().Cluster(logicalcluster.From(namespace)).AppsV1().Deployments(namespace.Name).
		Apply(test.Ctx(), DeploymentConfiguration(namespace.Name, name), ApplyOptions)
	test.Expect(err).NotTo(HaveOccurred())
	defer func() {
		test.Expect(test.Client().Core().Cluster(logicalcluster.From(namespace)).AppsV1().Deployments(namespace.Name).
			Delete(test.Ctx(), name, metav1.DeleteOptions{})).
			To(Succeed())
	}()

	// Create the Service
	_, err = test.Client().Core().Cluster(logicalcluster.From(namespace)).CoreV1().Services(namespace.Name).
		Apply(test.Ctx(), ServiceConfiguration(namespace.Name, name, map[string]string{}), ApplyOptions)
	test.Expect(err).NotTo(HaveOccurred())
	defer func() {
		test.Expect(test.Client().Core().Cluster(logicalcluster.From(namespace)).CoreV1().Services(namespace.Name).
			Delete(test.Ctx(), name, metav1.DeleteOptions{})).
			To(Succeed())
	}()

	// Create the Ingress
	customHost := "test.gblb-custom.com"
	_, err = test.Client().Core().Cluster(logicalcluster.From(namespace)).NetworkingV1().Ingresses(namespace.Name).
		Apply(test.Ctx(), IngressConfiguration(namespace.Name, name, name, customHost), ApplyOptions)
	test.Expect(err).NotTo(HaveOccurred())

	defer func() {
		test.Expect(test.Client().Core().Cluster(logicalcluster.From(namespace)).NetworkingV1().Ingresses(namespace.Name).
			Delete(test.Ctx(), name, metav1.DeleteOptions{})).
			To(Succeed())
	}()

	// Create the dummy zone file configmap
	_, err = test.Client().Core().Cluster(GLBCWorkspace).CoreV1().ConfigMaps(ConfigmapNamespace).
		Create(test.Ctx(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ConfigmapName,
				Namespace: ConfigmapNamespace,
			},
		}, metav1.CreateOptions{})

	defer func() {
		test.Expect(test.Client().Core().Cluster(GLBCWorkspace).CoreV1().ConfigMaps(ConfigmapNamespace).
			Delete(test.Ctx(), ConfigmapName, metav1.DeleteOptions{})).
			To(Succeed())
	}()

	// Wait until the Ingress is reconciled with the load balancer Ingresses
	test.Eventually(Ingress(test, namespace, name)).WithTimeout(TestTimeoutMedium).Should(And(
		WithTransform(Annotations, And(
			HaveKey(traffic.ANNOTATION_HCG_HOST),
			HaveKey(traffic.ANNOTATION_PENDING_CUSTOM_HOSTS),
		)),
		WithTransform(Labels, And(
			HaveKey(traffic.LABEL_HAS_PENDING_HOSTS),
		)),
		WithTransform(LoadBalancerIngresses, HaveLen(1)),
		Satisfy(HostsEqualsToGeneratedHost),
	))

	// Retrieve the Ingress
	ingress := GetIngress(test, namespace, name)

	zoneID := os.Getenv("AWS_DNS_PUBLIC_ZONE_ID")
	test.Expect(zoneID).NotTo(BeNil())

	ingressStatus := &networkingv1.IngressStatus{}
	for a, v := range ingress.Annotations {
		if strings.Contains(a, workload.InternalClusterStatusAnnotationPrefix) {
			err = json.Unmarshal([]byte(v), &ingressStatus)
			break
		}
	}

	// Check a DNSRecord for the Ingress is created with the expected Spec
	test.Eventually(DNSRecord(test, namespace, name)).Should(And(
		// ensure the ingress certificate is marked as ready when the DNSrecord is created
		WithTransform(DNSRecordToIngressCertReady(test, namespace, name), Equal("ready")),
		WithTransform(DNSRecordEndpoints, HaveLen(1)),
		WithTransform(DNSRecordEndpoints, ContainElement(MatchFieldsP(IgnoreExtras,
			Fields{
				"DNSName":          Equal(ingress.Annotations[traffic.ANNOTATION_HCG_HOST]),
				"Targets":          ConsistOf(ingressStatus.LoadBalancer.Ingress[0].IP),
				"RecordType":       Equal("A"),
				"RecordTTL":        Equal(kuadrantv1.TTL(60)),
				"SetIdentifier":    Equal(ingressStatus.LoadBalancer.Ingress[0].IP),
				"ProviderSpecific": ConsistOf(kuadrantv1.ProviderSpecific{{Name: "aws/weight", Value: "120"}}),
			})),
		),
		WithTransform(DNSRecordCondition(zoneID, kuadrantv1.DNSRecordFailedConditionType), MatchFieldsP(IgnoreExtras,
			Fields{
				"Status":  Equal("False"),
				"Reason":  Equal("ProviderSuccess"),
				"Message": Equal("The DNS provider succeeded in ensuring the record"),
			})),
	))

	// Create a domain verification for the custom domain
	test.Client().Kuadrant().Cluster(logicalcluster.From(ingress)).KuadrantV1().DomainVerifications().Create(test.Ctx(), &kuadrantv1.DomainVerification{
		ObjectMeta: metav1.ObjectMeta{
			Name: customHost,
		},
		Spec: kuadrantv1.DomainVerificationSpec{
			Domain: customHost,
		},
	}, metav1.CreateOptions{})
	defer func() {
		test.Expect(test.Client().Kuadrant().Cluster(logicalcluster.From(namespace)).KuadrantV1().DomainVerifications().
			Delete(test.Ctx(), customHost, metav1.DeleteOptions{})).
			To(Succeed())
	}()

	// see domain verification is not verified
	test.Eventually(DomainVerification(test, logicalcluster.From(ingress), customHost)).WithTimeout(TestTimeoutMedium).Should(And(
		WithTransform(DomainVerificationFor, Equal(customHost)),
		WithTransform(DomainVerified, Equal(false)),
		WithTransform(DomainToken, Not(Equal(""))),
	))

	// see custom host is not active in ingress
	// see custom host is in pending annotation
	test.Eventually(Ingress(test, namespace, name)).WithTimeout(TestTimeoutMedium).Should(And(
		WithTransform(IngressHosts, Not(ContainElement(customHost))),
		WithTransform(IngressPendingHosts, ContainElement(customHost)),
	))

	// get domainVerification in order to read required token
	dv, err := test.Client().Kuadrant().Cluster(logicalcluster.From(ingress)).KuadrantV1().DomainVerifications().Get(test.Ctx(), customHost, metav1.GetOptions{})
	test.Expect(err).NotTo(HaveOccurred())

	// set TXT record in DNS
	err = SetTXTRecord(test, customHost, dv.Status.Token)
	test.Expect(err).NotTo(HaveOccurred())

	// see domain verification is verified
	test.Eventually(DomainVerification(test, logicalcluster.From(ingress), customHost)).WithTimeout(TestTimeoutMedium).Should(And(
		WithTransform(DomainVerificationFor, Equal(customHost)),
		WithTransform(DomainVerified, Equal(true)),
		WithTransform(DomainToken, Equal(dv.Status.Token)),
	))

	// see custom host is active in ingress
	// see custom host cleared from pending annotations
	test.Eventually(Ingress(test, namespace, name)).WithTimeout(TestTimeoutMedium).Should(And(
		WithTransform(IngressPendingHosts, Not(ContainElement(customHost))),
		WithTransform(IngressHosts, ContainElement(customHost)),
	))
}
