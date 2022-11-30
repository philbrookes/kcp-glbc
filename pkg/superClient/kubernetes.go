package superClient

import (
	"github.com/kcp-dev/logicalcluster/v2"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	kuadrantclientv1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned"
)

type Kubernetes struct {
	KuadrantClient   kuadrantclientv1.Interface
	KcpKubeClient    kubernetes.Interface
	KcpDynamicClient dynamic.Interface
}

func (k *Kubernetes) WorkspaceClient(_ logicalcluster.Name) kubernetes.Interface {
	return k.KcpKubeClient
}

func (k *Kubernetes) WorkspaceDynamicClient(_ logicalcluster.Name) dynamic.Interface {
	return k.KcpDynamicClient
}

func (k *Kubernetes) WorkspaceKuadrantClient(_ logicalcluster.Name) kuadrantclientv1.Interface {
	return k.KuadrantClient
}
