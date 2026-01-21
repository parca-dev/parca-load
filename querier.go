package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/cenkalti/backoff/v4"
	"golang.org/x/sync/errgroup"
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

// profileTypeToString converts a ProfileType proto to its string representation.
func profileTypeToString(pt *queryv1alpha1.ProfileType) string {
	s := fmt.Sprintf("%s:%s:%s:%s:%s", pt.Name, pt.SampleType, pt.SampleUnit, pt.PeriodType, pt.PeriodUnit)
	if pt.Delta {
		s += ":delta"
	}
	return s
}

type querierMetrics struct {
	labelsHistogram       *prometheus.HistogramVec
	valuesHistogram       *prometheus.HistogramVec
	profileTypesHistogram *prometheus.HistogramVec
	rangeHistogram        *prometheus.HistogramVec
	mergeHistogram        *prometheus.HistogramVec
	labelsCounter         *prometheus.CounterVec
	valuesCounter         *prometheus.CounterVec
	profileTypesCounter   *prometheus.CounterVec
	rangeCounter          *prometheus.CounterVec
	mergeCounter          *prometheus.CounterVec
}

type Querier struct {
	cancel context.CancelFunc
	done   chan struct{}

	metrics querierMetrics

	client queryv1alpha1connect.QueryServiceClient

	profileTypes []string
	// valuesForLabels are label names to query values for (from flag).
	valuesForLabels []string

	// queryTimeRanges are used by range and merge queries.
	queryTimeRanges []time.Duration
	// labelSelectors are appended to profile types for filtering queries.
	labelSelectors []string
}

func NewQuerier(
	reg *prometheus.Registry,
	client queryv1alpha1connect.QueryServiceClient,
	queryTimeRangesConf []time.Duration,
	labelSelectors []string,
	profileTypes []string,
	valuesForLabels []string,
) *Querier {
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
				[]string{"grpc_code", "label"},
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
			labelsCounter: promauto.With(reg).NewCounterVec(
				prometheus.CounterOpts{
					Name: "parca_client_labels_total",
					Help: "Total number of Labels requests against Parca",
				},
				[]string{"grpc_code"},
			),
			valuesCounter: promauto.With(reg).NewCounterVec(
				prometheus.CounterOpts{
					Name: "parca_client_values_total",
					Help: "Total number of Values requests against Parca",
				},
				[]string{"grpc_code", "label"},
			),
			profileTypesCounter: promauto.With(reg).NewCounterVec(
				prometheus.CounterOpts{
					Name: "parca_client_profiletypes_total",
					Help: "Total number of ProfileTypes requests against Parca",
				},
				[]string{"grpc_code"},
			),
			rangeCounter: promauto.With(reg).NewCounterVec(
				prometheus.CounterOpts{
					Name: "parca_client_queryrange_total",
					Help: "Total number of QueryRange requests against Parca",
				},
				[]string{"grpc_code", "range", "labels"},
			),
			mergeCounter: promauto.With(reg).NewCounterVec(
				prometheus.CounterOpts{
					Name:        "parca_client_query_total",
					Help:        "Total number of Query requests against Parca",
					ConstLabels: map[string]string{"mode": "merge"},
				},
				[]string{"grpc_code", "range", "labels"},
			),
		},
		client:          client,
		queryTimeRanges: queryTimeRangesConf,
		labelSelectors:  labelSelectors,
		profileTypes:    profileTypes,
		valuesForLabels: valuesForLabels,
	}
}

func (q *Querier) Run(ctx context.Context, interval time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	q.cancel = cancel

	defer close(q.done)

	// If no profile types configured, discover them from the backend.
	if len(q.profileTypes) == 0 {
		// Use the longest query time range to discover all profile types.
		tr := q.queryTimeRanges[len(q.queryTimeRanges)-1]
		types, latency, err := q.fetchProfileTypes(ctx, tr)
		if err != nil {
			log.Printf("failed to discover profile types: %v\n", err)
			return
		}
		if len(types) == 0 {
			log.Println("no profile types found on backend")
			return
		}
		q.profileTypes = make([]string, 0, len(types))
		for _, pt := range types {
			q.profileTypes = append(q.profileTypes, profileTypeToString(pt))
		}
		log.Printf("discovered %d profile types(over=%s) in %v: %v\n", len(q.profileTypes), tr, latency, q.profileTypes)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	run := func() {
		g, ctx := errgroup.WithContext(ctx)

		g.Go(
			func() error {
				q.queryProfileTypes(ctx, interval)
				return nil
			},
		)
		g.Go(
			func() error {
				q.queryLabels(ctx, interval)
				return nil
			},
		)
		g.Go(
			func() error {
				q.queryValues(ctx, interval)
				return nil
			},
		)
		g.Go(
			func() error {
				q.queryRange(ctx)
				return nil
			},
		)
		g.Go(
			func() error {
				q.queryMerge(ctx)
				return nil
			},
		)

		if err := g.Wait(); err != nil {
			log.Printf("query error: %v\n", err)
		}
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

// fetchProfileTypes executes the ProfileTypes API call and returns the results.
func (q *Querier) fetchProfileTypes(ctx context.Context, tr time.Duration) (
	[]*queryv1alpha1.ProfileType,
	time.Duration,
	error,
) {
	rangeEnd := time.Now()
	rangeStart := rangeEnd.Add(-1 * tr)

	queryStart := time.Now()
	resp, err := q.client.ProfileTypes(
		ctx, connect.NewRequest(
			&queryv1alpha1.ProfileTypesRequest{
				Start: timestamppb.New(rangeStart),
				End:   timestamppb.New(rangeEnd),
			},
		),
	)
	latency := time.Since(queryStart)
	if err != nil {
		q.metrics.profileTypesHistogram.WithLabelValues(connect.CodeOf(err).String()).Observe(latency.Seconds())
		q.metrics.profileTypesCounter.WithLabelValues(connect.CodeOf(err).String()).Inc()
		return nil, latency, err
	}
	q.metrics.profileTypesHistogram.WithLabelValues(grpcCodeOK).Observe(latency.Seconds())
	q.metrics.profileTypesCounter.WithLabelValues(grpcCodeOK).Inc()
	return resp.Msg.Types, latency, nil
}

func (q *Querier) queryLabels(ctx context.Context, interval time.Duration) {
	for _, profileType := range q.profileTypes {
		for _, tr := range q.queryTimeRanges {
			rangeEnd := time.Now()
			rangeStart := rangeEnd.Add(-1 * tr)

			pt := profileType
			var resp *connect.Response[queryv1alpha1.LabelsResponse]
			var count int
			operation := func() (err error) {
				defer func() { count++ }()

				queryStart := time.Now()
				req := &queryv1alpha1.LabelsRequest{
					Start:       timestamppb.New(rangeStart),
					End:         timestamppb.New(rangeEnd),
					ProfileType: &pt,
				}
				resp, err = q.client.Labels(ctx, connect.NewRequest(req))
				latency := time.Since(queryStart)
				if err != nil {
					q.metrics.labelsHistogram.WithLabelValues(connect.CodeOf(err).String()).Observe(latency.Seconds())
					q.metrics.labelsCounter.WithLabelValues(connect.CodeOf(err).String()).Inc()
					log.Printf("labels(type=%s,over=%s): failed to make request %d: %v\n", pt, tr, count, err)
					return
				}
				q.metrics.labelsHistogram.WithLabelValues(grpcCodeOK).Observe(latency.Seconds())
				q.metrics.labelsCounter.WithLabelValues(grpcCodeOK).Inc()
				log.Printf(
					"labels(type=%s,over=%s): took %v and got %d results\n",
					pt,
					tr,
					latency,
					len(resp.Msg.LabelNames),
				)

				return nil
			}

			exp := backoff.NewExponentialBackOff()
			exp.MaxElapsedTime = interval
			if err := backoff.Retry(operation, backoff.WithContext(exp, ctx)); err != nil {
				continue
			}
		}
	}
}

func (q *Querier) queryValues(ctx context.Context, interval time.Duration) {
	if len(q.valuesForLabels) == 0 {
		return
	}

	for _, label := range q.valuesForLabels {
		for _, profileType := range q.profileTypes {
			for _, tr := range q.queryTimeRanges {
				rangeEnd := time.Now()
				rangeStart := rangeEnd.Add(-1 * tr)

				pt := profileType
				lbl := label
				var resp *connect.Response[queryv1alpha1.ValuesResponse]
				var count int
				operation := func() (err error) {
					defer func() { count++ }()
					queryStart := time.Now()
					req := &queryv1alpha1.ValuesRequest{
						LabelName:   lbl,
						Match:       nil,
						Start:       timestamppb.New(rangeStart),
						End:         timestamppb.New(rangeEnd),
						ProfileType: &pt,
					}
					resp, err = q.client.Values(ctx, connect.NewRequest(req))
					latency := time.Since(queryStart)
					if err != nil {
						q.metrics.valuesHistogram.WithLabelValues(connect.CodeOf(err).String(), lbl).Observe(latency.Seconds())
						q.metrics.valuesCounter.WithLabelValues(connect.CodeOf(err).String(), lbl).Inc()
						log.Printf(
							"values(label=%s,type=%s,over=%s): failed to make request %d: %v\n",
							lbl,
							pt,
							tr,
							count,
							err,
						)
						return
					}
					q.metrics.valuesHistogram.WithLabelValues(grpcCodeOK, lbl).Observe(latency.Seconds())
					q.metrics.valuesCounter.WithLabelValues(grpcCodeOK, lbl).Inc()
					log.Printf(
						"values(label=%s,type=%s,over=%s): took %v and got %d results\n",
						lbl,
						pt,
						tr,
						latency,
						len(resp.Msg.LabelValues),
					)

					return nil
				}

				exp := backoff.NewExponentialBackOff()
				exp.MaxElapsedTime = interval
				if err := backoff.Retry(operation, backoff.WithContext(exp, ctx)); err != nil {
					continue
				}
			}
		}
	}
}

func (q *Querier) queryProfileTypes(ctx context.Context, interval time.Duration) {
	for _, tr := range q.queryTimeRanges {
		var count int
		operation := func() error {
			defer func() { count++ }()

			types, latency, err := q.fetchProfileTypes(ctx, tr)
			if err != nil {
				log.Printf("profile_types(over=%s): failed to make request %d: %v\n", tr, count, err)
				return err
			}
			log.Printf("profile_types(over=%s): took %v and got %d types\n", tr, latency, len(types))
			return nil
		}

		exp := backoff.NewExponentialBackOff()
		exp.MaxElapsedTime = interval
		if err := backoff.Retry(operation, backoff.WithContext(exp, ctx)); err != nil {
			continue
		}
	}
}

func (q *Querier) queryRange(ctx context.Context) {
	for _, profileType := range q.profileTypes {
		for _, tr := range q.queryTimeRanges {
			for _, labelSelector := range q.labelSelectors {
				rangeEnd := time.Now()
				rangeStart := rangeEnd.Add(-1 * tr)

				query := profileType
				if labelSelector != "all" {
					query = profileType + labelSelector
				}

				queryStart := time.Now()
				resp, err := q.client.QueryRange(
					ctx, connect.NewRequest(
						&queryv1alpha1.QueryRangeRequest{
							Query: query,
							Start: timestamppb.New(rangeStart),
							End:   timestamppb.New(rangeEnd),
							Step:  durationpb.New(time.Duration(tr.Nanoseconds() / numHorizontalPixelsOn8KDisplay)),
						},
					),
				)
				latency := time.Since(queryStart)
				if err != nil {
					q.metrics.rangeHistogram.WithLabelValues(
						connect.CodeOf(err).String(), tr.String(), labelSelector,
					).Observe(latency.Seconds())
					q.metrics.rangeCounter.WithLabelValues(
						connect.CodeOf(err).String(), tr.String(), labelSelector,
					).Inc()
					log.Printf(
						"range(query=%s,over=%s,labels=%s): failed to make request: %v\n",
						query,
						tr,
						labelSelector,
						err,
					)
					continue
				}

				q.metrics.rangeHistogram.WithLabelValues(
					grpcCodeOK, tr.String(),
					labelSelector,
				).Observe(latency.Seconds())
				q.metrics.rangeCounter.WithLabelValues(
					grpcCodeOK,
					tr.String(),
					labelSelector,
				).Inc()
				log.Printf(
					"range(query=%s,over=%s,labels=%s): took %s and got %d series\n",
					query, tr, labelSelector, latency, len(resp.Msg.Series),
				)
			}
		}
	}
}

func (q *Querier) queryMerge(ctx context.Context) {
	for _, profileType := range q.profileTypes {
		for _, tr := range q.queryTimeRanges {
			for _, labelSelector := range q.labelSelectors {
				rangeEnd := time.Now()
				rangeStart := rangeEnd.Add(-1 * tr)

				query := profileType
				if labelSelector != "all" {
					query = profileType + labelSelector
				}

				queryStart := time.Now()
				_, err := q.client.Query(
					ctx, connect.NewRequest(
						&queryv1alpha1.QueryRequest{
							Mode: queryv1alpha1.QueryRequest_MODE_MERGE,
							Options: &queryv1alpha1.QueryRequest_Merge{
								Merge: &queryv1alpha1.MergeProfile{
									Query: query,
									Start: timestamppb.New(rangeStart),
									End:   timestamppb.New(rangeEnd),
								},
							},
							ReportType:        queryv1alpha1.QueryRequest_REPORT_TYPE_FLAMEGRAPH_ARROW,
							NodeTrimThreshold: &nodeTrimThreshold,
						},
					),
				)
				latency := time.Since(queryStart)
				if err != nil {
					q.metrics.mergeHistogram.WithLabelValues(
						connect.CodeOf(err).String(), tr.String(),
						labelSelector,
					).Observe(latency.Seconds())
					q.metrics.mergeCounter.WithLabelValues(
						connect.CodeOf(err).String(),
						tr.String(),
						labelSelector,
					).Inc()

					log.Printf(
						"merge(query=%s,over=%s,labels=%s): failed to make request: %v\n",
						query, tr, labelSelector, err,
					)
					continue
				}

				q.metrics.mergeHistogram.WithLabelValues(
					grpcCodeOK, tr.String(),
					labelSelector,
				).Observe(latency.Seconds())
				q.metrics.mergeCounter.WithLabelValues(
					grpcCodeOK,
					tr.String(),
					labelSelector,
				).Inc()

				log.Printf(
					"merge(query=%s,over=%s,labels=%s): took %s\n",
					query, tr, labelSelector, latency,
				)
			}
		}
	}
}
