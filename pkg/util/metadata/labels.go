package metadata

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
)

func HasLabel(obj metav1.Object, key string) bool {
	labels := obj.GetLabels()
	if labels == nil {
		return false
	}
	_, ok := labels[key]
	return ok
}

func HasLabelsContaining(obj metav1.Object, key string) (bool, map[string]string) {
	matches := map[string]string{}
	labels := obj.GetLabels()
	if labels == nil {
		return false, matches
	}

	for k, label := range labels {
		if strings.Contains(k, key) {
			matches[k] = label
		}
	}
	return len(matches) > 0, matches
}
