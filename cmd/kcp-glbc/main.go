package main

import (
	"context"
	"flag"
	certmanclient "github.com/jetstack/cert-manager/pkg/client/clientset/versioned"
	certmanv1client "github.com/jetstack/cert-manager/pkg/client/clientset/versioned/typed/certmanager/v1"
	certmaninformer "github.com/jetstack/cert-manager/pkg/client/informers/externalversions"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/certificate"
	"sync"
	"time"

	// Make sure our workqueue MetricsProvider is the first to register
	_ "github.com/kuadrant/kcp-glbc/pkg/reconciler"

	"golang.org/x/sync/errgroup"

	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/kcp-dev/apimachinery/pkg/logicalcluster"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned"
	"github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/informers/externalversions"
	"github.com/kuadrant/kcp-glbc/pkg/metrics"
	"github.com/kuadrant/kcp-glbc/pkg/net"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/deployment"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/dns"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/ingress"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/service"
	tlsreconciler "github.com/kuadrant/kcp-glbc/pkg/reconciler/tls"
	"github.com/kuadrant/kcp-glbc/pkg/tls"
	"github.com/kuadrant/kcp-glbc/pkg/util/env"
)

const (
	numThreads   = 2
	resyncPeriod = 10 * time.Hour
)

var (
	// Control cluster client options
	glbcKubeconfig = flag.String("glbc-kubeconfig", "", "Path to GLBC kubeconfig")
	// KCP client options
	kubeconfig           = flag.String("kubeconfig", "", "Path to kubeconfig")
	kubecontext          = flag.String("context", env.GetEnvString("GLBC_KCP_CONTEXT", ""), "Context to use in the Kubeconfig file, instead of the current context")
	logicalClusterTarget = flag.String("logical-cluster", env.GetEnvString("GLBC_LOGICAL_CLUSTER_TARGET", "*"), "set the target logical cluster")
	// TLS certificate issuance options
	tlsProviderEnabled = flag.Bool("glbc-tls-provided", env.GetEnvBool("GLBC_TLS_PROVIDED", false), "Whether to generate TLS certificates for hosts")
	tlsProvider        = flag.String("glbc-tls-provider", env.GetEnvString("GLBC_TLS_PROVIDER", "glbc-ca"), "The TLS certificate issuer, one of [glbc-ca, le-staging, le-production]")
	// DNS management options
	domain            = flag.String("domain", env.GetEnvString("GLBC_DOMAIN", "hcpapps.net"), "The domain to use to expose ingresses")
	enableCustomHosts = flag.Bool("enable-custom-hosts", env.GetEnvBool("GLBC_ENABLE_CUSTOM_HOSTS", false), "Flag to enable hosts to be custom")
	dnsProvider       = flag.String("dns-provider", env.GetEnvString("GLBC_DNS_PROVIDER", "aws"), "The DNS provider being used [aws, fake]")
	// AWS Route53 options
	region = flag.String("region", env.GetEnvString("AWS_REGION", "eu-central-1"), "the region we should target with AWS clients")
	// Observability options
	monitoringPort = flag.Int("monitoring-port", 8080, "The port of the metrics endpoint (can be set to \"0\" to disable the metrics serving)")
)

var controllersGroup = sync.WaitGroup{}

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	var overrides clientcmd.ConfigOverrides
	if *kubecontext != "" {
		overrides.CurrentContext = *kubecontext
	}

	kcpClientConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfig},
		&overrides).ClientConfig()
	if err != nil {
		klog.Fatal(err)
	}

	glbcClientConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: *glbcKubeconfig},
		&clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		klog.Fatal(err)
	}

	ctx := genericapiserver.SetupSignalContext()

	kcpKubeClient, err := kubernetes.NewClusterForConfig(kcpClientConfig)
	if err != nil {
		klog.Fatal(err)
	}
	kcpKubeInformerFactory := informers.NewSharedInformerFactory(kcpKubeClient.Cluster(logicalcluster.New(*logicalClusterTarget)), resyncPeriod)

	kcpDnsRecordClient, err := kuadrantv1.NewClusterForConfig(kcpClientConfig)
	if err != nil {
		klog.Fatal(err)
	}
	kcpKuadrantInformerFactory := externalversions.NewSharedInformerFactory(kcpDnsRecordClient.Cluster(logicalcluster.New(*logicalClusterTarget)), resyncPeriod)

	// glbcKubeClient targets the control cluster (this is the cluster where glbc is deployed).
	// This is not a KCP cluster.
	glbcKubeClient, err := kubernetes.NewForConfig(glbcClientConfig)
	if err != nil {
		klog.Fatal(err)
	}

	namespace := env.GetNamespace()

	var certProvider tls.Provider
	if *tlsProviderEnabled {
		if namespace == "" {
			namespace = tls.DefaultCertificateNS
		}

		var tlsCertProvider tls.CertProvider
		switch *tlsProvider {
		case "glbc-ca":
			tlsCertProvider = tls.CertProviderCA
		case "le-staging":
			tlsCertProvider = tls.CertProviderLEStaging
		case "le-production":
			tlsCertProvider = tls.CertProviderLEProd
		default:
			klog.Fatalf("unsupported TLS certificate issuer:", *tlsProvider)
		}

		klog.Infof("Using TLS certificate issuer: %s", tlsCertProvider)

		certProvider, err = tls.NewCertManager(tls.CertManagerConfig{
			DNSValidator:  tls.DNSValidatorRoute53,
			CertClient:    certmanv1client.NewForConfigOrDie(glbcClientConfig),
			CertProvider:  tlsCertProvider,
			Region:        *region,
			K8sClient:     glbcKubeClient,
			ValidDomains:  []string{*domain},
			CertificateNS: namespace,
		})
		if err != nil {
			klog.Fatal(err)
		}

		tlsreconciler.InitMetrics(certProvider)

		// ensure Issuer is setup at start up time
		// TODO consider extracting out the setup to CRD
		if err := certProvider.Initialize(ctx); err != nil {
			klog.Fatal(err)
		}
	}

	glbcKubeInformerFactory := informers.NewSharedInformerFactoryWithOptions(glbcKubeClient, time.Minute, informers.WithNamespace(namespace))
	tlsController, err := tlsreconciler.NewController(&tlsreconciler.ControllerConfig{
		GlbcSecretInformer: glbcKubeInformerFactory.Core().V1().Secrets(),
		GlbcKubeClient:     glbcKubeClient,
		KcpKubeClient:      kcpKubeClient,
	})
	if err != nil {
		klog.Fatal(err)
	}

	certInformer := certmaninformer.NewSharedInformerFactoryWithOptions(certmanclient.NewForConfigOrDie(glbcClientConfig), 60*time.Second)

	ingressController := ingress.NewController(&ingress.ControllerConfig{
		KubeClient:            kcpKubeClient,
		CertClient:            certmanv1client.NewForConfigOrDie(glbcClientConfig),
		DnsRecordClient:       kcpDnsRecordClient,
		SharedInformerFactory: kcpKubeInformerFactory,
		CertInformer:          certInformer,
		Domain:                domain,
		CertProvider:          certProvider,
		HostResolver:          net.NewDefaultHostResolver(),
		// For testing. TODO: Make configurable through flags/env variable
		// HostResolver: &net.ConfigMapHostResolver{
		// 	Name:      "hosts",
		// 	Namespace: "default",
		// },
		CustomHostsEnabled: enableCustomHosts,
	})

	dnsRecordController, err := dns.NewController(&dns.ControllerConfig{
		DnsRecordClient:       kcpDnsRecordClient,
		SharedInformerFactory: kcpKuadrantInformerFactory,
		DNSProvider:           dnsProvider,
	})
	if err != nil {
		klog.Fatal(err)
	}

	serviceController, err := service.NewController(&service.ControllerConfig{
		ServicesClient:        kcpKubeClient,
		SharedInformerFactory: kcpKubeInformerFactory,
	})
	if err != nil {
		klog.Fatal(err)
	}

	deploymentController, err := deployment.NewController(&deployment.ControllerConfig{
		DeploymentClient:      kcpKubeClient,
		SharedInformerFactory: kcpKubeInformerFactory,
	})
	if err != nil {
		klog.Fatal(err)
	}

	certificateController, err := certificate.NewController(&certificate.ControllerConfig{
		CoreClient:   kcpKubeClient,
		CertClient:   certmanv1client.NewForConfigOrDie(glbcClientConfig),
		CertManager:  certProvider,
		CertInformer: certInformer,
	})
	if err != nil {
		klog.Fatal(err)
	}

	certInformer.Start(ctx.Done())
	certInformer.WaitForCacheSync(ctx.Done())

	kcpKubeInformerFactory.Start(ctx.Done())
	kcpKubeInformerFactory.WaitForCacheSync(ctx.Done())

	kcpKuadrantInformerFactory.Start(ctx.Done())
	kcpKuadrantInformerFactory.WaitForCacheSync(ctx.Done())

	if *tlsProviderEnabled {
		// the control cluster Kube informer is only used when TLS certificate issuance is enabled
		glbcKubeInformerFactory.Start(ctx.Done())
		glbcKubeInformerFactory.WaitForCacheSync(ctx.Done())
	}

	// start listening on the metrics endpoint
	metricsServer, err := metrics.NewServer(monitoringPort)
	if err != nil {
		klog.Exitf("Failed to create metrics server: %v", err)
	}

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(metricsServer.Start)

	start(gCtx, ingressController)
	start(gCtx, dnsRecordController)
	if *tlsProviderEnabled {
		start(gCtx, tlsController)
	}
	start(gCtx, serviceController)
	start(gCtx, deploymentController)
	start(gCtx, certificateController)

	g.Go(func() error {
		// wait until the controllers have return before stopping serving metrics
		controllersGroup.Wait()
		return metricsServer.Shutdown()
	})

	if err := g.Wait(); err != nil {
		klog.Exitf("Exiting due to error: %v", err)
	}
}

type Controller interface {
	Start(context.Context, int)
}

func start(ctx context.Context, runnable Controller) {
	controllersGroup.Add(1)
	go func() {
		defer controllersGroup.Done()
		runnable.Start(ctx, numThreads)
	}()
}
