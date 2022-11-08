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

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/kcp-dev/logicalcluster/v2"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	"github.com/kuadrant/kcp-glbc/pkg/dns"
	traffic "github.com/kuadrant/kcp-glbc/pkg/traffic"
)

const (
	ConfigmapName      = "hosts"
	ConfigmapNamespace = "kcp-glbc"
)

func GetDNSRecord(t Test, namespace *corev1.Namespace, name string) *kuadrantv1.DNSRecord {
	t.T().Helper()
	return DNSRecord(t, namespace, name)(t)
}

func DNSRecord(t Test, namespace *corev1.Namespace, name string) func(g gomega.Gomega) *kuadrantv1.DNSRecord {
	return func(g gomega.Gomega) *kuadrantv1.DNSRecord {
		dnsRecord, err := t.Client().Kuadrant().Cluster(logicalcluster.From(namespace)).KuadrantV1().DNSRecords(namespace.Name).Get(t.Ctx(), name, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return dnsRecord
	}
}

func GetDNSRecords(t Test, namespace *corev1.Namespace, labelSelector string) []kuadrantv1.DNSRecord {
	t.T().Helper()
	return DNSRecords(t, namespace, labelSelector)(t)
}

func DNSRecords(t Test, namespace *corev1.Namespace, labelSelector string) func(g gomega.Gomega) []kuadrantv1.DNSRecord {
	return func(g gomega.Gomega) []kuadrantv1.DNSRecord {
		dnsRecords, err := t.Client().Kuadrant().Cluster(logicalcluster.From(namespace)).KuadrantV1().DNSRecords(namespace.Name).List(t.Ctx(), metav1.ListOptions{LabelSelector: labelSelector})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return dnsRecords.Items
	}
}

func DNSRecordEndpoints(record *kuadrantv1.DNSRecord) []*kuadrantv1.Endpoint {
	return record.Spec.Endpoints
}

func DNSRecordEndpointsCount(record *kuadrantv1.DNSRecord) int {
	return len(DNSRecordEndpoints(record))
}

func DNSRecordCondition(zoneID, condition string) func(record *kuadrantv1.DNSRecord) *kuadrantv1.DNSZoneCondition {
	return func(record *kuadrantv1.DNSRecord) *kuadrantv1.DNSZoneCondition {
		for _, z := range record.Status.Zones {
			if z.DNSZone.ID != zoneID {
				continue
			}
			for _, c := range z.Conditions {
				if c.Type == condition {
					return &c
				}
			}
			return nil
		}
		return nil
	}
}

func DNSRecordToIngressCertReady(t Test, namespace *corev1.Namespace, name string) func(dnsrecord *kuadrantv1.DNSRecord) string {
	return func(dnsrecord *kuadrantv1.DNSRecord) string {
		ing := GetIngress(t, namespace, dnsrecord.Name)
		return ing.Annotations[traffic.ANNOTATION_CERTIFICATE_STATE]
	}
}

func DNSRecordToRouteCertReady(t Test, namespace *corev1.Namespace, name string) func(dnsrecord *kuadrantv1.DNSRecord) string {
	return func(dnsrecord *kuadrantv1.DNSRecord) string {
		route, err := GetRoute(t, namespace, dnsrecord.Name)
		t.Expect(err).NotTo(gomega.HaveOccurred())
		return route.Annotations[traffic.ANNOTATION_CERTIFICATE_STATE]
	}
}

type Record struct {
	TXT string `json:"TXT,omitempty"`
	A   string `json:"A,omitempty"`
}

type Zone []Record

func AddRecord(t Test, host string, record Record) error {
	currentZone, err := GetZone(t, host)
	if err != nil && !dns.IsNoSuchHostError(err) {
		return err
	}
	currentZone = append(currentZone, record)
	byteValue, err := json.Marshal(currentZone)
	if err != nil {
		return err
	}
	return setDNSRecord(t, host, string(byteValue))
}

func SetTXTRecord(t Test, host, value string) error {
	return AddRecord(t, host, Record{TXT: value})
}

func SetARecord(t Test, host, value string) error {
	return AddRecord(t, host, Record{A: value})
}

func GetZone(t Test, host string) (Zone, error) {
	cfg, err := t.Client().Core().Cluster(GLBCWorkspace).CoreV1().ConfigMaps(ConfigmapNamespace).Get(t.Ctx(), ConfigmapName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	values := cfg.Data
	results := Zone{}
	if values == nil {
		return results, nil
	}
	if value, ok := values[host]; ok {
		err = json.Unmarshal([]byte(value), &results)
		if err != nil {
			return nil, err
		}
		return results, nil
	}
	return results, dns.NoSuchHost
}

// setDNSRecord - do not call this directly - use SetTXTRecord or SetARecord
func setDNSRecord(t Test, key, value string) error {
	cfg, err := t.Client().Core().Cluster(GLBCWorkspace).CoreV1().ConfigMaps(ConfigmapNamespace).Get(t.Ctx(), ConfigmapName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	values := cfg.Data
	if values == nil {
		values = map[string]string{}
	}
	values[key] = value
	return setDNSRecords(t, values)
}

// setDNSRecords - do not call this directly - use SetTXTRecord or SetARecord
func setDNSRecords(t Test, values map[string]string) error {
	_, err := t.Client().Core().Cluster(GLBCWorkspace).CoreV1().ConfigMaps(ConfigmapNamespace).Apply(
		t.Ctx(),
		corev1apply.ConfigMap(ConfigmapName, ConfigmapNamespace).WithData(values),
		ApplyOptions,
	)

	return err
}
