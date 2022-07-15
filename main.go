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
	"github.com/prometheus/prometheus/model/labels"
	queryv1alpha1 "go.buf.build/bufbuild/connect-go/parca-dev/parca/parca/query/v1alpha1"
	"go.buf.build/bufbuild/connect-go/parca-dev/parca/parca/query/v1alpha1/queryv1alpha1connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/parca-dev/parca-load/sync"
)

func main() {
	rand.Seed(time.Now().UnixNano())

	url := flag.String("url", "http://localhost:7070", "The URL for the Parca instance to query")
	flag.Parse()

	client := queryv1alpha1connect.NewQueryServiceClient(
		&http.Client{Timeout: 10 * time.Second},
		*url,
		connect.WithGRPCWeb(),
	)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	labels := sync.NewMap[string, struct{}]()
	profileTypes := sync.NewMap[string, struct{}]()
	series := sync.NewMap[string, []*queryv1alpha1.MetricsSample]()

	go func() {
		s := make(chan os.Signal)
		signal.Notify(s, os.Interrupt, os.Kill)
		<-s
		stop()
	}()

	go queryLabels(ctx, client, labels)
	go queryValues(ctx, client, labels)

	go queryProfileTypes(ctx, client, profileTypes)
	go queryQueryRange(ctx, client, profileTypes, series)
	go queryQuerySingle(ctx, client, series)

	<-ctx.Done()
}

func queryLabels(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	labelsStore *sync.Map[string, struct{}],
) {
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
				log.Println("failed to make labels request", err)
				continue
			}
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
	labelsStore *sync.Map[string, struct{}],
) {
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
			resp, err := client.Values(ctx, connect.NewRequest[queryv1alpha1.ValuesRequest](&queryv1alpha1.ValuesRequest{
				LabelName: labelName,
				Match:     nil,
				Start:     timestamppb.New(time.Now().Add(-1 * time.Hour)),
				End:       timestamppb.New(time.Now()),
			}))
			if err != nil {
				log.Println("failed to make values request", err)
				continue
			}
			log.Printf("querying values took %v for %s and got %d values\n", time.Since(start), labelName, len(resp.Msg.LabelValues))
		}
	}

}

func queryProfileTypes(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	profileTypes *sync.Map[string, struct{}],
) {
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
				log.Println("failed to make profiles types request", err)
				continue
			}
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

func queryQueryRange(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	profileTypesStore *sync.Map[string, struct{}],
	seriesStore *sync.Map[string, []*queryv1alpha1.MetricsSample],
) {
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
				log.Println(err)
				continue
			}
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
	seriesStore *sync.Map[string, []*queryv1alpha1.MetricsSample],
) {
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
				log.Println(err)
				continue
			}

			log.Printf("querying %s took %v for %s\n", reportType.String(), time.Since(start), series)
		}
	}
}
