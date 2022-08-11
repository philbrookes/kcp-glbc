// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	v1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned/typed/kuadrant/v1"
	rest "k8s.io/client-go/rest"
	testing "k8s.io/client-go/testing"
)

type FakeKuadrantV1 struct {
	*testing.Fake
}

func (c *FakeKuadrantV1) DNSRecords(namespace string) v1.DNSRecordInterface {
	return &FakeDNSRecords{c, namespace}
}

func (c *FakeKuadrantV1) DomainVerifications() v1.DomainVerificationInterface {
	return &FakeDomainVerifications{c}
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (c *FakeKuadrantV1) RESTClient() rest.Interface {
	var ret *rest.RESTClient
	return ret
}
