# parca-load

This is a tool that continuously queries Parca instances for their data.

It is based on the Parca gRPC APIs defined on https://buf.build/parca-dev/parca and uses the generated connect-go code via `go.buf.build/bufbuild/connect-go/parca-dev/parca`.

### Installation

```
go install github.com/parca-dev/parca-load@latest
```

or run the container images

```
docker run -d ghcr.io/parca-dev/parca-load
```

### How it works

The tool queries a Parca instance using profile types. If the `-types` flag is not specified, the tool will auto-discover available profile types from the backend using the ProfileTypes API.

It starts a `Labels` query that gets all labels for each profile type and writes these into a shared map.
The map is then read by the `Values` query that selects a random label and queries all values for it.

This process is repeated at the configured query interval (default: 5 seconds).

For each profile type and time range, `QueryRange` requests are made to get profile series data.

`Query` requests with merge mode are made to request merged profiles for each profile type and time range combination.

Metrics are collected and available on http://localhost:7171/metrics

### Configuration

#### Profile Types (Optional)

The `-types` flag specifies which profile types to query. If not specified, profile types are auto-discovered from the backend. Multiple profile types are separated by semicolons.

Profile type format: `name:sample_type:sample_unit:period_type:period_unit[:delta]`

```
# Auto-discover profile types from the backend
./parca-load -url=http://localhost:7070

# Single profile type
./parca-load -url=http://localhost:7070 -types='parca_agent:samples:count:cpu:nanoseconds:delta'

# Multiple profile types
./parca-load -url=http://localhost:7070 -types='parca_agent:samples:count:cpu:nanoseconds:delta;memory:alloc_objects:count:space:bytes'
```

#### Query Range

The `-query-range` flag specifies time durations for queries, separated by semicolons. Default: `15m;12h;168h`.

```
./parca-load -url=http://localhost:7070 -query-range="1h;24h"
```

#### Label Selectors

The `-labels` flag allows filtering queries by label selectors. Use Prometheus-style label selectors with braces. Multiple selectors are separated by semicolons. Default: `all` (no filtering).

```
# Single selector
./parca-load -url=http://localhost:7070 -labels='{job="api"}'

# Multiple labels in one selector
./parca-load -url=http://localhost:7070 -labels='{job="api",env="prod"}'

# Multiple selectors (each is queried separately)
./parca-load -url=http://localhost:7070 -labels='{job="api"};{namespace="production"}'
```

When label selectors are specified, they are appended to the profile type in QueryRange and Query (merge) requests. For example, with `-labels='{job="api"}'`, a query becomes `memory:alloc_objects:count:space:bytes{job="api"}`
