package dns

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type resolver interface {
	LookupTXT(ctx context.Context, domain string) ([]string, error)
}

func NewVerifier(resolver resolver) *verifier {
	return &verifier{
		resolver: resolver,
	}
}

type verifier struct {
	resolver resolver
}

func (v *verifier) TxtRecordExists(ctx context.Context, domain, value string) (bool, error) {
	values, err := v.resolver.LookupTXT(ctx, domain)
	if err != nil {
		return false, errors.New(fmt.Sprintf("error looking for TXT record on '%v': %v", domain, err))
	}

	for _, txtValue := range values {
		if strings.TrimSpace(txtValue) == strings.TrimSpace(value) {
			return true, nil
		}
	}
	return false, nil
}
