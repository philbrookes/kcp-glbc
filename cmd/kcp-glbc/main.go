package main

import (
	"context"
	"flag"
	"fmt"
	dnspkg "github.com/kuadrant/kcp-glbc/pkg/dns"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/domainverification"
	gonet "net"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"

	// Make sure our workqueue MetricsProvider is the first to register
	_ "github.com/kuadrant/kcp-glbc/pkg/reconciler"

	certmanclient "github.com/jetstack/cert-manager/pkg/client/clientset/versioned"
	certmaninformer "github.com/jetstack/cert-manager/pkg/client/informers/externalversions"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	"github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1/helper"
	conditionsutil "github.com/kcp-dev/kcp/pkg/apis/third_party/conditions/util/conditions"
	kcp "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"
	"github.com/kcp-dev/logicalcluster/v2"

	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned"
	"github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/informers/externalversions"
	"github.com/kuadrant/kcp-glbc/pkg/log"
	"github.com/kuadrant/kcp-glbc/pkg/metrics"
	"github.com/kuadrant/kcp-glbc/pkg/net"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/deployment"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/dns"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/ingress"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/service"
	"github.com/kuadrant/kcp-glbc/pkg/tls"
	"github.com/kuadrant/kcp-glbc/pkg/util/env"
)

const (
	numThreads   = 2
	resyncPeriod = 10 * time.Hour
)

var options struct {
	// The GLBC workspace
	GLBCWorkspace string
	// The kcp logical cluster
	LogicalClusterTarget string
	// Whether to generate TLS certificates for hosts
	TLSProviderEnabled bool
	// The TLS certificate issuer
	TLSProvider string
	// The base domain
	Domain string
	// Whether custom hosts are permitted
	EnableCustomHosts bool
	// The DNS provider
	DNSProvider string
	// The AWS Route53 region
	Region string
	// The port number of the metrics endpoint
	MonitoringPort int
}

func init() {
	flagSet := flag.CommandLine

	// KCP client options
	flagSet.StringVar(&options.GLBCWorkspace, "glbc-workspace", env.GetEnvString("GLBC_WORKSPACE", "root:default:kcp-glbc"), "The GLBC workspace")
	flagSet.StringVar(&options.LogicalClusterTarget, "logical-cluster", env.GetEnvString("GLBC_LOGICAL_CLUSTER_TARGET", "*"), "set the target logical cluster")
	// TLS certificate issuance options
	flagSet.BoolVar(&options.TLSProviderEnabled, "glbc-tls-provided", env.GetEnvBool("GLBC_TLS_PROVIDED", true), "Whether to generate TLS certificates for hosts")
	flagSet.StringVar(&options.TLSProvider, "glbc-tls-provider", env.GetEnvString("GLBC_TLS_PROVIDER", "glbc-ca"), "The TLS certificate issuer, one of [glbc-ca, le-staging, le-production]")
	// DNS management options
	flagSet.StringVar(&options.Domain, "domain", env.GetEnvString("GLBC_DOMAIN", "dev.hcpapps.net"), "The domain to use to expose ingresses")
	flagSet.BoolVar(&options.EnableCustomHosts, "enable-custom-hosts", env.GetEnvBool("GLBC_ENABLE_CUSTOM_HOSTS", false), "Flag to enable hosts to be custom")
	flag.StringVar(&options.DNSProvider, "dns-provider", env.GetEnvString("GLBC_DNS_PROVIDER", "fake"), "The DNS provider being used [aws, fake]")
	// // AWS Route53 options
	flag.StringVar(&options.Region, "region", env.GetEnvString("AWS_REGION", "eu-central-1"), "the region we should target with AWS clients")
	//  Observability options
	flagSet.IntVar(&options.MonitoringPort, "monitoring-port", 8080, "The port of the metrics endpoint (can be set to \"0\" to disable the metrics serving)")

	opts := log.Options{
		EncoderConfigOptions: []log.EncoderConfigOption{
			func(c *zapcore.EncoderConfig) {
				c.ConsoleSeparator = " "
			},
		},
		ZapOpts: []zap.Option{
			zap.AddCaller(),
		},
	}
	opts.BindFlags(flag.CommandLine)
	klog.InitFlags(flag.CommandLine)
	flag.Parse()

	log.Logger = log.New(log.UseFlagOptions(&opts))
	klog.SetLogger(log.Logger)
}

var controllersGroup = sync.WaitGroup{}

func main() {
	// start listening on the metrics endpoint
	metricsServer, err := metrics.NewServer(options.MonitoringPort)
	exitOnError(err, "Failed to create metrics server")

	ctx := genericapiserver.SetupSignalContext()
	g, gCtx := errgroup.WithContext(ctx)

	g.Go(metricsServer.Start)

	kcpClientConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{}).ClientConfig()
	exitOnError(err, "Failed to create K8S config")
	kubeClient, err := kubernetes.NewForConfig(kcpClientConfig)
	exitOnError(err, "Failed to create K8S client")

	kcpClient, err := kcp.NewClusterForConfig(kcpClientConfig)
	exitOnError(err, "Failed to create KCP client")

	glbcAPIExport, err := kcpClient.Cluster(logicalcluster.New(options.GLBCWorkspace)).ApisV1alpha1().APIExports().Get(ctx, "glbc", metav1.GetOptions{})
	exitOnError(err, "Failed to get GLBC APIExport")

	glbcVirtualWorkspaceURL, glbcIdentityHash := getAPIExportVirtualWorkspaceURLAndIdentityHash(glbcAPIExport)
	log.Logger.Info(fmt.Sprintf("GLBC APIExport URL: %s, identityHash :%s", glbcVirtualWorkspaceURL, glbcIdentityHash))

	glbcVWClientConfig := rest.CopyConfig(kcpClientConfig)
	glbcVWClientConfig.Host = glbcVirtualWorkspaceURL

	kcpKuadrantClient, err := kuadrantv1.NewClusterForConfig(glbcVWClientConfig)
	exitOnError(err, "Failed to create KCP kuadrant client")
	kcpKuadrantInformerFactory := externalversions.NewSharedInformerFactory(kcpKuadrantClient.Cluster(logicalcluster.New(options.LogicalClusterTarget)), resyncPeriod)

	kcpKubeClient, err := kubernetes.NewClusterForConfig(glbcVWClientConfig)
	exitOnError(err, "Failed to create KCP core client")
	kcpKubeInformerFactory := informers.NewSharedInformerFactory(kcpKubeClient.Cluster(logicalcluster.New(options.LogicalClusterTarget)), resyncPeriod)

	// certificate client targeting the glbc workspace
	certClient := certmanclient.NewForConfigOrDie(kcpClientConfig)

	namespace := env.GetNamespace()
	if namespace == "" {
		namespace = tls.DefaultCertificateNS
	}

	certificateInformerFactory := certmaninformer.NewSharedInformerFactoryWithOptions(certClient, resyncPeriod, certmaninformer.WithNamespace(namespace))

	var certProvider tls.Provider
	if options.TLSProviderEnabled {

		// TLSProvider is mandatory when TLS is enabled
		if options.TLSProvider == "" {
			exitOnError(fmt.Errorf("TLS Provider not specified"), "Failed to create cert provider")
		}

		var tlsCertProvider tls.CertProvider = tls.CertProvider(options.TLSProvider)

		log.Logger.Info("Instantiating TLS certificate provider", "issuer", tlsCertProvider)

		certProvider, err = tls.NewCertManager(tls.CertManagerConfig{
			DNSValidator:  tls.DNSValidatorRoute53,
			CertClient:    certClient,
			CertProvider:  tlsCertProvider,
			Region:        options.Region,
			K8sClient:     kubeClient,
			ValidDomains:  []string{options.Domain},
			CertificateNS: namespace,
		})
		exitOnError(err, "Failed to create cert provider")

		ingress.InitMetrics(certProvider)

	}

	glbcKubeInformerFactory := informers.NewSharedInformerFactoryWithOptions(kubeClient, time.Minute, informers.WithNamespace(namespace))

	exitOnError(err, "Failed to create TLS certificate controller")

	ingressController := ingress.NewController(&ingress.ControllerConfig{
		KubeClient:               kcpKubeClient,
		DnsRecordClient:          kcpKuadrantClient,
		DNSRecordInformer:        kcpKuadrantInformerFactory,
		KCPSharedInformerFactory: kcpKubeInformerFactory,
		CertificateInformer:      certificateInformerFactory,
		GlbcInformerFactory:      glbcKubeInformerFactory,
		Domain:                   options.Domain,
		CertProvider:             certProvider,
		HostResolver:             net.NewDefaultHostResolver(),
		// For testing. TODO: Make configurable through flags/env variable
		// HostResolver: &net.ConfigMapHostResolver{
		// 	Name:      "hosts",
		// 	Namespace: "default",
		// },
		CustomHostsEnabled: options.EnableCustomHosts,
	})

	dnsRecordController, err := dns.NewController(&dns.ControllerConfig{
		DnsRecordClient:       kcpKuadrantClient,
		SharedInformerFactory: kcpKuadrantInformerFactory,
		DNSProvider:           options.DNSProvider,
	})
	exitOnError(err, "Failed to create DNSRecord controller")

	domainVerificationController, err := domainverification.NewController(&domainverification.ControllerConfig{
		DomainVerificationClient: kcpKuadrantClient,
		SharedInformerFactory:    kcpKuadrantInformerFactory,
		DNSVerifier:              dnspkg.NewVerifier(gonet.DefaultResolver),
	})

	exitOnError(err, "Failed to create DomainVerification controller")

	serviceController, err := service.NewController(&service.ControllerConfig{
		ServicesClient:        kcpKubeClient,
		SharedInformerFactory: kcpKubeInformerFactory,
	})
	exitOnError(err, "Failed to create Service controller")

	deploymentController, err := deployment.NewController(&deployment.ControllerConfig{
		DeploymentClient:      kcpKubeClient,
		SharedInformerFactory: kcpKubeInformerFactory,
	})
	exitOnError(err, "Failed to create Deployment controller")

	kcpKubeInformerFactory.Start(ctx.Done())
	kcpKubeInformerFactory.WaitForCacheSync(ctx.Done())

	kcpKuadrantInformerFactory.Start(ctx.Done())
	kcpKuadrantInformerFactory.WaitForCacheSync(ctx.Done())

	if options.TLSProviderEnabled {
		certificateInformerFactory.Start(ctx.Done())
		certificateInformerFactory.WaitForCacheSync(ctx.Done())
		glbcKubeInformerFactory.Start(ctx.Done())
		glbcKubeInformerFactory.WaitForCacheSync(ctx.Done())
	}

	start(gCtx, ingressController)
	start(gCtx, dnsRecordController)
	start(gCtx, domainVerificationController)

	start(gCtx, serviceController)
	start(gCtx, deploymentController)

	g.Go(func() error {
		// wait until the controllers have return before stopping serving metrics
		controllersGroup.Wait()
		return metricsServer.Shutdown()
	})

	exitOnError(g.Wait(), "Exiting due to error")
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

func exitOnError(err error, msg string) {
	if err != nil {
		log.Logger.Error(err, msg)
		os.Exit(1)
	}
}

func getAPIExportVirtualWorkspaceURLAndIdentityHash(export *apisv1alpha1.APIExport) (string, string) {
	if conditionsutil.IsFalse(export, apisv1alpha1.APIExportVirtualWorkspaceURLsReady) {
		exitOnError(fmt.Errorf("APIExport %s is not ready", helper.QualifiedObjectName(export)), "Failed to get APIExport virtual workspace URL")
	}

	if len(export.Status.VirtualWorkspaces) != 1 {
		// It's not clear how to handle multiple API export virtual workspace URLs. Let's fail fast.
		exitOnError(fmt.Errorf("APIExport %s has multiple virtual workspace URLs", helper.QualifiedObjectName(export)), "Failed to get APIExport virtual workspace URL")
	}

	return export.Status.VirtualWorkspaces[0].URL, export.Status.IdentityHash
}
