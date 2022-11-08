//go:build e2e
// +build e2e

package e2e

import (
	"crypto/x509/pkix"
	"os"
	"testing"

	"github.com/onsi/gomega/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kuadrant/kcp-glbc/pkg/_internal/env"
	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	"github.com/kuadrant/kcp-glbc/pkg/dns"
	"github.com/kuadrant/kcp-glbc/pkg/traffic"

	routeapiv1 "github.com/openshift/api/route/v1"

	"github.com/kcp-dev/logicalcluster/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	corev1 "k8s.io/api/core/v1"

	. "github.com/kuadrant/kcp-glbc/test/support"
)

// TestRoute covers the main interactions with the route object. Assigning a GLBC host, custom domains transformations and TLS. It uses a dummy DNS provider
func TestRoute(t *testing.T) {
	test := With(t)
	test.T().Parallel()

	// Create the test workspace
	workspace := test.NewTestWorkspace()

	// Create GLBC APIBinding in workspace
	test.CreateGLBCAPIBindings(workspace, GLBCWorkspace, GLBCExportName)
	test.CreatePlacements(workspace)

	// Create a namespace
	namespace := test.NewTestNamespace(InWorkspace(workspace), WithLabel("kuadrant.dev/cluster-type", "glbc-routes"))

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

	// Create the route with a custom domain set
	customHost := "route-test.routes-gblb-custom.com"

	//storing this to confirm it is restored once the custom domain is verified
	originalTLSConfig := &routeapiv1.TLSConfig{
		Termination:   routeapiv1.TLSTerminationEdge,
		Certificate:   "",
		Key:           "",
		CACertificate: "",
	}

	route := &routeapiv1.Route{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Route",
			APIVersion: RouteResource.GroupVersion().String(),
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: routeapiv1.RouteSpec{
			Host:           customHost,
			WildcardPolicy: routeapiv1.WildcardPolicyNone,
			TLS:            originalTLSConfig,
			To:             routeapiv1.RouteTargetReference{Name: name, Kind: "Service"},
			Path:           "/",
		},
	}
	uRoute, err := runtime.DefaultUnstructuredConverter.ToUnstructured(route)
	test.Expect(err).NotTo(HaveOccurred())

	originalUnstructuredRoute, err := test.Client().Dynamic().Cluster(logicalcluster.From(namespace)).Resource(RouteResource).Namespace(namespace.Name).Create(
		test.Ctx(),
		&unstructured.Unstructured{Object: uRoute},
		metav1.CreateOptions{},
	)
	test.Expect(err).NotTo(HaveOccurred())

	originalRoute, err := TrafficRouteFromUnstructured(originalUnstructuredRoute)
	test.Expect(err).NotTo(HaveOccurred())
	secretName := traffic.TLSSecretName(originalRoute)

	defer func() {
		test.Expect(test.Client().Dynamic().Cluster(logicalcluster.From(namespace)).Resource(RouteResource).Namespace(namespace.Name).
			Delete(test.Ctx(), originalRoute.Name, metav1.DeleteOptions{})).
			To(Succeed())
	}()

	// Create the dummy zone file configmap
	zoneConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigmapName,
			Namespace: ConfigmapNamespace,
		},
	}
	_, _ = test.Client().Core().Cluster(GLBCWorkspace).CoreV1().ConfigMaps(ConfigmapNamespace).
		Create(test.Ctx(), zoneConfigMap, metav1.CreateOptions{})

	// set empty TXT record in DNS
	err = SetARecord(test, customHost, "192.168.0.1")
	test.Expect(err).NotTo(HaveOccurred())

	// Test our annotations are as expected and that we have not modified the original spec
	test.Eventually(Route(test, namespace, name)).WithTimeout(TestTimeoutMedium).Should(And(
		WithTransform(Labels, And(
			HaveKey(traffic.LABEL_HAS_PENDING_HOSTS),
		)),
		// ensure the original spec has not changed
		Satisfy(OriginalSpecUnchanged(test, originalRoute)),
	))
	test.T().Log("route in expected state")

	dnsRecords := GetDNSRecord(test, namespace, name)
	hostname := dnsRecords.Annotations[traffic.ANNOTATION_HCG_HOST]
	resolver := dns.ConfigMapHostResolver{}
	zoneID := os.Getenv("AWS_DNS_PUBLIC_ZONE_ID")
	test.Expect(zoneID).NotTo(BeNil())

	err = SetARecord(test, hostname, "192.168.0.1")
	test.Expect(err).NotTo(HaveOccurred())

	//TLS
	issuer := env.GetEnvString("GLBC_TLS_PROVIDER", "glbc-ca")
	fields := map[string]types.GomegaMatcher{
		"DNSNames": ConsistOf(hostname),
	}
	switch issuer {

	case "glbc-ca":
		fields["Issuer"] = Equal(pkix.Name{
			Organization: []string{"Kuadrant"},
			Names: []pkix.AttributeTypeAndValue{
				{
					Type:  []int{2, 5, 4, 10},
					Value: "Kuadrant",
				},
			},
		})

	case "le-staging":
		fields["Issuer"] = Equal(pkix.Name{
			Country:      []string{"US"},
			Organization: []string{"(STAGING) Let's Encrypt"},
			CommonName:   "(STAGING) Artificial Apricot R3",
			Names: []pkix.AttributeTypeAndValue{
				{
					Type:  []int{2, 5, 4, 6},
					Value: "US",
				},
				{
					Type:  []int{2, 5, 4, 10},
					Value: "(STAGING) Let's Encrypt",
				},
				{
					Type:  []int{2, 5, 4, 3},
					Value: "(STAGING) Artificial Apricot R3",
				},
			},
		})
		fields["Subject"] = Equal(pkix.Name{
			CommonName: hostname,
			Names: []pkix.AttributeTypeAndValue{
				{
					Type:  []int{2, 5, 4, 3},
					Value: hostname,
				},
			},
		})

	case "le-production":
		fields["Issuer"] = Equal(pkix.Name{
			Country:      []string{"US"},
			Organization: []string{"Let's Encrypt"},
			CommonName:   "Artificial Apricot R3",
			Names: []pkix.AttributeTypeAndValue{
				{
					Type:  []int{2, 5, 4, 6},
					Value: "US",
				},
				{
					Type:  []int{2, 5, 4, 10},
					Value: "Let's Encrypt",
				},
				{
					Type:  []int{2, 5, 4, 3},
					Value: "R3",
				},
			},
		})
		fields["Subject"] = Equal(pkix.Name{
			CommonName: hostname,
			Names: []pkix.AttributeTypeAndValue{
				{
					Type:  []int{2, 5, 4, 3},
					Value: hostname,
				},
			},
		})
	}
	//// check our tls secret exists and is correct
	test.Eventually(Secret(test, namespace, secretName)).
		WithTimeout(TestTimeoutShort).
		Should(
			WithTransform(Labels, And(HaveKeyWithValue("kuadrant.dev/hcg.managed", "true"))),
			WithTransform(Certificate, PointTo(MatchFields(IgnoreExtras, fields))),
		)

	test.T().Log("tls secret exists and is correctly configured")

	// DNS Check a DNSRecord for the Route is created with the expected Spec
	test.Eventually(DNSRecord(test, namespace, name)).Should(And(
		// ensure the ingress certificate is marked as ready when the DNSrecord is created
		WithTransform(DNSRecordToRouteCertReady(test, namespace, name), Equal("ready")),
		WithTransform(DNSRecordEndpoints, HaveLen(1)),
		WithTransform(DNSRecordEndpoints, ContainElements(IngressEndpoints(test, originalRoute, &resolver))),
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
	test.T().Log("DNS is as expected")

	glbcHost := dnsRecords.Annotations[traffic.ANNOTATION_HCG_HOST]
	tlsSecret := Secret(test, namespace, secretName)(test)
	tlsConfig := &routeapiv1.TLSConfig{
		Termination:   routeapiv1.TLSTerminationEdge,
		Certificate:   string(tlsSecret.Data[corev1.TLSCertKey]),
		Key:           string(tlsSecret.Data[corev1.TLSPrivateKeyKey]),
		CACertificate: string(tlsSecret.Data[corev1.ServiceAccountRootCAKey]),
	}

	// Test that our transforms have the expected spec and that our status is set to the generated host
	test.Eventually(Route(test, namespace, name)).WithTimeout(TestTimeoutShort).Should(And(
		Satisfy(OriginalSpecUnchanged(test, originalRoute)),
		Satisfy(RouteTransformedSpec(test, GetDefaultRouteSpec(glbcHost, name, tlsConfig))),
		//check that we have a LB set to our generated host
		WithTransform(RouteLoadBalancerIngresses, HaveLen(1)),
		Satisfy(RouteLBHostEqualToGeneratedHost),
	))
	test.T().Log("transforms are in place and ingress is ready (dns load balancer is set in the status)")

	// Create a domain verification for the custom domain
	_, err = test.Client().Kuadrant().Cluster(logicalcluster.From(originalRoute)).KuadrantV1().DomainVerifications().Create(test.Ctx(), &kuadrantv1.DomainVerification{
		ObjectMeta: metav1.ObjectMeta{
			Name: customHost,
		},
		Spec: kuadrantv1.DomainVerificationSpec{
			Domain: customHost,
		},
	}, metav1.CreateOptions{})
	test.Expect(err).NotTo(HaveOccurred())
	defer func() {
		test.Expect(test.Client().Kuadrant().Cluster(logicalcluster.From(namespace)).KuadrantV1().DomainVerifications().
			Delete(test.Ctx(), customHost, metav1.DeleteOptions{})).
			To(Succeed())
	}()

	// see domain verification is not verified
	test.Eventually(DomainVerification(test, logicalcluster.From(originalRoute), customHost)).WithTimeout(TestTimeoutMedium).Should(And(
		WithTransform(DomainVerificationFor, Equal(customHost)),
		WithTransform(DomainVerified, Equal(false)),
		WithTransform(DomainToken, Not(Equal(""))),
	))

	// see custom host is not active in route
	test.Eventually(Route(test, namespace, name)).WithTimeout(TestTimeoutMedium).Should(And(
		Satisfy(OriginalSpecUnchanged(test, originalRoute)),
		Satisfy(RouteTransformedSpec(test, GetDefaultRouteSpec(glbcHost, name, tlsConfig))),
	))
	test.T().Log("domain not verified custom host not propagated to cluster")

	// get domainVerification in order to read required token
	dv, err := test.Client().Kuadrant().Cluster(logicalcluster.From(originalRoute)).KuadrantV1().DomainVerifications().Get(test.Ctx(), customHost, metav1.GetOptions{})
	test.Expect(err).NotTo(HaveOccurred())

	// set TXT record in DNS
	err = SetTXTRecord(test, customHost, dv.Status.Token)
	test.Expect(err).NotTo(HaveOccurred())

	// see domain verification is verified
	test.Eventually(DomainVerification(test, logicalcluster.From(originalRoute), customHost)).WithTimeout(TestTimeoutShort).Should(And(
		WithTransform(DomainVerificationFor, Equal(customHost)),
		WithTransform(DomainVerified, Equal(true)),
		WithTransform(DomainToken, Equal(dv.Status.Token)),
	))
	test.T().Log("domain is now verified")
	withCustomDomain := GetDefaultRouteSpec(glbcHost, name, originalTLSConfig)
	withCustomDomain.Host = customHost

	// now we have built up our expected transformed spec check it is the same as the transformations applied to the annotations
	test.Eventually(Route(test, namespace, name)).WithTimeout(TestTimeoutShort).Should(And(
		Satisfy(RouteTransformedSpec(test, withCustomDomain)),
	))

	//verify shadow is created
	test.Eventually(Route(test, namespace, name+"-shadow")).WithTimeout(TestTimeoutShort).Should(And(
		Satisfy(RouteTransformedSpec(test, GetDefaultRouteSpec(glbcHost, name+"-shadow", tlsConfig))),
	))

	test.T().Log("route is transformed correctly and in final state")
}
