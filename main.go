package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/bufbuild/connect-go"
	vault "github.com/hashicorp/vault/api"
	auth "github.com/hashicorp/vault/api/auth/kubernetes"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/prometheus/model/labels"
	queryv1alpha1 "go.buf.build/bufbuild/connect-go/parca-dev/parca/parca/query/v1alpha1"
	"go.buf.build/bufbuild/connect-go/parca-dev/parca/parca/query/v1alpha1/queryv1alpha1connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/parca-dev/parca-load/sync"
)

const grpcCodeOK = "ok"

func main() {
	rand.Seed(time.Now().UnixNano())

	url := flag.String("url", "http://localhost:7070", "The URL for the Parca instance to query")
	addr := flag.String("addr", "127.0.0.1:7171", "The address the HTTP server binds to")
	token := flag.String("token", "", "A bearer token that can be send along each request")
	vaultURL := flag.String("vault-url", "", "The URL for parca-load to reach Vault on")
	vaultTokenPath := flag.String("vault-token-path", "parca-load/token", "The path in Vault to find the parca-load token")
	vaultRole := flag.String("vault-role", "parca-load", "The role name of parca-load in Vault")
	clientTimeout := flag.Duration("client-timeout", 10*time.Second, "Timeout for requests to the Parca instance")

	intervalProfileTypes := flag.Duration("interval-profile-types", 10*time.Second, "How frequent the ProfileTypes should be queried")
	intervalLabels := flag.Duration("interval-labels", 5*time.Second, "How frequent the Labels should be queried")
	intervalValues := flag.Duration("interval-values", 5*time.Second, "How frequent the Values should be queried")
	intervalQueryRange := flag.Duration("interval-query-range", 30*time.Second, "How frequent the QueryRange should be queried")
	intervalQuerySingle := flag.Duration("interval-query-single", 10*time.Second, "How frequent a single Query should be queried")
	intervalQueryMerge := flag.Duration("interval-query-merge", 15*time.Second, "How frequent a merge Query should be queried")

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

	// Expire the stores values in the maps depending on the interval they are queried at.
	labels := sync.NewExpireMap[string, struct{}](*intervalLabels * 4)
	profileTypes := sync.NewExpireMap[string, struct{}](*intervalProfileTypes * 4)
	series := sync.NewExpireMap[string, []*queryv1alpha1.MetricsSample](*intervalQueryRange * 2)

	var gr run.Group

	gr.Add(run.SignalHandler(ctx, os.Interrupt, os.Kill))
	gr.Add(internalServer(reg, *addr))

	gr.Add(queryLabels(ctx, client, reg, labels, *intervalLabels))
	gr.Add(queryValues(ctx, client, reg, labels, *intervalValues))
	gr.Add(queryProfileTypes(ctx, client, reg, profileTypes, *intervalProfileTypes))
	gr.Add(queryQueryRange(ctx, client, reg, profileTypes, series, *intervalQueryRange))
	gr.Add(queryQuerySingle(ctx, client, reg, series, *intervalQuerySingle))
	gr.Add(queryQueryMerge(ctx, client, reg, profileTypes, series, *intervalQueryMerge))

	if err := gr.Run(); err != nil {
		log.Fatal(err)
	}
}

func internalServer(reg *prometheus.Registry, addr string) (func() error, func(error)) {
	handler := http.NewServeMux()
	handler.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	server := http.Server{
		Addr:    addr,
		Handler: handler,
	}

	execute := func() error {
		log.Println("Running http server", addr)
		return server.ListenAndServe()
	}
	interrupt := func(err error) {
		_ = server.Shutdown(context.Background())
	}

	return execute, interrupt
}

type LabelStore interface {
	Store(string, struct{})
	Range(func(string, struct{}) bool)
}

func queryLabels(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	reg *prometheus.Registry,
	labelsStore LabelStore,
	interval time.Duration,
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_labels_seconds",
		Help:    "The seconds it takes to make Labels requests against a Parca",
		Buckets: []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1},
	}, []string{"grpc_code"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(interval)
	execute := func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				start := time.Now()
				resp, err := client.Labels(ctx, connect.NewRequest(&queryv1alpha1.LabelsRequest{}))
				if err != nil {
					histogram.WithLabelValues(connect.CodeOf(err).String()).Observe(time.Since(start).Seconds())
					log.Println("failed to make labels request", err)
					continue
				}
				histogram.WithLabelValues(grpcCodeOK).Observe(time.Since(start).Seconds())
				log.Printf("querying labels took %v and got %d labels\n", time.Since(start), len(resp.Msg.LabelNames))
				for _, label := range resp.Msg.LabelNames {
					labelsStore.Store(label, struct{}{})
				}
			}
		}
	}

	interrupt := func(err error) {
		ticker.Stop()
		cancel()
	}

	return execute, interrupt
}

func queryValues(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	reg *prometheus.Registry,
	labelsStore LabelStore,
	interval time.Duration,
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_values_seconds",
		Help:    "The seconds it takes to make Values requests against a Parca",
		Buckets: []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 1.25, 1.5, 1.75, 2, 3, 4, 5},
	}, []string{"grpc_code"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(interval)
	execute := func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				var labelName string
				labelsStore.Range(func(k string, v struct{}) bool {
					labelName = k
					return false
				})

				if labelName == "" {
					continue
				}

				start := time.Now()
				resp, err := client.Values(ctx, connect.NewRequest[queryv1alpha1.ValuesRequest](&queryv1alpha1.ValuesRequest{
					LabelName: labelName,
					Match:     nil,
					Start:     timestamppb.New(time.Now().Add(-1 * time.Hour)),
					End:       timestamppb.New(time.Now()),
				}))
				if err != nil {
					histogram.WithLabelValues(connect.CodeOf(err).String()).Observe(time.Since(start).Seconds())
					log.Println("failed to make values request", err)
					continue
				}
				histogram.WithLabelValues(grpcCodeOK).Observe(time.Since(start).Seconds())
				log.Printf("querying values took %v for %s and got %d values\n", time.Since(start), labelName, len(resp.Msg.LabelValues))
			}
		}
	}

	interrupt := func(err error) {
		ticker.Stop()
		cancel()
	}

	return execute, interrupt
}

type ProfileTypeStore interface {
	Store(string, struct{})
	Range(func(string, struct{}) bool)
}

func queryProfileTypes(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	reg *prometheus.Registry,
	profileTypes ProfileTypeStore,
	interval time.Duration,
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_profiletypes_seconds",
		Help:    "The seconds it takes to make ProfileTypes requests against a Parca",
		Buckets: []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 2, 3, 4, 5},
	}, []string{"grpc_code"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(interval)
	execute := func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				start := time.Now()
				resp, err := client.ProfileTypes(ctx, connect.NewRequest[queryv1alpha1.ProfileTypesRequest](nil))
				if err != nil {
					histogram.WithLabelValues(connect.CodeOf(err).String()).Observe(time.Since(start).Seconds())
					log.Println("failed to make profiles types request", err)
					continue
				}
				histogram.WithLabelValues(grpcCodeOK).Observe(time.Since(start).Seconds())
				log.Printf("querying profile types took %v and got %d types\n", time.Since(start), len(resp.Msg.Types))

				types := resp.Msg.GetTypes()
				if len(types) == 0 {
					log.Println("no profile types")
				}
				for _, pt := range types {
					key := fmt.Sprintf("%s:%s:%s:%s:%s", pt.Name, pt.SampleType, pt.SampleUnit, pt.PeriodType, pt.PeriodUnit)
					if pt.Delta {
						key += ":delta"
					}
					profileTypes.Store(key, struct{}{})
				}
			}
		}
	}

	interrupt := func(err error) {
		ticker.Stop()
		cancel()
	}

	return execute, interrupt
}

type SeriesStore interface {
	Store(string, []*queryv1alpha1.MetricsSample)
	Range(func(string, []*queryv1alpha1.MetricsSample) bool)
}

func queryQueryRange(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	reg *prometheus.Registry,
	profileTypesStore ProfileTypeStore,
	seriesStore SeriesStore,
	interval time.Duration,
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_queryrange_seconds",
		Help:    "The seconds it takes to make QueryRange requests against a Parca",
		Buckets: []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 1.25, 1.5, 1.75, 2, 2.5, 3, 3.5, 4, 4.5, 5, 6, 7, 8, 9, 10},
	}, []string{"grpc_code", "range"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(interval)

	execute := func() error {
	executeLoop:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				pt := ""
				profileTypesStore.Range(func(k string, _ struct{}) bool {
					pt = k
					return false
				})

				if pt == "" {
					continue
				}

				// Request query ranges from shortest to longest: 15m, 1w.
				for _, tr := range []time.Duration{
					15 * time.Minute,
					7 * 24 * time.Hour,
				} {
					end := time.Now()
					start := end.Add(-1 * tr) // -1 to go back in time

					timeRange := queryRange(ctx, client, histogram, pt, start, end, seriesStore)

					// If the longestDuration is fewer than 90% of this requested time range,
					// we don't query for the next longer time range.
					if timeRange.Seconds() < end.Sub(start).Seconds()*0.9 {
						continue executeLoop
					}
				}
			}
		}
	}

	interrupt := func(err error) {
		ticker.Stop()
		cancel()
	}

	return execute, interrupt
}

func queryRange(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	histogram *prometheus.HistogramVec,
	query string,
	start, end time.Time,
	seriesStore SeriesStore,
) time.Duration {
	begin := time.Now()
	resp, err := client.QueryRange(ctx, connect.NewRequest[queryv1alpha1.QueryRangeRequest](&queryv1alpha1.QueryRangeRequest{
		Query: query,
		Start: timestamppb.New(start),
		End:   timestamppb.New(end),
	}))
	if err != nil {
		histogram.WithLabelValues(connect.CodeOf(err).String(), end.Sub(start).String()).Observe(time.Since(begin).Seconds())
		log.Println(err)
		return 0
	}
	histogram.WithLabelValues(grpcCodeOK, end.Sub(start).String()).Observe(time.Since(begin).Seconds())
	log.Printf("querying range took %v for %s %s and got %d series\n", time.Since(begin), query, end.Sub(start).String(), len(resp.Msg.Series))

	var longestTimeRange time.Duration

	for _, series := range resp.Msg.Series {
		lset := labels.Labels{}
		for _, l := range series.Labelset.Labels {
			lset = append(lset, labels.Label{Name: l.Name, Value: l.Value})
		}
		sort.Sort(lset)

		seriesStore.Store(query+lset.String(), series.Samples)

		first := series.Samples[0]
		last := series.Samples[len(series.Samples)-1]
		timerange := last.Timestamp.AsTime().Sub(first.Timestamp.AsTime())
		if timerange > longestTimeRange {
			longestTimeRange = timerange
		}
	}

	return longestTimeRange
}

func queryQuerySingle(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	reg *prometheus.Registry,
	seriesStore SeriesStore,
	interval time.Duration,
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:        "parca_client_query_seconds",
		Help:        "The seconds it takes to make Query requests against a Parca",
		ConstLabels: map[string]string{"mode": "single"},
		Buckets:     []float64{0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 2, 3, 4, 5},
	}, []string{"grpc_code", "report_type", "range"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(interval)
	execute := func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				var (
					series  string
					samples []*queryv1alpha1.MetricsSample
				)
				seriesStore.Range(func(k string, v []*queryv1alpha1.MetricsSample) bool {
					series = k
					samples = v
					return false
				})

				if series == "" || len(samples) == 0 {
					continue
				}
				sample := samples[rand.Intn(len(samples))]

				reportType := queryv1alpha1.QueryRequest_ReportType(rand.Intn(2))

				querySingle(
					ctx,
					client,
					histogram,
					series,
					sample.Timestamp,
					reportType,
				)

				// If we query a flame graph, we want to not only query
				// the deprecated old flame graphs but the newer FLAMEGRAPH_TABLE
				// The query should be the same so that comparing both implementations makes sense.
				if reportType == 0 {
					querySingle(
						ctx,
						client,
						histogram,
						series,
						sample.Timestamp,
						queryv1alpha1.QueryRequest_REPORT_TYPE_FLAMEGRAPH_TABLE,
					)
				}
			}
		}
	}

	interrupt := func(err error) {
		ticker.Stop()
		cancel()
	}

	return execute, interrupt
}

func querySingle(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	histogram *prometheus.HistogramVec,
	query string,
	ts *timestamppb.Timestamp,
	report queryv1alpha1.QueryRequest_ReportType,
) {
	start := time.Now()

	_, err := client.Query(ctx, connect.NewRequest(&queryv1alpha1.QueryRequest{
		Mode: queryv1alpha1.QueryRequest_MODE_SINGLE_UNSPECIFIED,
		Options: &queryv1alpha1.QueryRequest_Single{
			Single: &queryv1alpha1.SingleProfile{
				Query: query,
				Time:  ts,
			},
		},
		ReportType: report,
	}))
	if err != nil {
		histogram.WithLabelValues(
			connect.CodeOf(err).String(),
			report.String(),
			"",
		).Observe(time.Since(start).Seconds())
		log.Println(err)
		return
	}

	histogram.WithLabelValues(
		grpcCodeOK,
		report.String(),
		"",
	).Observe(time.Since(start).Seconds())
	log.Printf("querying single %s took %v for %s\n", report.String(), time.Since(start), query)
}

func queryQueryMerge(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	reg *prometheus.Registry,
	profileTypesStore ProfileTypeStore,
	seriesStore SeriesStore,
	interval time.Duration,
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:        "parca_client_query_seconds",
		Help:        "The seconds it takes to make Query requests against a Parca",
		ConstLabels: map[string]string{"mode": "merge"},
		Buckets: []float64{
			0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11,
			12, 13, 14, 15, 16, 17, 18, 19, 20,
		},
	}, []string{"grpc_code", "report_type", "range"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(interval)
	execute := func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				var (
					samples []*queryv1alpha1.MetricsSample

					timeRange time.Duration
					last      time.Time
				)

				pt := ""
				profileTypesStore.Range(func(k string, _ struct{}) bool {
					pt = k
					return false
				})
				if pt == "" {
					continue
				}

				// Retry until we find samples with more than two timestamps.
				for retries := 0; retries < 100; retries++ {
					seriesStore.Range(func(k string, v []*queryv1alpha1.MetricsSample) bool {
						samples = v
						return false
					})

					if len(samples) < 2 {
						continue
					}

					first := samples[0].Timestamp.AsTime()
					last = samples[len(samples)-1].Timestamp.AsTime()
					timeRange = last.Sub(first)

					if timeRange > 0 {
						break
					}
				}
				if !(timeRange > 0) {
					// Time range not found, try again.
					continue
				}

				reportType := queryv1alpha1.QueryRequest_ReportType(rand.Intn(2))

				// Request merges over 15 minutes and 7 days.
				for _, tr := range []time.Duration{
					15 * time.Minute,
					7 * 24 * time.Hour,
				} {
					if timeRange < tr {
						log.Println(
							"skipping query merge request for time range",
							tr,
							"due to not enough data. Current range is:",
							timeRange,
						)
						break
					}

					end := last
					start := end.Add(-1 * tr) // -1 to go back in time

					queryMerge(
						ctx,
						client,
						histogram,
						pt,
						start,
						end,
						reportType,
					)

					// If we query a flame graph, we want to not only query
					// the deprecated old flame graphs but the newer FLAMEGRAPH_TABLE.
					// The query should be the same so that comparing both implementations makes sense.
					if reportType == 0 {
						queryMerge(
							ctx,
							client,
							histogram,
							pt,
							start,
							end,
							queryv1alpha1.QueryRequest_REPORT_TYPE_FLAMEGRAPH_TABLE,
						)
					}
				}
			}
		}
	}

	interrupt := func(err error) {
		cancel()
		ticker.Stop()
	}

	return execute, interrupt
}

func queryMerge(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	histogram *prometheus.HistogramVec,
	query string,
	start time.Time,
	end time.Time,
	report queryv1alpha1.QueryRequest_ReportType,
) {
	reqStart := time.Now()
	_, err := client.Query(ctx, connect.NewRequest(&queryv1alpha1.QueryRequest{
		Mode: queryv1alpha1.QueryRequest_MODE_MERGE,
		Options: &queryv1alpha1.QueryRequest_Merge{
			Merge: &queryv1alpha1.MergeProfile{
				Query: query,
				Start: timestamppb.New(start),
				End:   timestamppb.New(end),
			},
		},
		ReportType: report,
	}))
	if err != nil {
		histogram.WithLabelValues(
			connect.CodeOf(err).String(),
			report.String(),
			end.Sub(start).String(),
		).Observe(time.Since(reqStart).Seconds())

		log.Println("failed to make query merge request", err)
		return
	}

	histogram.WithLabelValues(
		grpcCodeOK,
		report.String(),
		end.Sub(start).String(),
	).Observe(time.Since(reqStart).Seconds())

	log.Printf("querying merge %s took %v for %s\n", report.String(), time.Since(reqStart), query)
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
