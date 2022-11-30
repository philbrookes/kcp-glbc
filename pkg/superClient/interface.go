package superClient

import (
	"github.com/kcp-dev/logicalcluster/v2"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	kuadrantclientv1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned"
)

type Interface interface {
	WorkspaceClient(name logicalcluster.Name) kubernetes.Interface
	WorkspaceDynamicClient(name logicalcluster.Name) dynamic.Interface
	WorkspaceKuadrantClient(name logicalcluster.Name) kuadrantclientv1.Interface
}
