package certificate

import (
	"context"
	certman "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	certmanv1 "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	"github.com/kuadrant/kcp-glbc/pkg/util/metadata"
	"strconv"
	"time"
)

const (
	RequestTimeAnnotation = "kuadrant.dev/certificate-request-time"
)

type Status string

func (c *Controller) reconcile(ctx context.Context, certificate *certman.Certificate) error {
	now := time.Now().Unix()
	metadata.AddAnnotation(certificate, RequestTimeAnnotation, strconv.FormatInt(now, 10), false)
	return nil
}

func PendingTime(certificate *certman.Certificate) int64 {
	now := time.Now().Unix()
	requestTime, err := strconv.Atoi(certificate.Annotations[RequestTimeAnnotation])
	if err != nil {
		return -1
	}
	return now - int64(requestTime)
}

func IsReady(certificate *certman.Certificate) bool {
	for _, condition := range certificate.Status.Conditions {
		if condition.Type == certman.CertificateConditionReady {
			return condition.Status == certmanv1.ConditionTrue
		}
	}
	return false
}
