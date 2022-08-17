package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"time"

	"github.com/bufbuild/connect-go"
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

const grpcCodeOK = "OK"

func main() {
	rand.Seed(time.Now().UnixNano())

	url := flag.String("url", "http://localhost:7070", "The URL for the Parca instance to query")
	addr := flag.String("addr", "127.0.0.1:7171", "The address the HTTP server binds to")
	flag.Parse()

	client := queryv1alpha1connect.NewQueryServiceClient(
		&http.Client{Timeout: 10 * time.Second},
		*url,
		connect.WithGRPCWeb(),
	)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	labels := sync.NewExpireMap[string, struct{}](time.Minute)
	profileTypes := sync.NewExpireMap[string, struct{}](time.Minute)
	series := sync.NewExpireMap[string, []*queryv1alpha1.MetricsSample](30 * time.Second)

	go func() {
		s := make(chan os.Signal)
		signal.Notify(s, os.Interrupt, os.Kill)
		<-s
		stop()
	}()

	go queryLabels(ctx, client, reg, labels)
	go queryValues(ctx, client, reg, labels)

	go queryProfileTypes(ctx, client, reg, profileTypes)
	go queryQueryRange(ctx, client, reg, profileTypes, series)
	go queryQuerySingle(ctx, client, reg, series)

	go internalServer(ctx, reg, *addr)

	<-ctx.Done()
}

func internalServer(ctx context.Context, reg *prometheus.Registry, addr string) {
	m := http.NewServeMux()
	m.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	log.Println("Running http server", addr)
	if err := http.ListenAndServe(addr, m); err != nil {
		log.Fatal(err)
	}
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
) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_labels_seconds",
		Help:    "The seconds it takes to make Labels requests against a Parca",
		Buckets: []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1},
	}, []string{"grpc_code"})

	ticker := time.NewTicker(5 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			//log.Println("querying labels...")
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

func queryValues(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	reg *prometheus.Registry,
	labelsStore LabelStore,
) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_values_seconds",
		Help:    "The seconds it takes to make Values requests against a Parca",
		Buckets: []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.55, 0.6, 0.65, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95, 1, 1.25, 1.5, 1.75, 2, 3, 4, 5},
	}, []string{"grpc_code"})

	ticker := time.NewTicker(5 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			//log.Println("querying values...")
			var labelName string
			labelsStore.Range(func(k string, v struct{}) bool {
				labelName = k
				return false
			})

			if labelName == "" {
				continue
			}

			start := time.Now()
			_, err := client.Values(ctx, connect.NewRequest[queryv1alpha1.ValuesRequest](&queryv1alpha1.ValuesRequest{
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

type ProfileTypeStore interface {
	Store(string, struct{})
	Range(func(string, struct{}) bool)
}

func queryProfileTypes(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	reg *prometheus.Registry,
	profileTypes ProfileTypeStore,
) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_profiletypes_seconds",
		Help:    "The seconds it takes to make ProfileTypes requests against a Parca",
		Buckets: []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 2, 3, 4, 5},
	}, []string{"grpc_code"})

	ticker := time.NewTicker(10 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			//log.Println("querying profile types...")
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
) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parca_client_queryrange_seconds",
		Help:    "The seconds it takes to make QueryRange requests against a Parca",
		Buckets: []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 1.25, 1.5, 1.75, 2, 2.5, 3, 3.5, 4, 4.5, 5, 6, 7, 8, 9, 10},
	}, []string{"grpc_code"})

	ticker := time.NewTicker(15 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pt := ""
			profileTypesStore.Range(func(k string, _ struct{}) bool {
				pt = k
				return false
			})

			if pt == "" {
				continue
			}

			//log.Println("querying query range...", pt)
			start := time.Now()
			resp, err := client.QueryRange(ctx, connect.NewRequest[queryv1alpha1.QueryRangeRequest](&queryv1alpha1.QueryRangeRequest{
				Query: pt,
				Start: timestamppb.New(time.Now().Add(-1 * time.Hour)),
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

func queryQuerySingle(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	reg *prometheus.Registry,
	seriesStore SeriesStore,
) {
	histogram := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name:        "parca_client_query_seconds",
		Help:        "The seconds it takes to make Query requests against a Parca",
		ConstLabels: map[string]string{"mode": "single"},
		Buckets:     []float64{0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 2, 3, 4, 5},
	}, []string{"grpc_code"})

	ticker := time.NewTicker(10 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return
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

			//log.Println("querying single...", series, reportType.String(), sample.Timestamp.Seconds)

			start := time.Now()
			_, err := client.Query(ctx, connect.NewRequest[queryv1alpha1.QueryRequest](&queryv1alpha1.QueryRequest{
				Mode: queryv1alpha1.QueryRequest_MODE_SINGLE_UNSPECIFIED,
				Options: &queryv1alpha1.QueryRequest_Single{
					Single: &queryv1alpha1.SingleProfile{
						Query: series,
						Time:  sample.Timestamp,
					},
				},
				ReportType: reportType,
			}))
			if err != nil {
				histogram.WithLabelValues(connect.CodeOf(err).String()).Observe(time.Since(start).Seconds())
				log.Println(err)
				continue
			}

			histogram.WithLabelValues(grpcCodeOK).Observe(time.Since(start).Seconds())
			log.Printf("querying %s took %v for %s\n", reportType.String(), time.Since(start), series)
		}
	}
}
