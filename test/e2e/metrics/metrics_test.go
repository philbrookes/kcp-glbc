//go:build e2e

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

package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	workload "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"

	"github.com/kuadrant/kcp-glbc/pkg/traffic"

	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	"github.com/onsi/gomega/types"

	prometheus "github.com/prometheus/client_model/go"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kcp-dev/logicalcluster/v2"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	. "github.com/kuadrant/kcp-glbc/test/support"
)

const issuer = "glbc-ca"

func TestMetrics(t *testing.T) {
	test := With(t)

	// Assert the metrics are initialized
	test.Expect(GetMetrics(test)).To(And(
		HaveKey("glbc_ingress_managed_object_total"),
		WithTransform(Metric("glbc_ingress_managed_object_total"), EqualP(
			ingressManagedObjectTotal(0),
		)),
		// glbc_ingress_managed_object_time_to_admission
		HaveKey("glbc_ingress_managed_object_time_to_admission"),
		WithTransform(Metric("glbc_ingress_managed_object_time_to_admission"), EqualP(
			ingressManagedObjectTimeToAdmission(0, -1),
		)),
		// glbc_tls_certificate_pending_request_count
		HaveKey("glbc_tls_certificate_pending_request_count"),
		WithTransform(Metric("glbc_tls_certificate_pending_request_count"), EqualP(
			certificatePendingRequestCount(issuer, 0),
		)),
		// glbc_tls_certificate_request_total
		HaveKey("glbc_tls_certificate_request_total"),
		WithTransform(Metric("glbc_tls_certificate_request_total"), EqualP(
			certificateRequestTotal(issuer, 0, 0),
		)),
		// glbc_tls_certificate_request_errors_total
		HaveKey("glbc_tls_certificate_request_errors_total"),
		WithTransform(Metric("glbc_tls_certificate_request_errors_total"), EqualP(
			certificateRequestErrorsTotal(issuer, 0),
		)),
		// glbc_tls_certificate_secret_count
		HaveKey("glbc_tls_certificate_secret_count"),
		WithTransform(Metric("glbc_tls_certificate_secret_count"), MatchFieldsP(IgnoreExtras,
			Fields{
				"Name":   EqualP("glbc_tls_certificate_secret_count"),
				"Help":   EqualP("GLBC TLS certificate secret count"),
				"Type":   EqualP(prometheus.MetricType_GAUGE),
				"Metric": ContainElement(certificateSecretCount(issuer, 0)),
			},
		)),
		// Client go rest metrics should exist
		// Asserting actual values may cause flakes, so just existence will suffice
		HaveKey("rest_client_request_latency_seconds"),
		HaveKey("rest_client_requests_total"),
		// glbc_tls_certificate_issuance_duration_seconds
		// histogram vector are not initialized
		Not(HaveKey("glbc_tls_certificate_issuance_duration_seconds")),
	))

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

	// Create the Service
	_, err = test.Client().Core().Cluster(logicalcluster.From(namespace)).CoreV1().Services(namespace.Name).
		Apply(test.Ctx(), ServiceConfiguration(namespace.Name, name, map[string]string{}), ApplyOptions)
	test.Expect(err).NotTo(HaveOccurred())

	// Create the Ingress, it's delayed and run in a separate Go routine, to mitigate the race
	// where cert-manager is being too prompt to issue the TLS certificate (which turns out to be quick fast
	// when using a CA issuer), and the below assertion happens too late to detect the pending TLS certificate request.
	timer := time.AfterFunc(2*time.Second, func() {
		_, err = test.Client().Core().Cluster(logicalcluster.From(namespace)).NetworkingV1().Ingresses(namespace.Name).
			Apply(test.Ctx(), IngressConfiguration(namespace.Name, name, name, "test.gblb.com"), ApplyOptions)
		test.Expect(err).NotTo(HaveOccurred())
	})
	t.Cleanup(func() {
		timer.Stop()
	})

	// We pull the metrics aggressively as the certificate can be issued quickly when using the CA issuer.
	// We may want to adjust the pull interval as well as the timeout based on the configured issuer.
	test.Eventually(Metrics(test), TestTimeoutMedium, 10*time.Millisecond).Should(And(
		HaveKey("glbc_tls_certificate_pending_request_count"),
		WithTransform(Metric("glbc_tls_certificate_pending_request_count"), Satisfy(
			func(m *prometheus.MetricFamily) bool {
				match, _ := EqualP(certificatePendingRequestCount(issuer, 1)).Match(m)
				return match
			},
		)),
	))
	secretName := fmt.Sprintf("hcg-tls-ingress-%s", name)

	// Wait until the Ingress is reconciled with the load balancer Ingresses
	test.Eventually(Ingress(test, namespace, name)).WithTimeout(TestTimeoutMedium).Should(And(
		// Host spec
		WithTransform(Annotations, And(
			HaveKey(traffic.ANNOTATION_HCG_HOST),
			HaveKey(traffic.ANNOTATION_PENDING_CUSTOM_HOSTS),
		)),
		WithTransform(Labels, And(
			HaveKey(traffic.LABEL_HAS_PENDING_HOSTS),
		)),
		// Rules spec
		Satisfy(HostsEqualsToGeneratedHost),
		// TLS certificate spec
		Satisfy(HasTLSSecretForGeneratedHost(secretName)),
		// Load balancer status
		WithTransform(LoadBalancerIngresses, HaveLen(1)),
	))

	ingress := GetIngress(test, namespace, name)

	// Check the TLS Secret
	test.Eventually(Secret(test, namespace, secretName)).WithTimeout(TestTimeoutMedium).Should(
		WithTransform(Certificate, PointTo(
			MatchFields(IgnoreExtras, map[string]types.GomegaMatcher{
				"DNSNames": ConsistOf(ingress.Annotations[traffic.ANNOTATION_HCG_HOST]),
			}),
		)),
	)

	zoneID := os.Getenv("AWS_DNS_PUBLIC_ZONE_ID")
	test.Expect(zoneID).NotTo(BeNil())

	ingressStatus := &networkingv1.IngressStatus{}
	for a, v := range ingress.Annotations {
		if strings.Contains(a, workload.InternalClusterStatusAnnotationPrefix) {
			err = json.Unmarshal([]byte(v), &ingressStatus)
			break
		}
	}
	test.Expect(err).NotTo(HaveOccurred())

	// Check a DNSRecord for the Ingress is updated with the expected Spec
	test.Eventually(DNSRecord(test, namespace, name)).WithTimeout(TestTimeoutMedium).Should(And(
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
	// TODO(cbrookes) if we want to keep this test we need to get the certificate not the secret
	//secret := GetSecret(test, namespace, ingress.Spec.TLS[0].SecretName)
	// Ingress creation timestamp is serialized to RFC3339 format and set in an annotation on the certificate request
	//duration := secret.CreationTimestamp.Sub(ingress.CreationTimestamp.Rfc3339Copy().Time).Seconds()

	// Check the metrics
	test.Expect(GetMetrics(test)).To(And(
		HaveKey("glbc_ingress_managed_object_total"),
		// should be managing 1 Ingress
		WithTransform(Metric("glbc_ingress_managed_object_total"), EqualP(
			ingressManagedObjectTotal(1),
		)),
		HaveKey("glbc_tls_certificate_pending_request_count"),
		WithTransform(Metric("glbc_tls_certificate_pending_request_count"), EqualP(
			certificatePendingRequestCount(issuer, 0),
		)),
		HaveKey("glbc_tls_certificate_request_total"),
		WithTransform(Metric("glbc_tls_certificate_request_total"), EqualP(
			certificateRequestTotal(issuer, 1, 0),
		)),
		HaveKey("glbc_tls_certificate_request_errors_total"),
		WithTransform(Metric("glbc_tls_certificate_request_errors_total"), EqualP(
			certificateRequestErrorsTotal(issuer, 0),
		)),
		HaveKey("glbc_tls_certificate_secret_count"),
		WithTransform(Metric("glbc_tls_certificate_secret_count"), MatchFieldsP(IgnoreExtras,
			Fields{
				"Name":   EqualP("glbc_tls_certificate_secret_count"),
				"Help":   EqualP("GLBC TLS certificate secret count"),
				"Type":   EqualP(prometheus.MetricType_GAUGE),
				"Metric": ContainElement(certificateSecretCount(issuer, 1)),
			},
		)),
		// TODO(cbrookes) need to get the certificate rather than the secret now
		// HaveKey("glbc_tls_certificate_issuance_duration_seconds"),
		// WithTransform(Metric("glbc_tls_certificate_issuance_duration_seconds"), EqualP(
		// 	certificateIssuanceDurationSeconds(issuer, 1, duration),
		// )),
	))

	// Wait for a period of time to allow all reconciliations to be completed
	// ToDo (mnairn) Is there any way we can do an assertion on something to know we are at this point?
	// Needs investigation into what is actually triggering a reconciliation after the DNSRecord is finished.
	time.Sleep(30 * time.Second)

	// Take a snapshot of the reconciliation metrics
	reconcileTotal := GetMetric(test, "glbc_controller_reconcile_total")
	// Continually gets the metrics and check no reconciliation occurred over a reasonable period of time.
	test.Consistently(Metrics(test), 30*time.Second).Should(And(
		HaveKey("glbc_controller_reconcile_total"),
		WithTransform(Metric("glbc_controller_reconcile_total"), Equal(reconcileTotal)),
	))

	// Finally, delete the Ingress and assert the metrics to cover the entire lifecycle
	test.Expect(test.Client().Core().Cluster(logicalcluster.From(namespace)).NetworkingV1().Ingresses(namespace.Name).
		Delete(test.Ctx(), name, metav1.DeleteOptions{})).
		To(Succeed())

	// Only the TLS certificate Secret count and number of managed Ingresses should change
	test.Eventually(Metrics(test), TestTimeoutShort).Should(And(
		HaveKey("glbc_tls_certificate_secret_count"),
		WithTransform(Metric("glbc_tls_certificate_secret_count"), MatchFieldsP(IgnoreExtras,
			Fields{
				"Name":   EqualP("glbc_tls_certificate_secret_count"),
				"Help":   EqualP("GLBC TLS certificate secret count"),
				"Type":   EqualP(prometheus.MetricType_GAUGE),
				"Metric": ContainElement(certificateSecretCount(issuer, 0)),
			},
		)),
		HaveKey("glbc_ingress_managed_object_total"),
		WithTransform(Metric("glbc_ingress_managed_object_total"), EqualP(
			ingressManagedObjectTotal(0)),
		),
	))

	// The other metrics should not be updated
	test.Consistently(Metrics(test), 15*time.Second).Should(And(
		HaveKey("glbc_tls_certificate_pending_request_count"),
		WithTransform(Metric("glbc_tls_certificate_pending_request_count"), EqualP(
			certificatePendingRequestCount(issuer, 0),
		)),
		HaveKey("glbc_tls_certificate_request_total"),
		WithTransform(Metric("glbc_tls_certificate_request_total"), EqualP(
			certificateRequestTotal(issuer, 1, 0),
		)),
		HaveKey("glbc_tls_certificate_request_errors_total"),
		WithTransform(Metric("glbc_tls_certificate_request_errors_total"), EqualP(
			certificateRequestErrorsTotal(issuer, 0)),
		),
		// HaveKey("glbc_tls_certificate_issuance_duration_seconds"),
		// WithTransform(Metric("glbc_tls_certificate_issuance_duration_seconds"), EqualP(
		// 	certificateIssuanceDurationSeconds(issuer, 1, duration),
		// )),
	))
}

func ingressManagedObjectTotal(value float64) prometheus.MetricFamily {
	return prometheus.MetricFamily{
		Name: stringP("glbc_ingress_managed_object_total"),
		Help: stringP("Total number of managed ingress object"),
		Type: metricTypeP(prometheus.MetricType_GAUGE),
		Metric: []*prometheus.Metric{
			{
				Gauge: &prometheus.Gauge{
					Value: float64P(value),
				},
			},
		},
	}
}

func ingressManagedObjectTimeToAdmission(count uint64, duration float64) prometheus.MetricFamily {
	return prometheus.MetricFamily{
		Name: stringP("glbc_ingress_managed_object_time_to_admission"),
		Help: stringP("Duration of the ingress object admission"),
		Type: metricTypeP(prometheus.MetricType_HISTOGRAM),
		Metric: []*prometheus.Metric{
			{
				Histogram: &prometheus.Histogram{
					SampleCount: uint64P(count),
					SampleSum:   positiveFloat64P(duration),
					Bucket: buckets(duration, []float64{
						1 * time.Second.Seconds(),
						5 * time.Second.Seconds(),
						10 * time.Second.Seconds(),
						15 * time.Second.Seconds(),
						30 * time.Second.Seconds(),
						45 * time.Second.Seconds(),
						1 * time.Minute.Seconds(),
						2 * time.Minute.Seconds(),
						5 * time.Minute.Seconds(),
						math.Inf(1),
					}),
				},
			},
		},
	}
}

func certificatePendingRequestCount(issuer string, _ float64) prometheus.MetricFamily {
	return prometheus.MetricFamily{
		Name: stringP("glbc_tls_certificate_pending_request_count"),
		Help: stringP("GLBC TLS certificate pending request count"),
		Type: metricTypeP(prometheus.MetricType_GAUGE),
		Metric: []*prometheus.Metric{
			{
				Label: []*prometheus.LabelPair{
					label("issuer", issuer),
				},
				Gauge: &prometheus.Gauge{
					Value: float64P(0),
				},
			},
		},
	}
}

func certificateRequestTotal(issuer string, succeeded, failed float64) prometheus.MetricFamily {
	return prometheus.MetricFamily{
		Name: stringP("glbc_tls_certificate_request_total"),
		Help: stringP("GLBC TLS certificate total number of requests"),
		Type: metricTypeP(prometheus.MetricType_COUNTER),
		Metric: []*prometheus.Metric{
			{
				Label: []*prometheus.LabelPair{
					label("issuer", issuer),
					label("result", "failed"),
				},
				Counter: &prometheus.Counter{
					Value: float64P(failed),
				},
			},
			{
				Label: []*prometheus.LabelPair{
					label("issuer", issuer),
					label("result", "succeeded"),
				},
				Counter: &prometheus.Counter{
					Value: float64P(succeeded),
				},
			},
		},
	}
}

func certificateRequestErrorsTotal(issuer string, value float64) prometheus.MetricFamily {
	return prometheus.MetricFamily{
		Name: stringP("glbc_tls_certificate_request_errors_total"),
		Help: stringP("GLBC TLS certificate total number of request errors"),
		Type: metricTypeP(prometheus.MetricType_COUNTER),
		Metric: []*prometheus.Metric{
			{
				Label: []*prometheus.LabelPair{
					label("issuer", issuer),
				},
				Counter: &prometheus.Counter{
					Value: float64P(value),
				},
			},
		},
	}
}

func certificateIssuanceDurationSeconds(issuer string, count uint64, duration float64) prometheus.MetricFamily {
	return prometheus.MetricFamily{
		Name: stringP("glbc_tls_certificate_issuance_duration_seconds"),
		Help: stringP("GLBC TLS certificate issuance duration"),
		Type: metricTypeP(prometheus.MetricType_HISTOGRAM),
		Metric: []*prometheus.Metric{
			{
				Label: []*prometheus.LabelPair{
					label("issuer", issuer),
					label("result", "succeeded"),
				},
				Histogram: &prometheus.Histogram{
					SampleCount: uint64P(count),
					SampleSum:   positiveFloat64P(duration),
					Bucket: buckets(duration, []float64{
						1 * time.Second.Seconds(),
						5 * time.Second.Seconds(),
						10 * time.Second.Seconds(),
						15 * time.Second.Seconds(),
						30 * time.Second.Seconds(),
						45 * time.Second.Seconds(),
						1 * time.Minute.Seconds(),
						2 * time.Minute.Seconds(),
						5 * time.Minute.Seconds(),
						math.Inf(1),
					}),
				},
			},
		},
	}
}

func certificateSecretCount(issuer string, value float64) *prometheus.Metric {
	return &prometheus.Metric{
		Label: []*prometheus.LabelPair{
			label("issuer", issuer),
		},
		Gauge: &prometheus.Gauge{
			Value: float64P(value),
		},
	}
}
