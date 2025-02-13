// Code generated by informer-gen. DO NOT EDIT.

package v1

import (
	internalinterfaces "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/informers/externalversions/internalinterfaces"
)

// Interface provides access to all the informers in this group version.
type Interface interface {
	// DNSRecords returns a DNSRecordInformer.
	DNSRecords() DNSRecordInformer
	// DomainVerifications returns a DomainVerificationInformer.
	DomainVerifications() DomainVerificationInformer
}

type version struct {
	factory          internalinterfaces.SharedInformerFactory
	namespace        string
	tweakListOptions internalinterfaces.TweakListOptionsFunc
}

// New returns a new Interface.
func New(f internalinterfaces.SharedInformerFactory, namespace string, tweakListOptions internalinterfaces.TweakListOptionsFunc) Interface {
	return &version{factory: f, namespace: namespace, tweakListOptions: tweakListOptions}
}

// DNSRecords returns a DNSRecordInformer.
func (v *version) DNSRecords() DNSRecordInformer {
	return &dNSRecordInformer{factory: v.factory, namespace: v.namespace, tweakListOptions: v.tweakListOptions}
}

// DomainVerifications returns a DomainVerificationInformer.
func (v *version) DomainVerifications() DomainVerificationInformer {
	return &domainVerificationInformer{factory: v.factory, tweakListOptions: v.tweakListOptions}
}
