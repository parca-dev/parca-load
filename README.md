# parca-load

This is a tool that continuously queries Parca instances for their data.

It is based on the Parca gRPC APIs defined on https://buf.build/parca-dev/parca and uses the generated connect-go code via `go.buf.build/bufbuild/connect-go/parca-dev/parca`. 

### How it works

It runs a goroutine per API type.

It starts a `Labels` goroutine that starts querying all labels on a Parca instance and then writes these into a shared map.
The map is then read by the `Values` goroutine that selects a random label and queries all values for it.

This process it repeated every 5 seconds. 
The entries of the shared map eventually expire to not query too old data.

Similarly, it starts querying `ProfileTypes` every 10 seconds to know what profile types are available.
The result is written to a shared map. 
Every 15 seconds there are `QueryRange` requests (querying up to 15min of data) for a random profile type.

Once the profile series are discovered above there are `Query` requests querying single profiles every 10 seconds.
For these queries it picks a random timestamp of the available time range and queries a random report type (flame graph, top table, pprof download).

Every 15 seconds there are `Query` requests that actually request merged profiles for either 15min, 5min or 1min, if enough data is available for each in a series.

Metrics are collected and available on http://localhost:7171/metrics
