package metadata

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func AddAnnotation(obj metav1.Object, key, value string, overwrite bool) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	for k, v := range annotations {
		if k == key {
			if v == value || overwrite {
				return
			}
		}
	}
	annotations[key] = value
	obj.SetAnnotations(annotations)
}

func RemoveAnnotation(obj metav1.Object, key string) {
	annotations := obj.GetAnnotations()
	for k := range annotations {
		if k == key {
			delete(annotations, key)
			obj.SetAnnotations(annotations)
			return
		}
	}
}

type AnnotationPredicate func(key, value string) bool

func KeyPredicate(predicate func(key string) bool) AnnotationPredicate {
	return func(key, _ string) bool {
		return predicate(key)
	}
}

// CopyAnnotation copies an annotation with key `key` from `fromObj` into `toObj`
// Returns `true` if the annotation was found and copied, `false` otherwise
func CopyAnnotation(fromObj, toObj metav1.Object, key string) bool {
	return CopyAnnotationsPredicate(fromObj, toObj, func(eachKey, value string) bool {
		return eachKey == key
	})
}

// CopyAnnotationsPredicate copies any annotation from fromObj into toObj annotations
// that fullfils the given predicate. Returns true if at least one annotation was
// copied
func CopyAnnotationsPredicate(fromObj, toObj metav1.Object, predicate AnnotationPredicate) bool {
	fromObjAnnotations := fromObj.GetAnnotations()
	if fromObjAnnotations == nil {
		return false
	}

	toObjAnnotations := toObj.GetAnnotations()
	if toObjAnnotations == nil {
		toObjAnnotations = map[string]string{}
		toObj.SetAnnotations(toObjAnnotations)
	}

	copied := false
	for key, value := range fromObjAnnotations {
		if !predicate(key, value) {
			continue
		}

		toObjAnnotations[key] = value
		copied = true
	}

	return copied
}
