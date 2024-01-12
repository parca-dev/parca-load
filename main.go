package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"syscall"
	"time"

	"buf.build/gen/go/parca-dev/parca/connectrpc/go/parca/query/v1alpha1/queryv1alpha1connect"
	"connectrpc.com/connect"
	vault "github.com/hashicorp/vault/api"
	auth "github.com/hashicorp/vault/api/auth/kubernetes"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const grpcCodeOK = "ok"

func main() {
	url := flag.String("url", "http://localhost:7070", "The URL for the Parca instance to query")
	addr := flag.String("addr", "127.0.0.1:7171", "The address the HTTP server binds to")
	token := flag.String("token", "", "A bearer token that can be send along each request")
	vaultURL := flag.String("vault-url", "", "The URL for parca-load to reach Vault on")
	vaultTokenPath := flag.String("vault-token-path", "parca-load/token", "The path in Vault to find the parca-load token")
	vaultRole := flag.String("vault-role", "parca-load", "The role name of parca-load in Vault")
	clientTimeout := flag.Duration("client-timeout", 10*time.Second, "Timeout for requests to the Parca instance")

	queryInterval := flag.Duration("query-interval", 5*time.Second, "The time interval between queries to the Parca instance")
	queryRangeStr := flag.String("query-range", "15m,12h,168h", "Comma-separated time durations for query")

	flag.Parse()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	// If a vault URL is given we'll try to get the token from Vault.
	// If successful the contents are written in place of the token flag.
	// Further down the token is retrieved from that flag's content.
	if *vaultURL != "" {
		config := vault.DefaultConfig()
		config.Address = *vaultURL

		client, err := vault.NewClient(config)
		if err != nil {
			log.Fatalf("unable to initialize Vault client: %v", err)
		}
		kubernetesAuth, err := auth.NewKubernetesAuth(*vaultRole)
		if err != nil {
			log.Fatalf("unable to initialize Kubernetes auth method: %v", err)
		}
		login, err := client.Auth().Login(ctx, kubernetesAuth)
		if err != nil {
			log.Fatalf("unable to log in with Kubernetes auth: %v", err)
		}
		if login == nil {
			log.Fatal("no auth info was returned after login")
		}

		// get secret from Vault, from the default mount path for KV v2 in dev mode, "secret"
		secret, err := client.KVv2("secret").Get(ctx, *vaultTokenPath)
		if err != nil {
			log.Fatalf("unable to read secret: %v", err)
		}

		tokenContent, ok := secret.Data["token"].(string)
		if !ok {
			log.Fatalf("value type assertion failed: %T %#v", secret.Data["token"], secret.Data["token"])
		}

		// Override the flag content with the token from Vault.
		*token = tokenContent
	}

	queryRanges, err := parseTimeRanges(*queryRangeStr)
	if err != nil {
		log.Fatalf("parse time range string error: %v", err)
	}

	clientOptions := []connect.ClientOption{
		connect.WithGRPCWeb(),
	}
	if *token != "" {
		clientOptions = append(clientOptions, connect.WithInterceptors(&bearerTokenInterceptor{token: *token}))
	}

	client := queryv1alpha1connect.NewQueryServiceClient(
		&http.Client{Timeout: *clientTimeout},
		*url,
		clientOptions...,
	)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	querier := NewQuerier(reg, client, queryRanges)

	var gr run.Group
	gr.Add(run.SignalHandler(ctx, os.Interrupt, syscall.SIGTERM))

	httpServer := newHTTPServer(reg, *addr)
	gr.Add(
		func() error {
			log.Printf("HTTP server: running at %s\n", *addr)
			return httpServer.ListenAndServe()
		},
		func(error) {
			log.Println("HTTP server: stopping")
			shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(shutdownCtx)
			log.Println("HTTP server: stopped")
		},
	)
	gr.Add(
		func() error {
			querier.Run(ctx, *queryInterval)
			return nil
		},
		func(error) {
			log.Println("querier: stopping")
			querier.Stop()
			log.Println("querier: stopped")
		},
	)

	if err := gr.Run(); err != nil {
		if _, ok := err.(run.SignalError); ok {
			log.Println("terminated:", err)
			return
		}
		log.Fatal(err)
	}
}

func newHTTPServer(reg *prometheus.Registry, addr string) *http.Server {
	handler := http.NewServeMux()
	handler.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	handler.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))

	server := &http.Server{
		Addr:    addr,
		Handler: handler,
	}
	return server
}

type bearerTokenInterceptor struct {
	token string
}

func (i *bearerTokenInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("authorization", "Bearer "+i.token)
		return next(ctx, req)
	}
}

func (i *bearerTokenInterceptor) WrapStreamingClient(client connect.StreamingClientFunc) connect.StreamingClientFunc {
	return client
}

func (i *bearerTokenInterceptor) WrapStreamingHandler(handler connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return handler
}

func parseTimeRanges(input string) ([]time.Duration, error) {
	parts := strings.Split(input, ",")
	durations := make([]time.Duration, len(parts))
	var err error

	for i, part := range parts {
		durations[i], err = time.ParseDuration(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
	}

	return durations, nil
}
