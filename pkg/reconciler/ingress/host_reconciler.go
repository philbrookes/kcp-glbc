package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kcp-dev/logicalcluster/v2"
	v1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
	kuadrantclientv1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"

	"github.com/go-logr/logr"
	"github.com/rs/xid"
	networkingv1 "k8s.io/api/networking/v1"
)

type hostReconciler struct {
	managedDomain      string
	log                logr.Logger
	customHostsEnabled bool
	kuadrantClient     kuadrantclientv1.ClusterInterface
}

func (r *hostReconciler) reconcile(ctx context.Context, ingress *networkingv1.Ingress) (reconcileStatus, error) {
	if ingress.Annotations == nil || ingress.Annotations[ANNOTATION_HCG_HOST] == "" {
		// Let's assign it a global hostname if any
		generatedHost := fmt.Sprintf("%s.%s", xid.New(), r.managedDomain)
		if ingress.Annotations == nil {
			ingress.Annotations = map[string]string{}
		}
		ingress.Annotations[ANNOTATION_HCG_HOST] = generatedHost
		//we need this host set and saved on the ingress before we go any further so force an update
		// if this is not saved we end up with a new host and the certificate can have the wrong host
		return reconcileStatusStop, nil
	}
	if r.customHostsEnabled {
		return r.processCustomHosts(ctx, ingress)
	}
	return r.replaceCustomHosts(ingress)
}

func (r *hostReconciler) replaceCustomHosts(ingress *networkingv1.Ingress) (reconcileStatus, error) {
	//once the annotation is definintely saved continue on
	managedHost := ingress.Annotations[ANNOTATION_HCG_HOST]
	var customHosts []string
	for i, rule := range ingress.Spec.Rules {
		if rule.Host != managedHost {
			ingress.Spec.Rules[i].Host = managedHost
			customHosts = append(customHosts, rule.Host)
		}
	}
	// clean up replaced hosts from the tls list
	removeHostsFromTLS(customHosts, ingress)

	if len(customHosts) > 0 {
		ingress.Annotations[ANNOTATION_HCG_CUSTOM_HOST_REPLACED] = fmt.Sprintf(" replaced custom hosts %v to the glbc host due to custom host policy not being allowed",
			customHosts)
	}

	return reconcileStatusContinue, nil
}

func (r *hostReconciler) processCustomHosts(ctx context.Context, ingress *networkingv1.Ingress) (reconcileStatus, error) {
	dvs, err := r.kuadrantClient.Cluster(logicalcluster.From(ingress)).KuadrantV1().DomainVerifications().List(ctx, metav1.ListOptions{})
	if err != nil {
		return reconcileStatusContinue, err
	}

	return doProcessCustomHostValidation(dvs, ingress)
}

func doProcessCustomHostValidation(dvs *v1.DomainVerificationList, ingress *networkingv1.Ingress) (reconcileStatus, error) {
	if ingress.Annotations == nil {
		ingress.Annotations = map[string]string{}
	}

	// Ensure the custom hosts replaced annotation is deleted, in case
	// the custom hosts feature was previously disabled
	delete(ingress.Annotations, ANNOTATION_HCG_CUSTOM_HOST_REPLACED)

	generatedHost, ok := ingress.Annotations[ANNOTATION_HCG_HOST]
	if !ok || generatedHost == "" {
		return reconcileStatusContinue, fmt.Errorf("generated host is empty for ingress: '%v/%v'", ingress.Namespace, ingress.Name)
	}

	var hosts []string

	preservedRules := make([]networkingv1.IngressRule, 0)

	// map[Custom domain] => Index of the generated rule for the domain
	generatedRules := map[string]int{}
	i := 0

	currentGeneratedRules := map[string]int{}
	// If the annotation has already been set, start by preserving the
	// current generated rules
	if annotationValue, ok := ingress.Annotations[GeneratedRulesAnnotation]; ok {
		if err := json.Unmarshal([]byte(annotationValue), &currentGeneratedRules); err != nil {
			return reconcileStatusContinue, err
		}

		for host, ruleIndex := range currentGeneratedRules {
			rule := ingress.Spec.Rules[ruleIndex].DeepCopy()
			preservedRules = append(preservedRules, *rule)
			generatedRules[host] = i
			i++
		}
	}

	// Create a generated rule from each custom domain
	for _, rule := range ingress.Spec.Rules {
		// ignore any rules for generated hosts (these are recalculated later)
		if rule.Host == generatedHost {
			continue
		}

		dv := findDomainVerification(rule.Host, dvs.Items)

		// check against domainverification status
		if dv != nil && dv.Status.Verified {
			preservedRules = append(preservedRules, rule)
			i++
		} else if strings.TrimSpace(rule.Host) != "" {
			hosts = append(hosts, rule.Host)
		}

		// if the host already has a generated rule, skip it
		if _, ok := generatedRules[rule.Host]; ok {
			continue
		}

		// Duplicate the rule and keep the association host => index
		generatedHostRule := *rule.DeepCopy()
		generatedHostRule.Host = generatedHost
		preservedRules = append(preservedRules, generatedHostRule)
		generatedRules[rule.Host] = i
		i++
	}
	ingress.Spec.Rules = preservedRules

	// Save the generated rules association in the annotation
	generatedHosts, err := json.Marshal(generatedRules)
	if err != nil {
		return reconcileStatusContinue, err
	}
	ingress.Annotations[GeneratedRulesAnnotation] = string(generatedHosts)

	// Ensure that every custom domain that has been verified is preserved
GeneratedRulesLoop:
	for host, generatedIndex := range generatedRules {
		// Validate the index hasn't been corrupted
		if generatedIndex < 0 || generatedIndex >= len(ingress.Spec.Rules) {
			continue
		}

		// If the domain hasn't been verified, do not include the rule
		dv := findDomainVerification(host, dvs.Items)
		if dv == nil || !dv.Status.Verified {
			continue
		}

		// If the rule already has been included, skip it
		for _, rule := range ingress.Spec.Rules {
			if rule.Host == host {
				continue GeneratedRulesLoop
			}
		}

		// Create a copy of the generated rule and set the custom host
		generatedRule := ingress.Spec.Rules[generatedIndex]
		customDomainRule := generatedRule.DeepCopy()
		customDomainRule.Host = host

		ingress.Spec.Rules = append(ingress.Spec.Rules, *customDomainRule)
	}

	// clean up replaced hosts from the tls list
	removeHostsFromTLS(hosts, ingress)

	return reconcileStatusContinue, nil
}

func findDomainVerification(host string, dvs []v1.DomainVerification) *v1.DomainVerification {
	if strings.TrimSpace(host) == "" {
		return nil
	}

	for _, dv := range dvs {
		if hostMatches(host, dv.Spec.Domain) {
			return &dv
		}
	}

	return nil
}

func hostMatches(host, domain string) bool {
	parentHostParts := strings.SplitN(host, ".", 2)

	if len(parentHostParts) < 2 {
		return false
	}

	if parentHostParts[1] == domain {
		return true
	}

	return hostMatches(parentHostParts[1], domain)
}
