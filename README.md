# parca-load

A load testing tool that continuously queries Parca instances.

## Installation

```
go install github.com/parca-dev/parca-load@latest
```

or via container:

```
docker run ghcr.io/parca-dev/parca-load
```

## How it works

The tool runs five query types in parallel at a configurable interval (default: 5s):

- **ProfileTypes** - discovers available profile types
- **Labels** - queries label names for each profile type
- **Values** - queries label values (if `-values-for-labels` is set)
- **QueryRange** - fetches profile series data
- **Query (merge)** - fetches merged flamegraph data

Each query type runs against all configured profile types, time ranges, and label selectors.

Metrics are exposed at `http://<addr>/metrics` (default: `127.0.0.1:7171`).

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-url` | `http://localhost:7070` | Parca instance URL |
| `-addr` | `127.0.0.1:7171` | HTTP server address for metrics |
| `-query-interval` | `5s` | Interval between query rounds |
| `-query-range` | `15m;12h;168h` | Time ranges for queries (semicolon-separated) |
| `-types` | (auto-discover) | Profile types to query (semicolon-separated) |
| `-labels` | `all` | Label selectors for filtering (semicolon-separated) |
| `-values-for-labels` | (none) | Label names to query values for (semicolon-separated) |
| `-token` | | Bearer token for authentication |
| `-headers` | | Custom headers (`key=value,key2=value2`) |
| `-client-timeout` | `10s` | HTTP client timeout |

## Examples

```bash
# Basic usage with auto-discovered profile types
./parca-load -url=http://localhost:7070

# Specific profile types
./parca-load -url=http://localhost:7070 \
  -types='parca_agent:samples:count:cpu:nanoseconds:delta'

# Custom time ranges and label filtering
./parca-load -url=http://localhost:7070 \
  -query-range='1h;24h' \
  -labels='{job="api",env="dev"}'

# Query values for specific labels
./parca-load -url=http://localhost:7070 \
  -values-for-labels='job;namespace'
```

Profile type format: `name:sample_type:sample_unit:period_type:period_unit[:delta]`
