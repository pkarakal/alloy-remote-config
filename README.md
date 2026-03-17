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
│  └─────────────────────────────────────────┘            │
└─────────────────────────────────────────────────────────┘
```

The `CollectorGroupBinding` controller registers three field indexes on the informer cache at startup. These allow the
remote-config API server (not yet implemented) to resolve which `PipelineConfig` to serve for a given collector via an
O(1) `MatchingFields` lookup — no separate in-memory store needed.

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

| Field          | Description                                                 |
|----------------|-------------------------------------------------------------|
| `phase`        | `Pending`, `Active`, or `Degraded`                          |
| `configHash`   | Hash of the `PipelineConfig` content currently being served |
| `lastSyncedAt` | Time of the last successful reconcile                       |
| `conditions`   | `RefsValid` — whether all referenced resources exist        |

---

## Project layout

```
.
├── api/v1alpha1/               # CRD type definitions and generated DeepCopy methods
├── cmd/main.go                 # Operator entry point (controller-manager)
├── main.go                     # Remote-config API server entry point (work in progress)
├── internal/
│   └── controller/             # Reconciliation logic for each CRD
├── config/
│   ├── crd/bases/              # Generated CRD manifests (do not edit)
│   ├── rbac/                   # Generated RBAC manifests (do not edit)
│   ├── manager/                # Operator Deployment manifests
│   └── samples/                # Example CRs
└── test/
    └── e2e/                    # End-to-end tests
```

---

## Getting started

See [docs/getting-started.md](docs/getting-started.md).
