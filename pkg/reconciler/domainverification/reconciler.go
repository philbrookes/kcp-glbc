package domainverification

import (
	"context"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/kcp-dev/apimachinery/pkg/logicalcluster"
	v1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilserrors "k8s.io/apimachinery/pkg/util/errors"
)

type reconcileStatus int

const (
	reconcileStatusStop reconcileStatus = iota
	reconcileStatusContinue
)

type reconciler interface {
	reconcile(ctx context.Context, dv *v1.DomainVerification) (reconcileStatus, error)
}

type dnsVerifier interface {
	TxtRecordExists(ctx context.Context, domain string, value string) (bool, error)
}

type domainVerificationStatus struct {
	updateStatus func(ctx context.Context, dv *v1.DomainVerification) error
	dnsVerifier  dnsVerifier
	requeAfter   func(item interface{}, duration time.Duration)
}

// reconcile ensures the status is as expected
func (dsr *domainVerificationStatus) reconcile(ctx context.Context, dv *v1.DomainVerification) (reconcileStatus, error) {
	var status = reconcileStatusContinue
	var errs error
	verified, ensureErr := dsr.ensureDomainVerificationStatus(ctx, dv)
	if ensureErr != nil {
		errs = multierror.Append(errs, ensureErr)
		status = reconcileStatusStop
	}
	updateErr := dsr.updateStatus(ctx, dv)
	if updateErr != nil {
		errs = multierror.Append(errs, updateErr)
		status = reconcileStatusStop
	}
	if !verified {
		status = reconcileStatusStop
		dsr.requeAfter(dv, recheckDefault)
	}

	return status, errs
}
func (dsr *domainVerificationStatus) ensureDomainVerificationStatus(ctx context.Context, domainVerification *v1.DomainVerification) (bool, error) {
	// default status
	domainVerification.Status.Verified = false
	//consider moving to mutating webhook that will create this resource
	if domainVerification.Status.Token == "" {
		domainVerification.Status.Token = domainVerification.GetToken()
		return false, nil
	}

	// check if this domain is already verified. Trusting the webhook to ensure this is only updated by our controller
	if domainVerification.Status.Verified {
		return true, nil
	}
	domainVerification.Status.LastChecked = metav1.Now()
	// check DNS to see can we validate
	exists, err := dsr.dnsVerifier.TxtRecordExists(ctx, domainVerification.Spec.Domain, domainVerification.Status.Token)
	if err != nil || !exists {
		domainVerification.Status.Message = "domain verification was not successful"
		domainVerification.Status.NextCheck = metav1.NewTime(time.Now().Add(recheckDefault))
		return false, err
	}
	if exists {
		domainVerification.Status.Message = "domain verification was successful"
		domainVerification.Status.Verified = true
	}
	return exists, nil
}

func (c *Controller) reconcile(ctx context.Context, domainVerfication *v1.DomainVerification) error {
	reconcilers := []reconciler{
		&domainVerificationStatus{
			updateStatus: c.updateStatus,
			dnsVerifier:  c.dnsVerifier,
			requeAfter:   c.EnqueueAfter,
		},
	}

	var errs []error

	for _, r := range reconcilers {
		status, err := r.reconcile(ctx, domainVerfication)
		if err != nil {
			errs = append(errs, err)
		}
		if status == reconcileStatusStop {
			break
		}
	}
	return utilserrors.NewAggregate(errs)
}

func (c *Controller) updateStatus(ctx context.Context, dv *v1.DomainVerification) error {
	_, err := c.domainVerificationClient.Cluster(logicalcluster.From(dv)).KuadrantV1().DomainVerifications().UpdateStatus(ctx, dv, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}
