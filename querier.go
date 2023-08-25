package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"buf.build/gen/go/parca-dev/parca/connectrpc/go/parca/query/v1alpha1/queryv1alpha1connect"
	queryv1alpha1 "buf.build/gen/go/parca-dev/parca/protocolbuffers/go/parca/query/v1alpha1"
	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/model/labels"
	"golang.org/x/exp/maps"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const numHorizontalPixelsOn8KDisplay = 7680

// Trim anything that can't be displayed by an 8K display (7680 horizontal
// pixels). This is a var because the proto request requires an address.
var nodeTrimThreshold = float32(1) / numHorizontalPixelsOn8KDisplay

type querierMetrics struct {
	labelsHistogram       *prometheus.HistogramVec
	valuesHistogram       *prometheus.HistogramVec
	profileTypesHistogram *prometheus.HistogramVec
	rangeHistogram        *prometheus.HistogramVec
	singleHistogram       *prometheus.HistogramVec
	mergeHistogram        *prometheus.HistogramVec
}

type timestampRange struct {
	first time.Time
	last  time.Time
}

type Querier struct {
	cancel context.CancelFunc
	done   chan struct{}

	metrics querierMetrics

	client queryv1alpha1connect.QueryServiceClient

	rng *rand.Rand

	labels       []string
	profileTypes []string
	series       map[string]timestampRange

	// queryTimeRanges are used by range and merge queries.
	queryTimeRanges []time.Duration
	// reportTypes are used by single and merge queries.
	reportTypes []queryv1alpha1.QueryRequest_ReportType
}

func NewQuerier(reg *prometheus.Registry, client queryv1alpha1connect.QueryServiceClient) *Querier {
	return &Querier{
		done: make(chan struct{}),
		metrics: querierMetrics{
			labelsHistogram: promauto.With(reg).NewHistogramVec(
				prometheus.HistogramOpts{
					Name:    "parca_client_labels_seconds",
					Help:    "The seconds it takes to make Labels requests against a Parca",
					Buckets: []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1},
				},
				[]string{"grpc_code"},
			),
			valuesHistogram: promauto.With(reg).NewHistogramVec(
				prometheus.HistogramOpts{
					Name:    "parca_client_values_seconds",
					Help:    "The seconds it takes to make Values requests against a Parca",
					Buckets: []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 1.25, 1.5, 1.75, 2, 3, 4, 5},
				},
				[]string{"grpc_code"},
			),
			profileTypesHistogram: promauto.With(reg).NewHistogramVec(
				prometheus.HistogramOpts{
					Name:    "parca_client_profiletypes_seconds",
					Help:    "The seconds it takes to make ProfileTypes requests against a Parca",
					Buckets: []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 2, 3, 4, 5},
				},
				[]string{"grpc_code"},
			),
			rangeHistogram: promauto.With(reg).NewHistogramVec(
				prometheus.HistogramOpts{
					Name:    "parca_client_queryrange_seconds",
					Help:    "The seconds it takes to make QueryRange requests against a Parca",
					Buckets: []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 1.25, 1.5, 1.75, 2, 2.5, 3, 3.5, 4, 4.5, 5, 6, 7, 8, 9, 10},
				},
				[]string{"grpc_code", "range"},
			),
			singleHistogram: promauto.With(reg).NewHistogramVec(
				prometheus.HistogramOpts{
					Name:        "parca_client_query_seconds",
					Help:        "The seconds it takes to make Query requests against a Parca",
					ConstLabels: map[string]string{"mode": "single"},
					Buckets:     prometheus.ExponentialBucketsRange(0.1, 120, 30),
				},
				[]string{"grpc_code", "report_type", "range"},
			),
			mergeHistogram: promauto.With(reg).NewHistogramVec(
				prometheus.HistogramOpts{
					Name:        "parca_client_query_seconds",
					Help:        "The seconds it takes to make Query requests against a Parca",
					ConstLabels: map[string]string{"mode": "merge"},
					Buckets: []float64{
						0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11,
						12, 13, 14, 15, 16, 17, 18, 19, 20,
					},
				},
				[]string{"grpc_code", "report_type", "range"},
			),
		},
		client:          client,
		rng:             rand.New(rand.NewSource(time.Now().UnixNano())),
		series:          make(map[string]timestampRange),
		queryTimeRanges: []time.Duration{15 * time.Minute, 12 * time.Hour, 7 * 24 * time.Hour},
		reportTypes: []queryv1alpha1.QueryRequest_ReportType{
			queryv1alpha1.QueryRequest_REPORT_TYPE_PPROF,
			// nolint:staticcheck
			queryv1alpha1.QueryRequest_REPORT_TYPE_FLAMEGRAPH_UNSPECIFIED,
			queryv1alpha1.QueryRequest_REPORT_TYPE_FLAMEGRAPH_TABLE,
		},
	}
}

func (q *Querier) Run(ctx context.Context, interval time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	q.cancel = cancel

	defer close(q.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Since a lot of these queries are dependent on other queries, run
			// them one at a time.
			q.queryLabels(ctx)
			q.queryValues(ctx)
			q.queryProfileTypes(ctx)
			q.queryRange(ctx)
			q.querySingle(ctx)
			q.queryMerge(ctx)
		}
	}
}

func (q *Querier) Stop() {
	q.cancel()
	<-q.done
}

func (q *Querier) queryLabels(ctx context.Context) {
	queryStart := time.Now()
	resp, err := q.client.Labels(ctx, connect.NewRequest(&queryv1alpha1.LabelsRequest{}))
	latency := time.Since(queryStart)
	if err != nil {
		q.metrics.labelsHistogram.WithLabelValues(connect.CodeOf(err).String()).Observe(latency.Seconds())
		log.Println("labels: failed to make request", err)
		return
	}
	q.metrics.labelsHistogram.WithLabelValues(grpcCodeOK).Observe(latency.Seconds())
	q.labels = append(q.labels[:0], resp.Msg.LabelNames...)
	log.Printf("labels: took %v and got %d results\n", latency, len(resp.Msg.LabelNames))
}

func (q *Querier) queryValues(ctx context.Context) {
	if len(q.labels) == 0 {
		log.Println("values: no labels to query")
		return
	}
	label := q.labels[q.rng.Intn(len(q.labels))]

	queryStart := time.Now()
	resp, err := q.client.Values(ctx, connect.NewRequest(&queryv1alpha1.ValuesRequest{
		LabelName: label,
		Match:     nil,
		Start:     timestamppb.New(time.Now().Add(-1 * time.Hour)),
		End:       timestamppb.New(time.Now()),
	}))
	latency := time.Since(queryStart)
	if err != nil {
		q.metrics.valuesHistogram.WithLabelValues(connect.CodeOf(err).String()).Observe(latency.Seconds())
		log.Printf("values(label=%s): failed to make request: %v\n", label, err)
		return
	}
	q.metrics.valuesHistogram.WithLabelValues(grpcCodeOK).Observe(latency.Seconds())
	log.Printf("values(label=%s): took %v and got %d results\n", label, latency, len(resp.Msg.LabelValues))
}

func (q *Querier) queryProfileTypes(ctx context.Context) {
	queryStart := time.Now()
	resp, err := q.client.ProfileTypes(ctx, connect.NewRequest(&queryv1alpha1.ProfileTypesRequest{}))
	latency := time.Since(queryStart)
	if err != nil {
		q.metrics.profileTypesHistogram.WithLabelValues(connect.CodeOf(err).String()).Observe(latency.Seconds())
		log.Println("profile types: failed to make request", err)
		return
	}
	q.metrics.profileTypesHistogram.WithLabelValues(grpcCodeOK).Observe(latency.Seconds())
	log.Printf("profile types: took %v and got %d types\n", latency, len(resp.Msg.Types))

	if len(resp.Msg.Types) == 0 {
		return
	}

	q.profileTypes = q.profileTypes[:0]
	for _, pt := range resp.Msg.Types {
		key := fmt.Sprintf("%s:%s:%s:%s:%s", pt.Name, pt.SampleType, pt.SampleUnit, pt.PeriodType, pt.PeriodUnit)
		if pt.Delta {
			key += ":delta"
		}
		q.profileTypes = append(q.profileTypes, key)
	}
}

func (q *Querier) queryRange(ctx context.Context) {
	if len(q.profileTypes) == 0 {
		log.Println("range: no profile types to query")
		return
	}

	profileType := q.profileTypes[q.rng.Intn(len(q.profileTypes))]

	for _, tr := range q.queryTimeRanges {
		rangeEnd := time.Now()
		rangeStart := rangeEnd.Add(-1 * tr)

		queryStart := time.Now()
		resp, err := q.client.QueryRange(ctx, connect.NewRequest(&queryv1alpha1.QueryRangeRequest{
			Query: profileType,
			Start: timestamppb.New(rangeStart),
			End:   timestamppb.New(rangeEnd),
			Step:  durationpb.New(time.Duration(tr.Nanoseconds() / numHorizontalPixelsOn8KDisplay)),
		}))
		latency := time.Since(queryStart)
		if err != nil {
			q.metrics.rangeHistogram.WithLabelValues(
				connect.CodeOf(err).String(), tr.String(),
			).Observe(latency.Seconds())
			log.Printf("range(type=%s,over=%s): failed to make request: %v\n", profileType, tr, err)
			continue
		}

		q.metrics.rangeHistogram.WithLabelValues(grpcCodeOK, tr.String()).Observe(latency.Seconds())
		log.Printf(
			"range(type=%s,over=%s): took %s and got %d series\n",
			profileType, tr, latency, len(resp.Msg.Series),
		)

		for k := range q.series {
			delete(q.series, k)
		}
		for _, series := range resp.Msg.Series {
			if len(series.Samples) < 2 {
				continue
			}
			lSet := labels.Labels{}
			for _, l := range series.Labelset.Labels {
				lSet = append(lSet, labels.Label{Name: l.Name, Value: l.Value})
			}
			sort.Sort(lSet)
			q.series[profileType+lSet.String()] = timestampRange{
				first: series.Samples[0].Timestamp.AsTime(),
				last:  series.Samples[len(series.Samples)-1].Timestamp.AsTime(),
			}
		}
	}
}

func (q *Querier) querySingle(ctx context.Context) {
	if len(q.series) == 0 {
		log.Println("single: no series to query")
		return
	}
	seriesKeys := maps.Keys(q.series)
	series := seriesKeys[q.rng.Intn(len(seriesKeys))]
	sampleTs := q.series[series].first

	for _, reportType := range q.reportTypes {
		queryStart := time.Now()
		_, err := q.client.Query(ctx, connect.NewRequest(&queryv1alpha1.QueryRequest{
			Mode: queryv1alpha1.QueryRequest_MODE_SINGLE_UNSPECIFIED,
			Options: &queryv1alpha1.QueryRequest_Single{
				Single: &queryv1alpha1.SingleProfile{
					Query: series,
					Time:  timestamppb.New(sampleTs),
				},
			},
			ReportType: reportType,
		}))
		latency := time.Since(queryStart)
		if err != nil {
			q.metrics.singleHistogram.WithLabelValues(
				connect.CodeOf(err).String(),
				reportType.String(),
				"",
			).Observe(latency.Seconds())
			log.Printf("single(series=%s, reportType=%s): failed to make request: %v\n", series, reportType, err)
			continue
		}

		q.metrics.singleHistogram.WithLabelValues(
			grpcCodeOK,
			reportType.String(),
			"",
		).Observe(latency.Seconds())
		log.Printf("single(series=%s, reportType=%s): took %v\n", series, reportType, latency)
	}
}

func (q *Querier) queryMerge(ctx context.Context) {
	if len(q.profileTypes) == 0 {
		log.Println("merge: no profile types to query")
		return
	}

	profileType := q.profileTypes[q.rng.Intn(len(q.profileTypes))]
	for _, tr := range q.queryTimeRanges {
		if tr > 24*time.Hour {
			// TODO(asubiotto): This currently OOMs frostdb. Gradually remove
			// this skip.
			continue
		}
		rangeEnd := time.Now()
		rangeStart := rangeEnd.Add(-1 * tr)

		for _, reportType := range q.reportTypes {
			queryStart := time.Now()
			_, err := q.client.Query(ctx, connect.NewRequest(&queryv1alpha1.QueryRequest{
				Mode: queryv1alpha1.QueryRequest_MODE_MERGE,
				Options: &queryv1alpha1.QueryRequest_Merge{
					Merge: &queryv1alpha1.MergeProfile{
						Query: profileType,
						Start: timestamppb.New(rangeStart),
						End:   timestamppb.New(rangeEnd),
					},
				},
				ReportType:        reportType,
				NodeTrimThreshold: &nodeTrimThreshold,
			}))
			latency := time.Since(queryStart)
			if err != nil {
				q.metrics.mergeHistogram.WithLabelValues(
					connect.CodeOf(err).String(),
					reportType.String(),
					tr.String(),
				).Observe(latency.Seconds())

				log.Printf(
					"merge(type=%s,reportType=%s,over=%s): failed to make request: %v\n",
					profileType, reportType, tr, err,
				)
				continue
			}

			q.metrics.mergeHistogram.WithLabelValues(
				grpcCodeOK,
				reportType.String(),
				tr.String(),
			).Observe(latency.Seconds())

			log.Printf(
				"merge(type=%s,reportType=%s,over=%s): took %s\n",
				profileType, reportType, tr, latency,
			)
		}
	}
}
