package main

import (
	"context"
	"flag"
	"fmt"
	gonet "net"
	"os"
	"reflect"
	"sync"
	"time"

	certmanclient "github.com/jetstack/cert-manager/pkg/client/clientset/versioned"
	certmaninformer "github.com/jetstack/cert-manager/pkg/client/informers/externalversions"
	"github.com/kcp-dev/logicalcluster/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/kuadrant/kcp-glbc/pkg/_internal/env"
	"github.com/kuadrant/kcp-glbc/pkg/_internal/log"
	kuadrantv1 "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/clientset/versioned"
	kuadrantinformer "github.com/kuadrant/kcp-glbc/pkg/client/kuadrant/informers/externalversions"
	"github.com/kuadrant/kcp-glbc/pkg/dns"
	"github.com/kuadrant/kcp-glbc/pkg/domains/domainverification"
	"github.com/kuadrant/kcp-glbc/pkg/metrics"
	"github.com/kuadrant/kcp-glbc/pkg/migration/deployment"
	"github.com/kuadrant/kcp-glbc/pkg/migration/secret"
	"github.com/kuadrant/kcp-glbc/pkg/migration/service"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/ingress"
	"github.com/kuadrant/kcp-glbc/pkg/reconciler/route"
	"github.com/kuadrant/kcp-glbc/pkg/superClient"
	"github.com/kuadrant/kcp-glbc/pkg/tls"
	"github.com/kuadrant/kcp-glbc/pkg/traffic"
)

const (
	numThreads   = 2
	resyncPeriod = 1 * time.Second
)

var options struct {
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
}

func init() {
	flagSet := flag.CommandLine

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

	clientConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{}).ClientConfig()
	exitOnError(err, "Failed to create K8S config")

	kubeClient, err := kubernetes.NewForConfig(clientConfig)
	exitOnError(err, "Failed to create K8S client")

	informerFactory := informers.NewSharedInformerFactoryWithOptions(kubeClient, resyncPeriod)

	dynamicClient, err := dynamic.NewForConfig(clientConfig)
	exitOnError(err, "Failed to create dynamic client")

	dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, resyncPeriod)

	// certificate client targeting the glbc workspace
	certClient := certmanclient.NewForConfigOrDie(clientConfig)

	kcpKuadrantClient, err := kuadrantv1.NewForConfig(clientConfig)
	exitOnError(err, "Failed to create KCP kuadrant client")
	kcpKuadrantInformerFactory := kuadrantinformer.NewSharedInformerFactory(kcpKuadrantClient, resyncPeriod)

	sClient := &superClient.Kubernetes{
		KuadrantClient:   kcpKuadrantClient,
		KcpKubeClient:    kubeClient,
		KcpDynamicClient: dynamicClient,
	}

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

	var controllers []Controller

	isControllerLeader := len(controllers) == 0

	dnsClient, domainVerifier := getDNSUtilities(os.Getenv("GLBC_HOST_RESOLVER"))

	routeController := route.NewController(&route.ControllerConfig{
		ControllerConfig: &reconciler.ControllerConfig{
			NameSuffix: "route-controller",
		},
		SuperClient:                     sClient,
		KCPInformer:                     kcpKuadrantInformerFactory,
		KCPSharedInformerFactory:        informerFactory,
		KCPDynamicSharedInformerFactory: dynamicInformerFactory,
		CertificateInformer:             certificateInformerFactory,
		GlbcInformerFactory:             informerFactory,
		Domain:                          options.Domain,
		CertProvider:                    certProvider,
		HostResolver:                    dnsClient,
		GLBCWorkspace:                   logicalcluster.New(""),
	})

	controllers = append(controllers, routeController)

	ingressController := ingress.NewController(&ingress.ControllerConfig{
		ControllerConfig: &reconciler.ControllerConfig{
			NameSuffix: "ingress-controller",
		},
		SuperClient:              sClient,
		KuadrantInformer:         kcpKuadrantInformerFactory,
		KCPSharedInformerFactory: informerFactory,
		CertificateInformer:      certificateInformerFactory,
		GlbcInformerFactory:      informerFactory,
		Domain:                   options.Domain,
		CertProvider:             certProvider,
		HostResolver:             dnsClient,
		GLBCWorkspace:            logicalcluster.New(""),
	})
	controllers = append(controllers, ingressController)

	dnsRecordController, err := dns.NewController(&dns.ControllerConfig{
		ControllerConfig: &reconciler.ControllerConfig{
			NameSuffix: "dns-record-controller",
		},
		SuperClient:           sClient,
		SharedInformerFactory: kcpKuadrantInformerFactory,
		DNSProvider:           options.DNSProvider,
	})
	exitOnError(err, "Failed to create DNSRecord controller")
	controllers = append(controllers, dnsRecordController)

	domainVerificationController, err := domainverification.NewController(&domainverification.ControllerConfig{
		ControllerConfig: &reconciler.ControllerConfig{
			NameSuffix: "domain-verification-controller",
		},
		SuperClient:           sClient,
		SharedInformerFactory: kcpKuadrantInformerFactory,
		DNSVerifier:           domainVerifier,
		GLBCWorkspace:         logicalcluster.New(""),
	})
	exitOnError(err, "Failed to create DomainVerification controller")
	controllers = append(controllers, domainVerificationController)

	serviceController, err := service.NewController(&service.ControllerConfig{
		ControllerConfig: &reconciler.ControllerConfig{
			NameSuffix: "service-controller",
		},
		SuperClient:           sClient,
		SharedInformerFactory: informerFactory,
	})
	exitOnError(err, "Failed to create Service controller")

	deploymentController, err := deployment.NewController(&deployment.ControllerConfig{
		ControllerConfig: &reconciler.ControllerConfig{
			NameSuffix: "deployment-controller",
		},
		SuperClient:           sClient,
		SharedInformerFactory: informerFactory,
	})
	exitOnError(err, "Failed to create Deployment controller")

	// Secret controller should not have more than one instance
	if isControllerLeader {
		secretController, err := secret.NewController(&secret.ControllerConfig{
			ControllerConfig: &reconciler.ControllerConfig{
				NameSuffix: "secret-controller",
			},
			SuperClient:           sClient,
			SharedInformerFactory: informerFactory,
		})
		exitOnError(err, "Failed to create Secret controller")

		controllers = append(controllers, secretController)
	}

	controllers = append(controllers, deploymentController)
	controllers = append(controllers, serviceController)

	certificateInformerFactory.Start(ctx.Done())
	certificateInformerFactory.WaitForCacheSync(ctx.Done())
	informerFactory.Start(ctx.Done())
	informerFactory.WaitForCacheSync(ctx.Done())
	dynamicInformerFactory.Start(ctx.Done())
	dynamicInformerFactory.WaitForCacheSync(ctx.Done())

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
