package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	v1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
)

type hostResult struct {
	Status  reconcileStatus
	Err     error
	Ingress *networkingv1.Ingress
}

func TestReconcileHost(t *testing.T) {
	ingress := func(rules []networkingv1.IngressRule, tls []networkingv1.IngressTLS) *networkingv1.Ingress {
		i := &networkingv1.Ingress{
			Spec: networkingv1.IngressSpec{
				Rules: rules,
			},
		}
		i.Spec.TLS = tls

		return i
	}

	var buildResult = func(r reconciler, i *networkingv1.Ingress) hostResult {
		status, err := r.reconcile(context.TODO(), i)
		return hostResult{
			Status:  status,
			Err:     err,
			Ingress: i,
		}
	}
	var mangedDomain = "test.com"

	var commonValidation = func(hr hostResult, expectedStatus reconcileStatus) error {
		if hr.Status != expectedStatus {
			return fmt.Errorf("unexpected status ")
		}
		if hr.Err != nil {
			return fmt.Errorf("unexpected error from reconcile : %s", hr.Err)
		}
		_, ok := hr.Ingress.Annotations[ANNOTATION_HCG_HOST]
		if !ok {
			return fmt.Errorf("expected annotation %s to be set", ANNOTATION_HCG_HOST)
		}

		return nil
	}

	cases := []struct {
		Name     string
		Ingress  func() *networkingv1.Ingress
		Validate func(hr hostResult) error
	}{
		{
			Name: "test managed host generated for empty host field",
			Ingress: func() *networkingv1.Ingress {
				return ingress([]networkingv1.IngressRule{{
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{},
					},
				}, {
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{},
					},
				}}, []networkingv1.IngressTLS{})
			},
			Validate: func(hr hostResult) error {
				return commonValidation(hr, reconcileStatusStop)
			},
		},
		{
			Name: "test custom host replaced with generated managed host",
			Ingress: func() *networkingv1.Ingress {
				i := ingress([]networkingv1.IngressRule{{
					Host: "api.example.com",
				}}, []networkingv1.IngressTLS{})
				i.Annotations = map[string]string{ANNOTATION_HCG_HOST: "123.test.com"}
				return i
			},
			Validate: func(hr hostResult) error {
				err := commonValidation(hr, reconcileStatusContinue)
				if err != nil {
					return err
				}
				if _, ok := hr.Ingress.Annotations[ANNOTATION_HCG_CUSTOM_HOST_REPLACED]; !ok {
					return fmt.Errorf("expected the custom host annotation to be present")
				}
				for _, r := range hr.Ingress.Spec.Rules {
					if r.Host != hr.Ingress.Annotations[ANNOTATION_HCG_HOST] {
						return fmt.Errorf("expected the host to be set to %s", hr.Ingress.Annotations[ANNOTATION_HCG_HOST])
					}
				}
				return nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			reconciler := &hostReconciler{
				managedDomain: mangedDomain,
			}

			if err := tc.Validate(buildResult(reconciler, tc.Ingress())); err != nil {
				t.Fatalf("fail: %s", err)
			}

		})
	}
}

func TestProcessCustomHostValidation(t *testing.T) {
	testCases := []struct {
		name                   string
		ingress                *networkingv1.Ingress
		domainVerifications    *v1.DomainVerificationList
		expectedGeneratedRules map[string]int
		expectedRules          []networkingv1.IngressRule
	}{
		{
			name: "Empty host",
			ingress: &networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "ingress",
					Annotations: map[string]string{
						ANNOTATION_HCG_HOST: "generated.host.net",
					},
				},
				Spec: networkingv1.IngressSpec{
					Rules: []networkingv1.IngressRule{
						{
							Host: "",
							IngressRuleValue: networkingv1.IngressRuleValue{
								HTTP: &networkingv1.HTTPIngressRuleValue{
									Paths: []networkingv1.HTTPIngressPath{
										{
											Path: "/",
										},
									},
								},
							},
						},
					},
				},
			},
			domainVerifications: &v1.DomainVerificationList{},
			expectedGeneratedRules: map[string]int{
				"": 0,
			},
			expectedRules: []networkingv1.IngressRule{
				{
					Host: "generated.host.net",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path: "/",
								},
							},
						},
					},
				},
			},
		},
		{
			name: "Custom host verified",
			ingress: &networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "ingress",
					Annotations: map[string]string{
						ANNOTATION_HCG_HOST: "generated.host.net",
					},
				},
				Spec: networkingv1.IngressSpec{
					Rules: []networkingv1.IngressRule{
						{
							Host: "test.pb-custom.hcpapps.net",
							IngressRuleValue: networkingv1.IngressRuleValue{
								HTTP: &networkingv1.HTTPIngressRuleValue{
									Paths: []networkingv1.HTTPIngressPath{
										{
											Path: "/path",
										},
									},
								},
							},
						},
					},
				},
			},
			domainVerifications: &v1.DomainVerificationList{
				Items: []v1.DomainVerification{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "pb-custom.hcpapps.net",
						},
						Spec: v1.DomainVerificationSpec{
							Domain: "pb-custom.hcpapps.net",
						},
						Status: v1.DomainVerificationStatus{
							Verified: true,
						},
					},
				},
			},
			expectedGeneratedRules: map[string]int{
				"test.pb-custom.hcpapps.net": 1,
			},
			expectedRules: []networkingv1.IngressRule{
				{
					Host: "test.pb-custom.hcpapps.net",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path: "/path",
								},
							},
						},
					},
				},
				{
					Host: "generated.host.net",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path: "/path",
								},
							},
						},
					},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ingress := testCase.ingress.DeepCopy()

			if _, err := doProcessCustomHostValidation(
				testCase.domainVerifications,
				ingress,
			); err != nil {
				t.Fatal(err)
			}

			// Assert the expected generated rules matches the
			// annotation
			if testCase.expectedGeneratedRules != nil {
				annotation, ok := ingress.Annotations[GeneratedRulesAnnotation]
				if !ok {
					t.Fatalf("expected GeneratedRulesAnnotation to be set")
				}

				generatedRules := map[string]int{}
				if err := json.Unmarshal(
					[]byte(annotation),
					&generatedRules,
				); err != nil {
					t.Fatalf("invalid format on GeneratedRulesAnnotation: %v", err)
				}

				for domain, index := range testCase.expectedGeneratedRules {
					if generatedRules[domain] != index {
						t.Errorf("expected generated rule for domain %s to be in index %d, but got %d",
							domain,
							index,
							generatedRules[domain],
						)
					}
				}
			}

			// Assert the reconciled rules match the expected rules
			for i, expectedRule := range testCase.expectedRules {
				rule := ingress.Spec.Rules[i]

				if !equality.Semantic.DeepEqual(expectedRule, rule) {
					t.Errorf("expected rule does not match rule at index %d", i)
				}
			}
		})
	}
}
