package metadata

import (
	"github.com/kcp-dev/apimachinery/pkg/logicalcluster"
	"k8s.io/apimachinery/pkg/types"
)

type WorkspaceNamespaceName struct {
	types.NamespacedName
	Workspace logicalcluster.LogicalCluster
}
