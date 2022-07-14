package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/bufbuild/connect-go"
	queryv1alpha1 "go.buf.build/bufbuild/connect-go/parca-dev/parca/parca/query/v1alpha1"
	"go.buf.build/bufbuild/connect-go/parca-dev/parca/parca/query/v1alpha1/queryv1alpha1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type syncMap[k comparable, v any] struct {
	mtx      *sync.RWMutex
	elements map[k]v
}

func (sm *syncMap[k, v]) Store(key k, value v) {
	sm.mtx.Lock()
	defer sm.mtx.Unlock()
	sm.elements[key] = value
}

func (sm syncMap[k, v]) Range(f func(key k, value v) bool) {
	sm.mtx.RLock()
	defer sm.mtx.RUnlock()

	for k, v := range sm.elements {
		if !f(k, v) {
			break
		}
	}
}

func newSyncMap[k comparable, v any]() *syncMap[k, v] {
	return &syncMap[k, v]{
		mtx:      &sync.RWMutex{},
		elements: make(map[k]v),
	}
}

func main() {
	url := flag.String("url", "http://localhost:7070", "The URL for the Parca instance to query")
	flag.Parse()

	client := queryv1alpha1connect.NewQueryServiceClient(
		&http.Client{Timeout: 10 * time.Second},
		*url,
		connect.WithGRPCWeb(),
	)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	profileTypes := newSyncMap[string, struct{}]()

	go func() {
		s := make(chan os.Signal)
		signal.Notify(s, os.Interrupt, os.Kill)
		<-s
		stop()
	}()

	go queryProfileTypes(ctx, client, profileTypes)
	go queryQueryRange(ctx, client, profileTypes)

	<-ctx.Done()
}

func queryProfileTypes(
	ctx context.Context,
	client queryv1alpha1connect.QueryServiceClient,
	profileTypes *syncMap[string, struct{}],
) {
	ticker := time.NewTicker(10 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Println("querying profile types...")
			resp, err := client.ProfileTypes(ctx, connect.NewRequest[queryv1alpha1.ProfileTypesRequest](nil))
			if err != nil {
				log.Println("failed to make profiles types request", err)
				continue
			}

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
	profileTypes *syncMap[string, struct{}],
) {
	ticker := time.NewTicker(15 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pt := ""
			profileTypes.Range(func(k string, _ struct{}) bool {
				pt = k
				return false
			})

			log.Println("querying query range...", pt)

			resp, err := client.QueryRange(ctx, connect.NewRequest[queryv1alpha1.QueryRangeRequest](&queryv1alpha1.QueryRangeRequest{
				Query: pt,
				Start: timestamppb.New(time.Now().Add(-1 * time.Hour)),
				End:   timestamppb.New(time.Now()),
			}))
			if err != nil {
				log.Println(err)
				continue
			}

			for _, series := range resp.Msg.Series {
				log.Println(pt, len(series.Samples))
			}
		}
	}
}
