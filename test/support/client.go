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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	kcp "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"

	certmanclient "github.com/jetstack/cert-manager/pkg/client/clientset/versioned"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned"
)

type Client interface {
	Core() kubernetes.ClusterInterface
	Kcp() kcp.ClusterInterface
	Kuadrant() kuadrantv1.ClusterInterface
	Certs() certmanclient.Interface
	Dynamic() dynamic.ClusterInterface
	GetConfig() *rest.Config
}

type client struct {
	core     kubernetes.ClusterInterface
	kcp      kcp.ClusterInterface
	kuadrant kuadrantv1.ClusterInterface
	certs    certmanclient.Interface
	config   *rest.Config
	dynamic  dynamic.ClusterInterface
}

func (c *client) Certs() certmanclient.Interface {
	return c.certs
}

func (c *client) Core() kubernetes.ClusterInterface {
	return c.core
}

func (c *client) Kcp() kcp.ClusterInterface {
	return c.kcp
}

func (c *client) Kuadrant() kuadrantv1.ClusterInterface {
	return c.kuadrant
}

func (c *client) Dynamic() dynamic.ClusterInterface {
	return c.dynamic
}

func (c *client) GetConfig() *rest.Config {
	return c.config
}

func newTestClient() (Client, error) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{
			CurrentContext: "system:admin",
		}).ClientConfig()
	if err != nil {
		return nil, err
	}

	kubeClient, err := kubernetes.NewClusterForConfig(cfg)
	if err != nil {
		return nil, err
	}

	kcpClient, err := kcp.NewClusterForConfig(cfg)
	if err != nil {
		return nil, err
	}

	kuandrantClient, err := kuadrantv1.NewClusterForConfig(cfg)
	if err != nil {
		return nil, err
	}

	certClient, err := certmanclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	dynamicClient, err := dynamic.NewClusterForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &client{
		core:     kubeClient,
		kcp:      kcpClient,
		kuadrant: kuandrantClient,
		certs:    certClient,
		config:   cfg,
		dynamic:  dynamicClient,
	}, nil
}
