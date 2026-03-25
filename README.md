# alloy-remote-config

A Kubernetes operator for fleet-wide management of [Grafana Alloy](https://grafana.com/docs/alloy/) telemetry pipeline
configurations.

Alloy supports a [`remotecfg`](https://grafana.com/docs/alloy/latest/reference/config-blocks/remotecfg/) block that
polls an API endpoint for its pipeline configuration at runtime. This project implements that API alongside a Kubernetes
operator that lets you manage pipeline configs and collector group assignments as first-class Kubernetes resources: no
config file rollouts, no restarts.

---

## Overview

The operator manages three custom resources under `fleet.pkarakal.com/v1alpha1`:

- **`PipelineConfig`**: stores raw Alloy pipeline config content and computes its SHA-256 hash, which collectors use to
  detect changes without fetching the full payload.
- **`CollectorGroup`**: a named logical group of collectors. Collectors self-identify with a group ID via their
  `remotecfg` attributes.
- **`CollectorGroupBinding`**: binds a `PipelineConfig` to either a `CollectorGroup` (all collectors in the group) or a
  specific tenant by ID (`tenantRef`). Tenant-specific bindings take priority over group-wide ones.

### Config resolution

When a collector polls for its config, resolution is first-match in priority order:

1. Exact `tenantRef` match: a binding targeting this specific collector
2. Exact `collectorGroupRef` match: a binding targeting the collector's group
3. Default fallback: a `PipelineConfig` labelled `fleet.pkarakal.com/default-pipeline-config: "true"` in the watched namespace

---

## Architecture

```
┌────────────────────────────────────────────────────────┐
│                    Kubernetes API                      │
│  ┌──────────────┐  ┌───────────────┐  ┌──────────────┐ │
│  │PipelineConfig│  │CollectorGroup │  │  CGB         │ │
│  └──────┬───────┘  └───────┬───────┘  └──────┬───────┘ │
└─────────┼──────────────────┼─────────────────┼─────────┘
          │ watch            │ watch           │ watch
          ▼                  ▼                 ▼
┌─────────────────────────────────────────────────────────┐
│                      Operator                           │
│              (cmd/main.go)                              │
│                                                         │
│  ┌────────────────┐  ┌──────────────┐  ┌────────────┐   │
│  │PipelineConfig  │  │CollectorGroup│  │    CGB     │   │
│  │Controller      │  │Controller    │  │Controller  │   │
│  └────────────────┘  └──────────────┘  └─────┬──────┘   │
│                                              │          │
│              Informer Cache                  │          │
│  ┌─────────────────────────────────────────┐ │          │
│  │ Field indexes:                          │◄┘          │
│  │  .spec.tenantRef                        │            │
│  │  .spec.collectorGroupRef                │            │
│  │  .spec.pipelineConfigRef                │            │
│  └──────────────────┬──────────────────────┘            │
│                     │ O(1) MatchingFields               │
│                     ▼                                   │
│  ┌──────────────────────────────────────────────────┐   │
│  │         Connect-RPC API Server (:12345)          │   │
│  │  CollectorService                                │   │
│  │   GetConfig / RegisterCollector /                │   │
│  │   UnregisterCollector                            │   │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
          ▲
          │ remotecfg polling
          │
┌─────────┴───────────┐
│   Alloy Collectors  │
└─────────────────────┘
```

The `CollectorGroupBinding` controller registers three field indexes on the informer cache at startup. The Connect-RPC
API server uses these indexes for O(1) `MatchingFields` lookups when resolving which `PipelineConfig` to serve — no
separate in-memory store needed.

---

## Custom resources

### PipelineConfig

Stores a raw Alloy pipeline configuration. The operator computes its SHA-256 content hash and tracks how many bindings
reference it.

```yaml
apiVersion: fleet.pkarakal.com/v1alpha1
kind: PipelineConfig
metadata:
  name: my-pipeline
spec:
  content: |
    otelcol.receiver.otlp "default" {
      http {}
      grpc {}
      output {
        traces = [otelcol.exporter.otlphttp.default.input]
      }
    }
    otelcol.exporter.otlphttp "default" {
      client { endpoint = sys.env("OTLP_ENDPOINT") }
    }
  description: "Receives OTLP traces and forwards to an OTLP HTTP exporter"
```

**Status fields:**

| Field            | Description                                                                                    |
|------------------|------------------------------------------------------------------------------------------------|
| `contentHash`    | SHA-256 of `spec.content`. Collectors compare this against their local hash to detect changes. |
| `activeBindings` | Number of `CollectorGroupBinding` resources currently referencing this config.                 |

Deletion is blocked while `activeBindings > 0`.

---

### CollectorGroup

A named group that collectors self-assign to via their `remotecfg` `collector_id` attribute.

```yaml
apiVersion: fleet.pkarakal.com/v1alpha1
kind: CollectorGroup
metadata:
  name: production-collectors
spec:
  description: "All production Alloy instances in the core pipeline"
```

**Status fields:**

| Field            | Description                                                                   |
|------------------|-------------------------------------------------------------------------------|
| `activeBindings` | Number of `CollectorGroupBinding` resources currently referencing this group. |

Deletion is blocked while `activeBindings > 0`.

---

### CollectorGroupBinding

Assigns a `PipelineConfig` to a `CollectorGroup` or to a specific collector by tenant ID. Exactly one of
`collectorGroupRef` or `tenantRef` must be set.

```yaml
# Group-wide binding
apiVersion: fleet.pkarakal.com/v1alpha1
kind: CollectorGroupBinding
metadata:
  name: production-binding
spec:
  collectorGroupRef: production-collectors
  pipelineConfigRef: my-pipeline
```

```yaml
# Tenant-specific binding (higher priority than group-wide)
apiVersion: fleet.pkarakal.com/v1alpha1
kind: CollectorGroupBinding
metadata:
  name: tenant-override
spec:
  tenantRef: "acme-corp-collector"
  pipelineConfigRef: my-pipeline-v2
```

**Status fields:**

| Field               | Description                                                                                                                                                                                                                                                                      |
|---------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `phase`             | `Pending`, `Active`, or `Degraded`                                                                                                                                                                                                                                               |
| `configHash`        | Hash of the `PipelineConfig` content currently being served                                                                                                                                                                                                                      |
| `lastSyncedAt`      | Time of the last successful reconcile                                                                                                                                                                                                                                            |
| `conditions`        | `RefsValid` — whether all referenced resources exist                                                                                                                                                                                                                             |
| `registeredTenants` | List of tenants currently registered to this binding. Each entry contains the tenant `id`, the collector instance `collectorId`, and a `lastSeenAt` timestamp. Entries are upserted on `RegisterCollector` and evicted after the collector TTL window elapses without a refresh. |

---

## Remote-config API

The operator embeds a [Connect-RPC](https://connectrpc.com/) server that implements the
[Grafana Alloy remote config API](https://github.com/grafana/alloy-remote-config). Alloy collectors configure a
`remotecfg` block pointing at this server; the server then resolves and returns the correct `PipelineConfig` content.

The server listens on `:12345` by default (configurable via `--connect-bind-address`).

### RPC methods

| Method                | Description                                                                                       |
|-----------------------|---------------------------------------------------------------------------------------------------|
| `GetConfig`           | Returns the pipeline config for a collector, with hash-based caching                              |
| `RegisterCollector`   | Upserts the collector's tenant into the matching `CollectorGroupBinding.status.registeredTenants` |
| `UnregisterCollector` | Removes the collector's tenant entry from its binding's `registeredTenants`                       |

`GetConfig` accepts a `hash` field. If the collector's cached hash matches the current config hash, the server returns
`notModified: true` and omits the config body as that Alloy server already has the configuration body.

### Configuring Alloy

Add a `remotecfg` block to your Alloy configuration pointing at the operator's Connect-RPC address:

```alloy
remotecfg {
  url = "http://alloy-remote-config-server.<namespace>.svc.cluster.local:12345"

  // Identifies this collector for tenant-specific bindings
  id = constants.hostname

  // Assign the collector to a group for group-wide bindings
  attributes {
    "tenant" = "tenant"
    "collector_group" = "production"
  }

  poll_interval = "1m"
}
```

The `tenant` attribute maps to `tenantRef` lookups; the `collector_group` attribute maps to `collectorGroupRef` lookups.

### Collector registration

When a collector calls `RegisterCollector`, the operator upserts it into `status.registeredTenants` of the matching
`CollectorGroupBinding`. This provides a live view of which tenants are active and what config they are receiving:

```bash
# See all registered tenants for a binding
kubectl get collectorgroupbinding production-binding \
  -o jsonpath='{.status.registeredTenants}' | jq .

# [
#   { "id": "acme", "collectorId": "alloy-prod-1", "lastSeenAt": "2026-03-25T10:00:00Z" },
#   { "id": "contoso", "collectorId": "alloy-prod-2", "lastSeenAt": "2026-03-25T10:01:00Z" }
# ]
```

Entries are evicted by the `CollectorGroupBinding` reconciler after the collector TTL window elapses without a refresh
(default: `5m`, configurable via `--collector-ttl`). Setting `--collector-ttl=0` disables eviction entirely.

### Default PipelineConfig

The default fallback config is discovered by label, not by name. Apply the well-known label to any `PipelineConfig` to
make it the default:

```yaml
metadata:
  labels:
    fleet.pkarakal.com/default-pipeline-config: "true"
```

The Helm chart creates a labelled default `PipelineConfig`, `CollectorGroup`, and `CollectorGroupBinding` out of the box
(see `values.defaults`). If multiple resources carry the label, the first match is used and a warning is logged.

---

## Observability

The operator exposes Prometheus metrics under the `alloy_remote_config` namespace.

### RPC metrics

| Metric                                             | Type      | Labels              | Description                                    |
|----------------------------------------------------|-----------|---------------------|------------------------------------------------|
| `alloy_remote_config_rpc_requests_total`           | Counter   | `procedure`, `code` | Total RPC requests by procedure and status     |
| `alloy_remote_config_rpc_request_duration_seconds` | Histogram | `procedure`         | RPC request latency                            |
| `alloy_remote_config_config_served_total`          | Counter   | —                   | GetConfig calls returning a fresh config body  |
| `alloy_remote_config_config_not_modified_total`    | Counter   | —                   | GetConfig calls returning notModified          |

### Resolution metrics

| Metric                                                    | Type      | Labels            | Description                                   |
|-----------------------------------------------------------|-----------|-------------------|-----------------------------------------------|
| `alloy_remote_config_config_resolution_total`             | Counter   | `path`, `outcome` | Resolution attempts per lookup step           |
| `alloy_remote_config_config_resolution_duration_seconds`  | Histogram | `path`            | Informer cache lookup latency per step        |

`path` is one of `tenant`, `collectorgroup`, or `default`.

### Resource metrics

| Metric                                           | Type  | Labels                   | Description                                        |
|--------------------------------------------------|-------|--------------------------|----------------------------------------------------|
| `alloy_remote_config_pipeline_configs`           | Gauge | `validity`               | PipelineConfig count by validity                   |
| `alloy_remote_config_collector_group_bindings`   | Gauge | `phase`                  | CollectorGroupBinding count by phase               |
| `alloy_remote_config_collector_groups`           | Gauge | —                        | Total CollectorGroup count                         |
| `alloy_remote_config_deletion_blocked_resources` | Gauge | `kind`                   | Resources blocked from deletion by active bindings |
| `alloy_remote_config_build_info`                 | Gauge | `version`, `goversion`   | Build metadata (value always 1)                    |

A `ServiceMonitor` manifest is included at `config/prometheus/` for Prometheus Operator-based scraping.

---

## Project layout

```
.
├── api/v1alpha1/               # CRD type definitions and generated DeepCopy methods
├── cmd/main.go                 # Entry point: controller-manager + Connect-RPC server
├── internal/
│   ├── controller/             # Reconciliation logic for each CRD
│   ├── service/                # Connect-RPC CollectorService implementation
│   ├── server/                 # HTTP server wrapping Connect-RPC (manager.Runnable)
│   ├── adapter/
│   │   ├── kubernetes/         # Informer-cache-backed ConfigResolver and KubernetesCollectorRegistry
│   │   └── noop/               # No-op CollectorRegistry (kept for testing)
│   ├── port/                   # ConfigResolver and CollectorRegistry interfaces
│   ├── metrics/                # Prometheus metric definitions and resource collector
│   └── interceptor/            # Connect-RPC metrics interceptor
├── config/
│   ├── crd/bases/              # Generated CRD manifests (do not edit)
│   ├── rbac/                   # Generated RBAC manifests (do not edit)
│   ├── manager/                # Operator Deployment manifests
│   ├── prometheus/             # ServiceMonitor for Prometheus Operator
│   └── samples/                # Example CRs
└── test/
    └── e2e/                    # End-to-end tests
```

---

## Getting started

See [docs/getting-started.md](docs/getting-started.md).
