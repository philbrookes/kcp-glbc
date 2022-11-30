package superClient

import (
	"github.com/kcp-dev/logicalcluster/v2"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	kuadrantclientv1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned"
)

type KCP struct {
	KuadrantClient   kuadrantclientv1.ClusterInterface
	KcpKubeClient    kubernetes.ClusterInterface
	KcpDynamicClient dynamic.ClusterInterface
}

func (k *KCP) WorkspaceClient(name logicalcluster.Name) kubernetes.Interface {
	return k.KcpKubeClient.Cluster(name)
}
func (k *KCP) WorkspaceDynamicClient(name logicalcluster.Name) dynamic.Interface {
	return k.KcpDynamicClient.Cluster(name)
}
func (k *KCP) WorkspaceKuadrantClient(name logicalcluster.Name) kuadrantclientv1.Interface {
	return k.KuadrantClient.Cluster(name)
}
