package traffic

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	certman "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/jetstack/cert-manager/pkg/apis/meta/v1"

	"github.com/kuadrant/kcp-glbc/pkg/_internal/log"
	"github.com/kuadrant/kcp-glbc/pkg/_internal/metadata"
	basereconciler "github.com/kuadrant/kcp-glbc/pkg/reconciler"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"

	"github.com/kcp-dev/logicalcluster/v2"

	"github.com/kuadrant/kcp-glbc/pkg/tls"
)

type CertificateReconciler struct {
	CreateCertificate    func(ctx context.Context, mapper tls.CertificateRequest) error
	DeleteCertificate    func(ctx context.Context, mapper tls.CertificateRequest) error
	GetCertificateSecret func(ctx context.Context, request tls.CertificateRequest) (*corev1.Secret, error)
	UpdateCertificate    func(ctx context.Context, request tls.CertificateRequest) error
	GetCertificateStatus func(ctx context.Context, request tls.CertificateRequest) (tls.CertStatus, error)
	CopySecret           func(ctx context.Context, workspace logicalcluster.Name, namespace string, s *corev1.Secret) error
	GetSecret            func(ctx context.Context, name, namespace string, cluster logicalcluster.Name) (*corev1.Secret, error)
	DeleteSecret         func(ctx context.Context, workspace logicalcluster.Name, namespace, name string) error
	Log                  logr.Logger
}

type Enqueue bool

func (r *CertificateReconciler) GetName() string {
	return "Certificate Reconciler"
}

// CertificateSecretFilter
func CertificateSecretFilter(obj interface{}) bool {
	s, ok := obj.(*corev1.Secret)
	if !ok {
		return false
	}
	if _, ok := s.Labels[basereconciler.LABEL_HCG_MANAGED]; !ok {
		return false
	}
	if s.Annotations != nil {
		if _, ok := s.Annotations[tls.TlsIssuerAnnotation]; ok {
			if _, ok := s.Annotations[ANNOTATION_TRAFFIC_KEY]; ok {
				return true
			}
		}
	}
	return false
}

// CertificateUpdatedHandler is used as an event handler for certificates
func CertificateUpdatedHandler(oldCert, newCert *certman.Certificate) Enqueue {
	issuer := newCert.Spec.IssuerRef

	revision := func(c *certman.Certificate) int {
		if c.Status.Revision != nil {
			return *c.Status.Revision
		}
		return 0
	}
	// certificate moved from not ready to ready so a new certificate is ready
	if !certificateReady(oldCert) && certificateReady(newCert) {
		// if it is the first cert decrement the counter
		//sometimes we see the new cert move to ready before the revision is incremented. So it can be at revision 0
		if revision(newCert) == 1 || revision(newCert) == 0 {
			log.Logger.Info("Incrementing successful certificate request metric for issuer: " + issuer.Name + ", domain: " + newCert.Spec.CommonName)
			TlsCertificateRequestCount.WithLabelValues(issuer.Name).Dec()
			TlsCertificateRequestTotal.WithLabelValues(issuer.Name, resultLabelSucceeded).Inc()
			TlsCertificateIssuanceDuration.
				WithLabelValues(issuer.Name, resultLabelSucceeded).
				Observe(time.Since(newCert.CreationTimestamp.Time).Seconds())
		}
		return Enqueue(true)
	}

	var hasFailed = func(cert *certman.Certificate) bool {
		if cert.Status.LastFailureTime != nil && cert.Status.RenewalTime == nil {
			return true
		}
		if newCert.Status.LastFailureTime != nil && newCert.Status.RenewalTime != nil {
			// this is a renewal that has failed
			if newCert.Status.LastFailureTime.Time.After(newCert.Status.RenewalTime.Time) {
				return true
			}
		}
		return false
	}
	// error case
	if !certificateReady(newCert) {
		//state transitioned to failure increment counter
		if hasFailed(newCert) && !hasFailed(oldCert) {
			TlsCertificateRequestErrors.WithLabelValues(issuer.Name, resultLabelFailed).Inc()
		}
	}

	return Enqueue(false)
}

// CertificateAddedHandler is used as an event handler for certificates
func CertificateAddedHandler(cert *certman.Certificate) {
	issuer := cert.Spec.IssuerRef
	// new cert added so increment
	TlsCertificateRequestCount.WithLabelValues(issuer.Name).Inc()
}

// CertificateDeletedHandler is used as an event handler
func CertificateDeletedHandler(cert *certman.Certificate) {
	issuer := cert.Spec.IssuerRef
	if !certificateReady(cert) {
		// cert never got to ready but is now being deleted so decerement counter
		TlsCertificateRequestCount.WithLabelValues(issuer.Name).Dec()
	}
}

func certificateReady(cert *certman.Certificate) bool {
	for _, cond := range cert.Status.Conditions {
		if cond.Type == certman.CertificateConditionReady {
			return cond.Status == cmmeta.ConditionTrue
		}
	}
	return false
}

func CertificateName(accessor Interface) string {
	// Removes chars which are invalid characters for cert manager certificate names. RFC 1123 subdomain must consist of
	// lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character
	clusterName := logicalcluster.From(accessor)
	if clusterName == logicalcluster.New("") {
		clusterName = logicalcluster.New("k8s")
	}
	return strings.ToLower(strings.ReplaceAll(fmt.Sprintf("%s-%s-%s-%s", clusterName, accessor.GetKind(), accessor.GetNamespace(), accessor.GetName()), ":", ""))
}

// TLSSecretName returns the name for the secret in the end user namespace
func TLSSecretName(accessor Interface) string {
	return strings.ToLower(fmt.Sprintf("hcg-tls-%s-%s", accessor.GetKind(), accessor.GetName()))
}

func (r *CertificateReconciler) Reconcile(ctx context.Context, accessor Interface) (ReconcileStatus, error) {
	annotations := map[string]string{}
	labels := map[string]string{
		basereconciler.LABEL_HCG_MANAGED: "true",
	}
	key, err := cache.MetaNamespaceKeyFunc(accessor)
	if err != nil {
		return ReconcileStatusStop, err
	}

	tlsSecretName := TLSSecretName(accessor)
	//set the accessor key on the certificate to help us with locating the accessor later
	annotations[ANNOTATION_TRAFFIC_KEY] = key
	annotations[ANNOTATION_TRAFFIC_KIND] = accessor.GetKind()
	certReq := tls.CertificateRequest{
		Name:        CertificateName(accessor),
		Labels:      labels,
		Annotations: annotations,
	}

	if accessor.GetDeletionTimestamp() != nil && !accessor.GetDeletionTimestamp().IsZero() {
		if err := r.DeleteCertificate(ctx, certReq); err != nil && !strings.Contains(err.Error(), "not found") {
			r.Log.Info("error deleting certificate")
			return ReconcileStatusStop, err
		}
		//TODO remove once owner refs work in kcp
		if err := r.DeleteSecret(ctx, logicalcluster.From(accessor), accessor.GetNamespace(), tlsSecretName); err != nil && !strings.Contains(err.Error(), "not found") {
			r.Log.Info("error deleting certificate secret")
			return ReconcileStatusStop, err
		}
		return ReconcileStatusContinue, nil
	}

	managedHost := accessor.GetHCGHost()
	if managedHost == "" {
		return ReconcileStatusStop, ErrGeneratedHostMissing
	}
	certReq.Host = managedHost

	err = r.CreateCertificate(ctx, certReq)
	if err != nil && !errors.IsAlreadyExists(err) {
		return ReconcileStatusStop, fmt.Errorf("certificate reconciler: error creating certificate, error: %v", err.Error())
	}
	metadata.AddAnnotation(accessor, ANNOTATION_CERTIFICATE_STATE, "requested")
	if errors.IsAlreadyExists(err) {
		// get certificate secret and copy
		secret, err := r.GetCertificateSecret(ctx, certReq)
		if err != nil {
			if tls.IsCertNotReadyErr(err) {
				// cetificate not ready so update the status and allow it continue Reconcile. Will be requeued once certificate becomes ready
				status, err := r.GetCertificateStatus(ctx, certReq)
				if err != nil {
					return ReconcileStatusStop, fmt.Errorf("certificate reconciler: error getting certificate status error: %v", err.Error())
				}
				metadata.AddAnnotation(accessor, ANNOTATION_CERTIFICATE_STATE, string(status))
				return ReconcileStatusContinue, nil
			}
			return ReconcileStatusStop, fmt.Errorf("certificate reconciler: error getting certificate secret error: %v", err.Error())
		}
		metadata.AddAnnotation(accessor, ANNOTATION_CERTIFICATE_STATE, "ready") // todo remove hardcoded string
		//copy over the secret to the accessor namesapce
		scopy := secret.DeepCopy()
		scopy.SetOwnerReferences([]metav1.OwnerReference{
			{
				APIVersion:         networkingv1.SchemeGroupVersion.String(),
				Kind:               accessor.GetKind(),
				Name:               accessor.GetName(),
				UID:                accessor.GetUID(),
				Controller:         pointer.Bool(true),
				BlockOwnerDeletion: pointer.Bool(true),
			},
		})

		scopy.Namespace = accessor.GetNamespace()
		scopy.Name = tlsSecretName
		if err := r.CopySecret(ctx, logicalcluster.From(accessor), accessor.GetNamespace(), scopy); err != nil {
			return ReconcileStatusStop, fmt.Errorf("certificate reconciler: error copying secret error: %v", err.Error())
		}
	}
	// set tls setting on the accessor
	certSecret, err := r.GetSecret(ctx, tlsSecretName, accessor.GetNamespace(), accessor.GetLogicalCluster())
	if err != nil {
		// don't proceed until the secret is present. We want TLS to be available before we make the ingress accessible via DNS
		return ReconcileStatusStop, fmt.Errorf("certificate reconciler: error getting secret to set on accessor error: %v", err.Error())
	}
	accessor.AddTLS(certReq.Host, certSecret)

	return ReconcileStatusContinue, nil
}
