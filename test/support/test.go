/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package support

import (
	"context"
	"sync"
	"testing"

	"github.com/kcp-dev/kcp/pkg/apis/scheduling/v1alpha1"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1"
	tenancyv1beta1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1beta1"
	workloadv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"
	"github.com/kcp-dev/logicalcluster/v2"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/apis/kuadrant/v1"
)

type Test interface {
	T() *testing.T
	Ctx() context.Context
	Client() Client

	gomega.Gomega

	CreateGLBCAPIBindings(*tenancyv1beta1.Workspace, logicalcluster.Name, string)
	CreatePlacements(workspace *tenancyv1beta1.Workspace)
	NewAPIBinding(name string, options ...Option) *apisv1alpha1.APIBinding
	NewSyncTarget(name string, options ...Option) *workloadv1alpha1.SyncTarget
	NewTestNamespace(...Option) *corev1.Namespace
	NewTestWorkspace() *tenancyv1beta1.Workspace
}

type Option interface {
	applyTo(interface{}) error
}

type errorOption func(to interface{}) error

func (o errorOption) applyTo(to interface{}) error {
	return o(to)
}

var _ Option = errorOption(nil)

func With(t *testing.T) Test {
	ctx := context.Background()
	if deadline, ok := t.Deadline(); ok {
		withDeadline, cancel := context.WithDeadline(ctx, deadline)
		t.Cleanup(cancel)
		ctx = withDeadline
	}

	return &T{
		WithT: gomega.NewWithT(t),
		t:     t,
		ctx:   ctx,
	}
}

type T struct {
	*gomega.WithT
	t      *testing.T
	ctx    context.Context
	client Client
	once   sync.Once
}

func (t *T) T() *testing.T {
	return t.t
}

func (t *T) Ctx() context.Context {
	return t.ctx
}

func (t *T) Client() Client {
	t.once.Do(func() {
		c, err := newTestClient()
		if err != nil {
			t.T().Fatalf("Error creating client: %v", err)
		}
		t.client = c
	})
	return t.client
}

func generatePlacement(name string, targetWorkspace *tenancyv1beta1.Workspace, locationSelectors, namespaceSelectors map[string]string) (*v1alpha1.Placement, error) {
	placement := &v1alpha1.Placement{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.PlacementSpec{
			LocationSelectors: []metav1.LabelSelector{{MatchLabels: locationSelectors}},
			LocationResource:  v1alpha1.GroupVersionResource{Group: "workload.kcp.dev", Resource: "synctargets", Version: "v1alpha1"},
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: namespaceSelectors},
			LocationWorkspace: TestOrganization.String(),
		},
	}

	err := InWorkspace(targetWorkspace).applyTo(placement)

	return placement, err
}

func (t *T) CreatePlacements(targetWorkspace *tenancyv1beta1.Workspace) {
	placement, err := generatePlacement(
		"glbc-ingresses",
		targetWorkspace,
		map[string]string{"kuadrant.dev/location": "glbc-ingresses"},
		map[string]string{"kuadrant.dev/cluster-type": "glbc-ingresses"},
	)
	if err != nil {
		t.Expect(err).NotTo(gomega.HaveOccurred())
	}

	_, err = t.Client().Kcp().Cluster(logicalcluster.From(placement)).SchedulingV1alpha1().Placements().Create(t.Ctx(), placement, metav1.CreateOptions{})
	if err != nil {
		t.Expect(err).NotTo(gomega.HaveOccurred())
	}
	placement, err = generatePlacement(
		"glbc-routes",
		targetWorkspace,
		map[string]string{"kuadrant.dev/location": "glbc-routes"},
		map[string]string{"kuadrant.dev/cluster-type": "glbc-routes"},
	)
	if err != nil {
		t.Expect(err).NotTo(gomega.HaveOccurred())
	}

	_, err = t.Client().Kcp().Cluster(logicalcluster.From(placement)).SchedulingV1alpha1().Placements().Create(t.Ctx(), placement, metav1.CreateOptions{})
	if err != nil {
		t.Expect(err).NotTo(gomega.HaveOccurred())
	}
}
func (t *T) CreateGLBCAPIBindings(targetWorkspace *tenancyv1beta1.Workspace, glbcWorkspace logicalcluster.Name, glbcExportName string) {

	t.T().Logf("Binding to kubernetes APIExport from %s to %s", glbcWorkspace.String(), targetWorkspace.Name)
	// Bind compute workspace APIs
	binding := t.NewAPIBinding("kubernetes", WithComputeServiceExport(glbcWorkspace), InWorkspace(targetWorkspace))

	// Wait until the APIBinding is actually in bound phase
	t.Eventually(APIBinding(t, logicalcluster.From(binding).String(), binding.Name)).
		Should(gomega.WithTransform(APIBindingPhase, gomega.Equal(apisv1alpha1.APIBindingPhaseBound)))

	// Wait until the APIs are imported into the test targetWorkspace
	t.Eventually(HasImportedAPIs(t, targetWorkspace,
		corev1.SchemeGroupVersion.WithKind("Service"),
		appsv1.SchemeGroupVersion.WithKind("Deployment"),
		networkingv1.SchemeGroupVersion.WithKind("Ingress"),
	)).Should(gomega.BeTrue())

	binding = GetAPIBinding(t, logicalcluster.From(binding).String(), binding.Name)
	kubeIdentityHash := binding.Status.BoundResources[0].Schema.IdentityHash

	t.T().Logf("Binding to %s APIExport from %s to %s", glbcExportName, glbcWorkspace.String(), targetWorkspace.Name)
	// Import GLBC APIs
	binding = t.NewAPIBinding("glbc", WithExportReference(glbcWorkspace, glbcExportName), WithGLBCAcceptablePermissionClaims(kubeIdentityHash), InWorkspace(targetWorkspace))

	// Wait until the APIBinding is actually in bound phase
	t.Eventually(APIBinding(t, logicalcluster.From(binding).String(), binding.Name)).
		Should(gomega.WithTransform(APIBindingPhase, gomega.Equal(apisv1alpha1.APIBindingPhaseBound)))

	// And check the APIs are imported into the test workspace
	t.Expect(HasImportedAPIs(t, targetWorkspace, kuadrantv1.SchemeGroupVersion.WithKind("DNSRecord"))(t)).
		Should(gomega.BeTrue())
	t.Expect(HasImportedAPIs(t, targetWorkspace, kuadrantv1.SchemeGroupVersion.WithKind("DomainVerification"))(t)).
		Should(gomega.BeTrue())
}

func (t *T) NewTestWorkspace() *tenancyv1beta1.Workspace {
	workspace := createTestWorkspace(t)
	t.T().Cleanup(func() {
		deleteTestWorkspace(t, workspace)
	})
	t.T().Logf("Creating workspace %v:%v", TestOrganization, workspace.Name)
	t.Eventually(Workspace(t, workspace.Name)).
		Should(gomega.WithTransform(WorkspacePhase, gomega.Equal(tenancyv1alpha1.ClusterWorkspacePhaseReady)))
	return workspace
}

func (t *T) NewAPIBinding(name string, options ...Option) *apisv1alpha1.APIBinding {
	return createAPIBinding(t, name, options...)
}

func (t *T) NewTestNamespace(options ...Option) *corev1.Namespace {
	namespace := createTestNamespace(t, options...)
	t.T().Cleanup(func() {
		deleteTestNamespace(t, namespace)
	})
	return namespace
}

func (t *T) NewSyncTarget(name string, options ...Option) *workloadv1alpha1.SyncTarget {
	workloadCluster, cleanup := createSyncTarget(t, name, options...)
	t.T().Cleanup(func() {
		deleteSyncTarget(t, workloadCluster)
	})
	t.T().Cleanup(func() {
		t.Expect(cleanup()).To(gomega.Succeed())
	})
	return workloadCluster
}
