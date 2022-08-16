package admission

import (
	"context"
	"fmt"
	"net/http"

	"github.com/kuadrant/kcp-glbc/pkg/log"

	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type TLSConfig struct {
	Cert []byte
	Key  []byte
	Host string
}

func StartServer(ctx context.Context /*, tlsChn <-chan TLSConfig*/) error {
	port := 8443

	logger := log.Logger.WithName("webhooks-server")
	logger.Info("Started webhook server")

	// tlsBlocks := <-tlsChn
	// _, err := tls.X509KeyPair(tlsBlocks.Cert, tlsBlocks.Key)
	// if err != nil {
	// 	return err
	// }

	// logger.Info(fmt.Sprintf("Got TLS key pair for host %s", tlsBlocks.Host))

	// tlsConfig := &tls.Config{
	// 	ServerName: tlsBlocks.Host,
	// 	Certificates: []tls.Certificate{
	// 		cert,
	// 	},
	// }

	mux := http.NewServeMux()

	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", port),
		// TLSConfig: tlsConfig,
		Handler: mux,
	}

	mux.Handle("/validating", &webhook.Admission{
		Handler: admission.HandlerFunc(func(ctx context.Context, r admission.Request) admission.Response {
			return admission.Denied("Just a test")
		}),
	})

	return srv.ListenAndServe()

	// ln, err := net.Listen("tcp", srv.Addr)
	// if err != nil {
	// 	return err
	// }

	// tlsListener := tls.NewListener(ln, tlsConfig)
	// return srv.Serve(tlsListener)
}
