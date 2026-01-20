package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/cenkalti/backoff/v4"
	"google.golang.org/protobuf/types/known/durationpb"

	"buf.build/gen/go/parca-dev/parca/connectrpc/go/parca/query/v1alpha1/queryv1alpha1connect"
	queryv1alpha1 "buf.build/gen/go/parca-dev/parca/protocolbuffers/go/parca/query/v1alpha1"
	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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
	mergeHistogram        *prometheus.HistogramVec
}

type Querier struct {
	cancel context.CancelFunc
	done   chan struct{}

	metrics querierMetrics

	client queryv1alpha1connect.QueryServiceClient

	rng *rand.Rand

	labels       []string
	profileTypes []string

	// queryTimeRanges are used by range and merge queries.
	queryTimeRanges []time.Duration
}

func NewQuerier(reg *prometheus.Registry, client queryv1alpha1connect.QueryServiceClient, queryTimeRangesConf []time.Duration) *Querier {
	return &Querier{
		done: make(chan struct{}),
		metrics: querierMetrics{
			labelsHistogram: promauto.With(reg).NewHistogramVec(
				prometheus.HistogramOpts{
					Name:                        "parca_client_labels_seconds",
					Help:                        "The seconds it takes to make Labels requests against a Parca",
					NativeHistogramBucketFactor: 1.1,
				},
				[]string{"grpc_code"},
			),
			valuesHistogram: promauto.With(reg).NewHistogramVec(
				prometheus.HistogramOpts{
					Name:                        "parca_client_values_seconds",
					Help:                        "The seconds it takes to make Values requests against a Parca",
					NativeHistogramBucketFactor: 1.1,
				},
				[]string{"grpc_code"},
			),
			profileTypesHistogram: promauto.With(reg).NewHistogramVec(
				prometheus.HistogramOpts{
					Name:                        "parca_client_profiletypes_seconds",
					Help:                        "The seconds it takes to make ProfileTypes requests against a Parca",
					NativeHistogramBucketFactor: 1.1,
				},
				[]string{"grpc_code"},
			),
			rangeHistogram: promauto.With(reg).NewHistogramVec(
				prometheus.HistogramOpts{
					Name:                        "parca_client_queryrange_seconds",
					Help:                        "The seconds it takes to make QueryRange requests against a Parca",
					NativeHistogramBucketFactor: 1.1,
				},
				[]string{"grpc_code", "range", "labels"},
			),
			mergeHistogram: promauto.With(reg).NewHistogramVec(
				prometheus.HistogramOpts{
					Name:                        "parca_client_query_seconds",
					Help:                        "The seconds it takes to make Query requests against a Parca",
					ConstLabels:                 map[string]string{"mode": "merge"},
					NativeHistogramBucketFactor: 1.1,
				},
				[]string{"grpc_code", "range", "labels"},
			),
		},
		client:          client,
		rng:             rand.New(rand.NewSource(time.Now().UnixNano())),
		queryTimeRanges: queryTimeRangesConf,
	}
}

func (q *Querier) Run(ctx context.Context, interval time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	q.cancel = cancel

	defer close(q.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	run := func() {
		// Since a lot of these queries are dependent on other queries, run
		// them one at a time.
		q.queryProfileTypes(ctx, interval)
		q.queryLabels(ctx, interval)
		q.queryValues(ctx, interval)
		q.queryRange(ctx)
		q.queryMerge(ctx)
	}

	// Immediately run and then wait for the ticker.
	// If we don't run immediately, we'll have to wait for the first tick to run which can be a long time e.g. 30min.
	run()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func (q *Querier) Stop() {
	q.cancel()
	<-q.done
}

func (q *Querier) queryLabels(ctx context.Context, interval time.Duration) {
	// Pick a random profile type if available
	var profileType *string
	if len(q.profileTypes) > 0 {
		pt := q.profileTypes[q.rng.Intn(len(q.profileTypes))]
		profileType = &pt
	}

	for _, tr := range q.queryTimeRanges {
		rangeEnd := time.Now()
		rangeStart := rangeEnd.Add(-1 * tr)

		var resp *connect.Response[queryv1alpha1.LabelsResponse]
		var count int
		operation := func() (err error) {
			defer func() { count++ }()

			queryStart := time.Now()
			req := &queryv1alpha1.LabelsRequest{
				Start: timestamppb.New(rangeStart),
				End:   timestamppb.New(rangeEnd),
			}
			if profileType != nil {
				req.ProfileType = profileType
			}
			resp, err = q.client.Labels(ctx, connect.NewRequest(req))
			latency := time.Since(queryStart)
			if err != nil {
				q.metrics.labelsHistogram.WithLabelValues(connect.CodeOf(err).String()).Observe(latency.Seconds())
				if profileType != nil {
					log.Printf("labels(type=%s,over=%s): failed to make request %d: %v\n", *profileType, tr, count, err)
				} else {
					log.Printf("labels(over=%s): failed to make request %d: %v\n", tr, count, err)
				}
				return
			}
			q.metrics.labelsHistogram.WithLabelValues(grpcCodeOK).Observe(latency.Seconds())
			q.labels = append(q.labels[:0], resp.Msg.LabelNames...)
			if profileType != nil {
				log.Printf("labels(type=%s,over=%s): took %v and got %d results\n", *profileType, tr, latency, len(resp.Msg.LabelNames))
			} else {
				log.Printf("labels(over=%s): took %v and got %d results\n", tr, latency, len(resp.Msg.LabelNames))
			}

			return nil
		}

		exp := backoff.NewExponentialBackOff()
		exp.MaxElapsedTime = interval
		if err := backoff.Retry(operation, backoff.WithContext(exp, ctx)); err != nil {
			continue
		}
	}
}

func (q *Querier) queryValues(ctx context.Context, interval time.Duration) {
	if len(q.labels) == 0 {
		log.Println("values: no labels to query")
		return
	}
	label := q.labels[q.rng.Intn(len(q.labels))]

	// Pick a random profile type if available
	var profileType *string
	if len(q.profileTypes) > 0 {
		pt := q.profileTypes[q.rng.Intn(len(q.profileTypes))]
		profileType = &pt
	}

	for _, tr := range q.queryTimeRanges {
		rangeEnd := time.Now()
		rangeStart := rangeEnd.Add(-1 * tr)

		var resp *connect.Response[queryv1alpha1.ValuesResponse]
		var count int
		operation := func() (err error) {
			defer func() { count++ }()
			queryStart := time.Now()
			req := &queryv1alpha1.ValuesRequest{
				LabelName: label,
				Match:     nil,
				Start:     timestamppb.New(rangeStart),
				End:       timestamppb.New(rangeEnd),
			}
			if profileType != nil {
				req.ProfileType = profileType
			}
			resp, err = q.client.Values(ctx, connect.NewRequest(req))
			latency := time.Since(queryStart)
			if err != nil {
				q.metrics.valuesHistogram.WithLabelValues(connect.CodeOf(err).String()).Observe(latency.Seconds())
				if profileType != nil {
					log.Printf("values(label=%s,type=%s,over=%s): failed to make request %d: %v\n", label, *profileType, tr, count, err)
				} else {
					log.Printf("values(label=%s,over=%s): failed to make request %d: %v\n", label, tr, count, err)
				}
				return
			}
			q.metrics.valuesHistogram.WithLabelValues(grpcCodeOK).Observe(latency.Seconds())
			if profileType != nil {
				log.Printf("values(label=%s,type=%s,over=%s): took %v and got %d results\n", label, *profileType, tr, latency, len(resp.Msg.LabelValues))
			} else {
				log.Printf("values(label=%s,over=%s): took %v and got %d results\n", label, tr, latency, len(resp.Msg.LabelValues))
			}

			return nil
		}

		exp := backoff.NewExponentialBackOff()
		exp.MaxElapsedTime = interval
		if err := backoff.Retry(operation, backoff.WithContext(exp, ctx)); err != nil {
			continue
		}
	}
}

func (q *Querier) queryProfileTypes(ctx context.Context, interval time.Duration) {
	for _, tr := range q.queryTimeRanges {
		rangeEnd := time.Now()
		rangeStart := rangeEnd.Add(-1 * tr)

		var resp *connect.Response[queryv1alpha1.ProfileTypesResponse]
		var count int
		operation := func() (err error) {
			defer func() { count++ }()

			queryStart := time.Now()
			resp, err = q.client.ProfileTypes(ctx, connect.NewRequest(&queryv1alpha1.ProfileTypesRequest{
				Start: timestamppb.New(rangeStart),
				End:   timestamppb.New(rangeEnd),
			}))
			latency := time.Since(queryStart)
			if err != nil {
				q.metrics.profileTypesHistogram.WithLabelValues(connect.CodeOf(err).String()).Observe(latency.Seconds())
				log.Printf("profile types(over=%s): failed to make request %d: %v\n", tr, count, err)
				return err
			}
			q.metrics.profileTypesHistogram.WithLabelValues(grpcCodeOK).Observe(latency.Seconds())
			log.Printf("profile types(over=%s): took %v and got %d types\n", tr, latency, len(resp.Msg.Types))

			return nil
		}

		exp := backoff.NewExponentialBackOff()
		exp.MaxElapsedTime = interval
		if err := backoff.Retry(operation, backoff.WithContext(exp, ctx)); err != nil {
			continue
		}

		if len(resp.Msg.Types) == 0 {
			continue
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
				connect.CodeOf(err).String(), tr.String(), "all",
			).Observe(latency.Seconds())
			log.Printf("range(type=%s,over=%s): failed to make request: %v\n", profileType, tr, err)
			continue
		}

		q.metrics.rangeHistogram.WithLabelValues(grpcCodeOK, tr.String(), "all").Observe(latency.Seconds())
		log.Printf(
			"range(type=%s,over=%s): took %s and got %d series\n",
			profileType, tr, latency, len(resp.Msg.Series),
		)
	}
}

func (q *Querier) queryMerge(ctx context.Context) {
	if len(q.profileTypes) == 0 {
		log.Println("merge: no profile types to query")
		return
	}

	profileType := q.profileTypes[q.rng.Intn(len(q.profileTypes))]
	for _, tr := range q.queryTimeRanges {
		rangeEnd := time.Now()
		rangeStart := rangeEnd.Add(-1 * tr)

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
			ReportType:        queryv1alpha1.QueryRequest_REPORT_TYPE_FLAMEGRAPH_ARROW,
			NodeTrimThreshold: &nodeTrimThreshold,
		}))
		latency := time.Since(queryStart)
		if err != nil {
			q.metrics.mergeHistogram.WithLabelValues(
				connect.CodeOf(err).String(),
				tr.String(),
				"all",
			).Observe(latency.Seconds())

			log.Printf(
				"merge(type=%s,over=%s): failed to make request: %v\n",
				profileType, tr, err,
			)
			continue
		}

		q.metrics.mergeHistogram.WithLabelValues(
			grpcCodeOK,
			tr.String(),
			"all",
		).Observe(latency.Seconds())

		log.Printf(
			"merge(type=%s,over=%s): took %s\n",
			profileType, tr, latency,
		)
	}
}
