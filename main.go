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
	flag.Parse()

	clientOptions := []connect.ClientOption{
		connect.WithGRPCWeb(),
	}
	if *token != "" {
		clientOptions = append(clientOptions, connect.WithInterceptors(&bearerTokenInterceptor{token: *token}))
	}

	client := queryv1alpha1connect.NewQueryServiceClient(
		&http.Client{Timeout: 10 * time.Second},
		*url,
		clientOptions...,
	)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	labels := sync.NewExpireMap[string, struct{}](time.Minute)
	profileTypes := sync.NewExpireMap[string, struct{}](time.Minute)
	series := sync.NewExpireMap[string, []*queryv1alpha1.MetricsSample](30 * time.Second)

	var gr run.Group

	gr.Add(run.SignalHandler(ctx, os.Interrupt, os.Kill))
	gr.Add(internalServer(reg, *addr))

	gr.Add(queryLabels(ctx, client, reg, labels))
	gr.Add(queryValues(ctx, client, reg, labels))
	gr.Add(queryProfileTypes(ctx, client, reg, profileTypes))
	gr.Add(queryQueryRange(ctx, client, reg, profileTypes, series))
	gr.Add(queryQuerySingle(ctx, client, reg, series))
	gr.Add(queryQueryMerge(ctx, client, reg, series))

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
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_labels_seconds",
		Help:    "The seconds it takes to make Labels requests against a Parca",
		Buckets: []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1},
	}, []string{"grpc_code"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(5 * time.Second)
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
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_values_seconds",
		Help:    "The seconds it takes to make Values requests against a Parca",
		Buckets: []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 1.25, 1.5, 1.75, 2, 3, 4, 5},
	}, []string{"grpc_code"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(5 * time.Second)
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
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_profiletypes_seconds",
		Help:    "The seconds it takes to make ProfileTypes requests against a Parca",
		Buckets: []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 2, 3, 4, 5},
	}, []string{"grpc_code"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(10 * time.Second)
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
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_queryrange_seconds",
		Help:    "The seconds it takes to make QueryRange requests against a Parca",
		Buckets: []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 1.25, 1.5, 1.75, 2, 2.5, 3, 3.5, 4, 4.5, 5, 6, 7, 8, 9, 10},
	}, []string{"grpc_code"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(15 * time.Second)

	execute := func() error {
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

				start := time.Now()
				resp, err := client.QueryRange(ctx, connect.NewRequest[queryv1alpha1.QueryRangeRequest](&queryv1alpha1.QueryRangeRequest{
					Query: pt,
					Start: timestamppb.New(time.Now().Add(-15 * time.Minute)),
					End:   timestamppb.New(time.Now()),
				}))
				if err != nil {
					histogram.WithLabelValues(connect.CodeOf(err).String()).Observe(time.Since(start).Seconds())
					log.Println(err)
					continue
				}
				histogram.WithLabelValues(grpcCodeOK).Observe(time.Since(start).Seconds())
				log.Printf("querying range took %v for %s and got %d series\n", time.Since(start), pt, len(resp.Msg.Series))

				for _, series := range resp.Msg.Series {
					lset := labels.Labels{}
					for _, l := range series.Labelset.Labels {
						lset = append(lset, labels.Label{Name: l.Name, Value: l.Value})
					}
					sort.Sort(lset)

					seriesStore.Store(pt+lset.String(), series.Samples)
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

func queryQuerySingle(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	reg *prometheus.Registry,
	seriesStore SeriesStore,
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:        "parca_client_query_seconds",
		Help:        "The seconds it takes to make Query requests against a Parca",
		ConstLabels: map[string]string{"mode": "single"},
		Buckets:     []float64{0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 2, 3, 4, 5},
	}, []string{"grpc_code", "report_type", "range"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(10 * time.Second)
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
	log.Printf("querying %s took %v for %s\n", report.String(), time.Since(start), query)
}

func queryQueryMerge(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	reg *prometheus.Registry,
	seriesStore SeriesStore,
) (func() error, func(error)) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:        "parca_client_query_seconds",
		Help:        "The seconds it takes to make Query requests against a Parca",
		ConstLabels: map[string]string{"mode": "merge"},
		Buckets:     []float64{0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 2, 3, 4, 5},
	}, []string{"grpc_code", "report_type", "range"})

	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(15 * time.Second)
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

				first := samples[0]
				last := samples[len(samples)-1]
				timerange := last.Timestamp.AsTime().Sub(first.Timestamp.AsTime())

				end := last.Timestamp.AsTime()

				var start time.Time
				if timerange > 15*time.Minute {
					start = end.Add(-15 * time.Minute)
				} else if timerange > 5*time.Minute {
					start = end.Add(-5 * time.Minute)
				} else if timerange > time.Minute {
					start = end.Add(-1 * time.Minute)
				} else {
					// Too little data we'll ignore it and not make a merge request this time.
					continue
				}

				reportType := queryv1alpha1.QueryRequest_ReportType(rand.Intn(2))

				queryMerge(
					ctx,
					client,
					histogram,
					series,
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
						series,
						start,
						end,
						queryv1alpha1.QueryRequest_REPORT_TYPE_FLAMEGRAPH_TABLE,
					)
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

		log.Println(err)
		return
	}

	histogram.WithLabelValues(
		grpcCodeOK,
		report.String(),
		end.Sub(start).String(),
	).Observe(time.Since(reqStart).Seconds())

	log.Printf("querying %s took %v for %s\n", report.String(), time.Since(reqStart), query)
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
