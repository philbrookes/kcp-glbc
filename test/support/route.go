package support

import (
	"encoding/json"
	"fmt"
	"strings"

	workload "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"
	"github.com/kcp-dev/logicalcluster/v2"
	"github.com/onsi/gomega"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	traffic "github.com/kuadrant/kcp-glbc/pkg/traffic"
)

var RouteResource = schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}

func TrafficRouteFromUnstructured(uRoute *unstructured.Unstructured) (*traffic.Route, error) {
	route := &routev1.Route{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(uRoute.Object, route)
	return traffic.NewRoute(route), err
}

func Route(t Test, namespace *corev1.Namespace, name string) func(g gomega.Gomega) *traffic.Route {
	return func(g gomega.Gomega) *traffic.Route {
		uRoute, err := t.Client().Dynamic().Cluster(logicalcluster.From(namespace)).Resource(RouteResource).Namespace(namespace.Name).Get(t.Ctx(), name, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())

		route, err := TrafficRouteFromUnstructured(uRoute)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return route
	}
}

func GetRoute(t Test, namespace *corev1.Namespace, name string) (*traffic.Route, error) {
	uRoute, err := t.Client().Dynamic().Cluster(logicalcluster.From(namespace)).Resource(RouteResource).Namespace(namespace.Name).Get(t.Ctx(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return TrafficRouteFromUnstructured(uRoute)
}

func OriginalSpecUnchanged(t Test, original traffic.Interface) func(i traffic.Interface) bool {
	t.T().Log("validating original spec is unchanged")
	return func(i traffic.Interface) bool {
		return equality.Semantic.DeepEqual(i.GetSpec(), original.GetSpec())
	}
}
func ValidateTransformedRoute(expectedSpec routev1.RouteSpec, transformed *traffic.Route) error {
	st := transformed.GetSyncTargets()
	for _, target := range st {
		// ensure each target has a transform value set and it is correct
		if _, ok := transformed.Annotations[workload.ClusterSpecDiffAnnotationPrefix+target]; !ok {
			return fmt.Errorf("expected a transformation for sync target " + target)
		}
		transforms := transformed.Annotations[workload.ClusterSpecDiffAnnotationPrefix+target]
		var patches []struct {
			Path  string      `json:"path"`
			Op    string      `json:"op"`
			Value interface{} `json:"value"`
		}
		if err := json.Unmarshal([]byte(transforms), &patches); err != nil {
			return fmt.Errorf("failed to unmarshal patch %s", err)
		}
		//ensure there is a rules and tls patch and they have the correct value
		hostPatch := false
		tlsPatch := false
		for _, p := range patches {
			if p.Path == "/host" {
				hostPatch = true
				host := ""
				b, err := json.Marshal(p.Value)
				if err != nil {
					return fmt.Errorf("failed to marshal host %s", err)
				}
				if err := json.Unmarshal(b, &host); err != nil {
					return err
				}
				if host != expectedSpec.Host {
					return fmt.Errorf("expected the host in the transform (%v) to match the rules in transformed route (%v)", host, expectedSpec.Host)
				}
			}
			if p.Path == "/tls" {
				tlsPatch = true
				tls := &routev1.TLSConfig{}
				b, err := json.Marshal(p.Value)
				if err != nil {
					return fmt.Errorf("failed to marshal tls %s", err)
				}
				if err := json.Unmarshal(b, tls); err != nil {
					return err
				}
				if !equality.Semantic.DeepEqual(tls, expectedSpec.TLS) {
					fmt.Printf("expected %v got %v ", expectedSpec.TLS, tls)
					return fmt.Errorf("expected the tls section in the transform to match the tls in transformed route")
				}
			}
		}
		if !hostPatch {
			return fmt.Errorf("expected to find a rules patch but one was missing")
		}
		if !tlsPatch {
			return fmt.Errorf("expected to find a tls patch but one was missing")
		}

	}
	return nil
}

// RouteTransformedSpec will look at the transforms applied and compare them to the expected spec.
func RouteTransformedSpec(test Test, expectedSpec routev1.RouteSpec) func(route *traffic.Route) bool {
	test.T().Log("Validating transformed spec for ingress")
	return func(route *traffic.Route) bool {
		if err := ValidateTransformedRoute(expectedSpec, route); err != nil {
			test.T().Log("transformed spec is not valid", err)
			return false

		}
		test.T().Log("transformed spec is valid")
		return true
	}
}
func GetDefaultRouteSpec(host, serviceName string, tls *routev1.TLSConfig) routev1.RouteSpec {
	return routev1.RouteSpec{
		Host: host,
		Path: "/",
		To: routev1.RouteTargetReference{
			Kind:   "Service",
			Name:   serviceName,
			Weight: nil,
		},
		TLS:            tls,
		WildcardPolicy: "",
	}
}

func RouteLBHostEqualToGeneratedHost(route *traffic.Route) bool {
	equals := true

	for _, i := range route.Status.Ingress {
		if i.Host != route.GetAnnotations()[traffic.ANNOTATION_HCG_HOST] {
			equals = false
		}
	}
	if !route.TMCEnabed() {
		return !equals
	}
	return equals
}

func RouteLoadBalancerIngresses(route *traffic.Route) []routev1.RouteIngress {
	for a, v := range route.Annotations {
		if strings.Contains(a, workload.InternalClusterStatusAnnotationPrefix) {
			routeStatus := routev1.RouteStatus{}
			err := json.Unmarshal([]byte(v), &routeStatus)
			if err != nil {
				return []routev1.RouteIngress{}
			}
			return routeStatus.Ingress
		}
	}
	return []routev1.RouteIngress{}

}
