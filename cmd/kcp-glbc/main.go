package main

import (
	"context"
	"flag"
	"fmt"
	gonet "net"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"

	certmanclient "github.com/jetstack/cert-manager/pkg/client/clientset/versioned"
	certmaninformer "github.com/jetstack/cert-manager/pkg/client/informers/externalversions"

	"github.com/kuadrant/kcp-glbc/pkg/superClient"

	// Make sure our workqueue MetricsProvider is the first to register
	_ "github.com/kuadrant/kcp-glbc/pkg/reconciler"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/route"
	"github.com/kuadrant/kcp-glbc/pkg/traffic"

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

	"github.com/kuadrant/kcp-glbc/pkg/_internal/log"
	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned"
	kuadrantinformer "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/informers/externalversions"
	"github.com/kuadrant/kcp-glbc/pkg/dns"
	"github.com/kuadrant/kcp-glbc/pkg/domains/domainverification"
	"github.com/kuadrant/kcp-glbc/pkg/metrics"
	"github.com/kuadrant/kcp-glbc/pkg/migration/deployment"
	"github.com/kuadrant/kcp-glbc/pkg/migration/secret"
	"github.com/kuadrant/kcp-glbc/pkg/migration/service"

	"github.com/kuadrant/kcp-glbc/pkg/_internal/env"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/ingress"
	"github.com/kuadrant/kcp-glbc/pkg/tls"
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
	// The TLS certificate issuer
	TLSProvider string
	// The base domain
	Domain string
	// The DNS provider
	DNSProvider string
	// The AWS Route53 region
	Region string
	// The port number of the metrics endpoint
	MonitoringPort int
	// The glbc exports to use
	ExportName string
}

type APIExportClusterInformers struct {
	SharedInformerFactory         informers.SharedInformerFactory
	KuadrantSharedInformerFactory kuadrantinformer.SharedInformerFactory
	KCPDynamicInformerFactory     dynamicinformer.DynamicSharedInformerFactory
}

func init() {
	flagSet := flag.CommandLine

	// KCP client options
	flagSet.StringVar(&options.GLBCWorkspace, "glbc-workspace", env.GetEnvString("GLBC_WORKSPACE", "root:kuadrant"), "The GLBC workspace")
	flagSet.StringVar(&options.ExportName, "glbc-export", env.GetEnvString("GLBC_EXPORT", "glbc-root-kuadrant"), "comma separated list of glbc APIExport names")
	flagSet.StringVar(&options.LogicalClusterTarget, "logical-cluster", env.GetEnvString("GLBC_LOGICAL_CLUSTER_TARGET", "*"), "set the target logical cluster")
	flagSet.StringVar(&options.TLSProvider, "glbc-tls-provider", env.GetEnvString("GLBC_TLS_PROVIDER", "glbc-ca"), "The TLS certificate issuer, one of [glbc-ca, le-staging, le-production]")
	// DNS management options
	flagSet.StringVar(&options.Domain, "domain", env.GetEnvString("GLBC_DOMAIN", "dev.hcpapps.net"), "The domain to use to expose ingresses")
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
	// Logging GLBC configuration
	printOptions()

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

	// certificate client targeting the glbc workspace
	certClient := certmanclient.NewForConfigOrDie(kcpClientConfig)

	namespace := env.GetNamespace()
	if namespace == "" {
		namespace = tls.DefaultCertificateNS
	}

	certificateInformerFactory := certmaninformer.NewSharedInformerFactoryWithOptions(certClient, resyncPeriod, certmaninformer.WithNamespace(namespace))

	var certProvider tls.Provider

	// TLSProvider is mandatory when TLS is enabled
	if options.TLSProvider == "" {
		exitOnError(fmt.Errorf("TLS Provider not specified"), "Failed to create cert provider")
	}

	var tlsCertProvider = tls.CertProvider(options.TLSProvider)

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
	route.InitMetrics()
	traffic.InitMetrics(certProvider)

	_, err = certProvider.IssuerExists(ctx)
	exitOnError(err, "Failed cert provider issuer check")

	glbcKubeInformerFactory := informers.NewSharedInformerFactoryWithOptions(kubeClient, time.Minute, informers.WithNamespace(namespace))

	exitOnError(err, "Failed to create TLS certificate controller")

	apiExportNames := strings.Split(options.ExportName, ",")
	log.Logger.Info(fmt.Sprintf("Instantiating controllers for APIExports: %v", apiExportNames))

	var apiExportClusterInformers []APIExportClusterInformers
	var controllers []Controller
	for _, name := range apiExportNames {
		glbcAPIExport, err := kcpClient.Cluster(logicalcluster.New(options.GLBCWorkspace)).ApisV1alpha1().APIExports().Get(ctx, name, metav1.GetOptions{})
		exitOnError(err, "Failed to get GLBC APIExport "+name)

		glbcVirtualWorkspaceURL, glbcIdentityHash := getAPIExportVirtualWorkspaceURLAndIdentityHash(glbcAPIExport)
		log.Logger.Info(fmt.Sprintf("GLBC APIExport URL: %s, identityHash :%s", glbcVirtualWorkspaceURL, glbcIdentityHash))

		glbcVWClientConfig := rest.CopyConfig(kcpClientConfig)
		glbcVWClientConfig.Host = glbcVirtualWorkspaceURL

		kcpKubeClient, err := kubernetes.NewClusterForConfig(glbcVWClientConfig)
		exitOnError(err, "Failed to create KCP core client")
		kcpKubeInformerFactory := informers.NewSharedInformerFactory(kcpKubeClient.Cluster(logicalcluster.New(options.LogicalClusterTarget)), resyncPeriod)

		kcpDynamicClient, err := dynamic.NewClusterForConfig(glbcVWClientConfig)
		exitOnError(err, "Failed to create KCP dynamic client")
		kcpDynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(kcpDynamicClient.Cluster(logicalcluster.New(options.LogicalClusterTarget)), resyncPeriod)

		kcpKuadrantClient, err := kuadrantv1.NewClusterForConfig(glbcVWClientConfig)
		exitOnError(err, "Failed to create KCP kuadrant client")
		kcpKuadrantInformerFactory := kuadrantinformer.NewSharedInformerFactory(kcpKuadrantClient.Cluster(logicalcluster.New(options.LogicalClusterTarget)), resyncPeriod)

		clusterInformers := &APIExportClusterInformers{}
		clusterInformers.SharedInformerFactory = kcpKubeInformerFactory
		clusterInformers.KuadrantSharedInformerFactory = kcpKuadrantInformerFactory
		clusterInformers.KCPDynamicInformerFactory = kcpDynamicInformerFactory

		isControllerLeader := len(controllers) == 0

		dnsClient, domainVerifier := getDNSUtilities(os.Getenv("GLBC_HOST_RESOLVER"))
		superClient := &superClient.KCP{
			KuadrantClient:   kcpKuadrantClient,
			KcpKubeClient:    kcpKubeClient,
			KcpDynamicClient: kcpDynamicClient,
		}
		routeController := route.NewController(&route.ControllerConfig{
			ControllerConfig: &reconciler.ControllerConfig{
				NameSuffix: name,
			},
			SuperClient:                     superClient,
			KCPInformer:                     kcpKuadrantInformerFactory,
			KCPSharedInformerFactory:        kcpKubeInformerFactory,
			KCPDynamicSharedInformerFactory: kcpDynamicInformerFactory,
			CertificateInformer:             certificateInformerFactory,
			GlbcInformerFactory:             glbcKubeInformerFactory,
			Domain:                          options.Domain,
			CertProvider:                    certProvider,
			HostResolver:                    dnsClient,
			GLBCWorkspace:                   logicalcluster.New(options.GLBCWorkspace),
		})

		controllers = append(controllers, routeController)

		ingressController := ingress.NewController(&ingress.ControllerConfig{
			ControllerConfig: &reconciler.ControllerConfig{
				NameSuffix: name,
			},
			SuperClient:              superClient,
			KuadrantInformer:         kcpKuadrantInformerFactory,
			KCPSharedInformerFactory: kcpKubeInformerFactory,
			CertificateInformer:      certificateInformerFactory,
			GlbcInformerFactory:      glbcKubeInformerFactory,
			Domain:                   options.Domain,
			CertProvider:             certProvider,
			HostResolver:             dnsClient,
			GLBCWorkspace:            logicalcluster.New(options.GLBCWorkspace),
		})
		controllers = append(controllers, ingressController)

		dnsRecordController, err := dns.NewController(&dns.ControllerConfig{
			ControllerConfig: &reconciler.ControllerConfig{
				NameSuffix: name,
			},
			SuperClient:           superClient,
			SharedInformerFactory: kcpKuadrantInformerFactory,
			DNSProvider:           options.DNSProvider,
		})
		exitOnError(err, "Failed to create DNSRecord controller")
		controllers = append(controllers, dnsRecordController)

		domainVerificationController, err := domainverification.NewController(&domainverification.ControllerConfig{
			ControllerConfig: &reconciler.ControllerConfig{
				NameSuffix: name,
			},
			SuperClient:           superClient,
			SharedInformerFactory: kcpKuadrantInformerFactory,
			DNSVerifier:           domainVerifier,
			GLBCWorkspace:         logicalcluster.New(options.GLBCWorkspace),
		})
		exitOnError(err, "Failed to create DomainVerification controller")
		controllers = append(controllers, domainVerificationController)

		serviceController, err := service.NewController(&service.ControllerConfig{
			ControllerConfig: &reconciler.ControllerConfig{
				NameSuffix: name,
			},
			SuperClient:           superClient,
			SharedInformerFactory: kcpKubeInformerFactory,
		})
		exitOnError(err, "Failed to create Service controller")

		deploymentController, err := deployment.NewController(&deployment.ControllerConfig{
			ControllerConfig: &reconciler.ControllerConfig{
				NameSuffix: name,
			},
			SuperClient:           superClient,
			SharedInformerFactory: kcpKubeInformerFactory,
		})
		exitOnError(err, "Failed to create Deployment controller")

		// Secret controller should not have more than one instance
		if isControllerLeader {
			secretController, err := secret.NewController(&secret.ControllerConfig{
				ControllerConfig: &reconciler.ControllerConfig{
					NameSuffix: name,
				},
				SuperClient:           superClient,
				SharedInformerFactory: kcpKubeInformerFactory,
			})
			exitOnError(err, "Failed to create Secret controller")

			controllers = append(controllers, secretController)
		}

		controllers = append(controllers, deploymentController)
		controllers = append(controllers, serviceController)

		apiExportClusterInformers = append(apiExportClusterInformers, *clusterInformers)
	}

	for _, clusterInformers := range apiExportClusterInformers {
		clusterInformers.SharedInformerFactory.Start(ctx.Done())
		clusterInformers.SharedInformerFactory.WaitForCacheSync(ctx.Done())

		clusterInformers.KuadrantSharedInformerFactory.Start(ctx.Done())
		clusterInformers.KuadrantSharedInformerFactory.WaitForCacheSync(ctx.Done())

		clusterInformers.KCPDynamicInformerFactory.Start(ctx.Done())
		clusterInformers.KCPDynamicInformerFactory.WaitForCacheSync(ctx.Done())
	}

	certificateInformerFactory.Start(ctx.Done())
	certificateInformerFactory.WaitForCacheSync(ctx.Done())
	glbcKubeInformerFactory.Start(ctx.Done())
	glbcKubeInformerFactory.WaitForCacheSync(ctx.Done())

	for _, controller := range controllers {
		start(gCtx, controller)
	}

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

func printOptions() {
	log.Logger.Info("GLBC Configuration options: ")
	v := reflect.ValueOf(&options).Elem()
	for i := 0; i < v.NumField(); i++ {
		log.Logger.Info("GLBC Options: ", v.Type().Field(i).Name, v.Field(i).Interface())
	}
}

func getDNSUtilities(hostResolverType string) (dns.HostResolver, domainverification.DNSVerifier) {
	switch hostResolverType {
	case "default":
		log.Logger.Info("using default host resolver")
		return dns.NewDefaultHostResolver(), dns.NewVerifier(gonet.DefaultResolver)
	case "e2e-mock":
		log.Logger.Info("using e2e-mock host resolver")
		resolver := &dns.ConfigMapHostResolver{
			Name:      "hosts",
			Namespace: "kcp-glbc",
		}

		return resolver, resolver
	default:
		log.Logger.Info("using default host resolver")
		return dns.NewDefaultHostResolver(), dns.NewVerifier(gonet.DefaultResolver)
	}
}
