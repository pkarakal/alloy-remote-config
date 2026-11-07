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

This is the default order. Optional [matcher cohorts and binding priority](#matcher-cohorts-and-binding-priority-new-opt-in)
rank all applicable bindings by priority instead; with neither set, the order is unchanged.

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

Deletion is blocked while `activeBindings > 0`. The count is derived from the bindings on every reconcile, so it self-heals after drift.

**Empty content is rejected.** `spec.content` must contain at least one non-whitespace character (enforced by
`minLength` and a CEL rule). Objects stored before this rule keep working through validation ratcheting, which needs
Kubernetes 1.30 or newer. Such a blank object is treated as not ready: its `contentHash` is cleared and the API answers
`Unavailable` instead of serving it.

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
| `conditions`     | `MatchersValid`, only when `spec.matchers` is set (see below).                |

Deletion is blocked while `activeBindings > 0`. The count is derived from the bindings on every reconcile, so it self-heals after drift.

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
| `conditions`        | `RefsValid` — whether all referenced resources exist; `Overlap` — see below                                                                                                                                                                                                                             |
| `effectivePriority` | `spec.priority`, or 200 (tenant) / 100 (group) when unset                                                                                                                                                                                                                        |
| `registeredTenants` | List of tenants currently registered to this binding. Each entry contains the tenant `id`, the collector instance `collectorId`, and a `lastSeenAt` timestamp. Entries are upserted on `RegisterCollector` and evicted after the collector TTL window elapses without a refresh. |

**Dangling references.** If the binding chosen for a tenant or group points at a `PipelineConfig` that does not exist,
`GetConfig` answers `Unavailable` (not `Internal`) and does not fall through to the group or default config.

**Deterministic tie-break.** When several bindings share a `tenantRef` (or a `collectorGroupRef`) in one namespace, the
oldest by `metadata.creationTimestamp` wins, then the lowest name. The same rule applies to multiple default-labeled
`PipelineConfig` resources, and the config resolver and collector registry always pick the same binding.

**Overlap condition.** A binding that loses to another binding with the same target and the same effective priority gets `Overlap=True` with reason
`DuplicateTarget` and a message naming the winner. Other bindings get `Overlap=False`. The phase is not affected, and
the condition clears when the winner is deleted or retargeted.

### Matcher cohorts and binding priority (new, opt-in)

Both features are optional. Without `spec.matchers` and `spec.priority` anywhere, resolution is exactly the
tenant, then group, then default order described above.

#### Matchers

A `CollectorGroup` can select members by collector attributes, in addition to the legacy rule (the collector sets
`collector_group` to the group's name). A collector is a member of group `G` if either holds.

```yaml
apiVersion: fleet.pkarakal.com/v1alpha1
kind: CollectorGroup
metadata:
  name: canary-org-1234
spec:
  matchers:
    - org_id="org-1234"
    - cluster_id=~"c-1|c-2"
```

Each item is `key OP "value"` (at most 16 items, 200 characters each). Keys may contain dots, so Alloy's built-in
`collector.os` and `collector.version` attributes work. Alloy always sends both of them, on top of the attributes you set.

| Operator | Meaning                  | Missing attribute (treated as `""`)       |
|----------|--------------------------|-------------------------------------------|
| `=`      | equals                   | matches only `key=""`                     |
| `!=`     | does not equal           | matches (`env!="prod"` matches no `env`)  |
| `=~`     | regex matches            | matches only if the regex matches `""`    |
| `!~`     | regex does not match     | matches unless the regex matches `""`     |

- Regexes are fully anchored (`^(?:re)$`), so `c-1|c-2` does not match `c-22`.
- The value is a Go-quoted string, so write `\\.` for a literal dot in a regex.
- All matchers of a group must match (AND).
- The `CollectorGroup` reconciler sets the `MatchersValid` condition when matchers are present. If a matcher does not
  compile (for example a bad regex), the condition is `False` with the error and the group contributes only legacy
  membership.

#### Priority

`CollectorGroupBinding.spec.priority` (optional, 0 to 1000) ranks bindings that apply to the same collector. The
highest effective priority wins, then the oldest `creationTimestamp`, then the lowest name. The top candidate
decides: if its `PipelineConfig` is missing or not ready the answer is `Unavailable`, with no fallthrough.

| Candidate                       | Default effective priority |
|---------------------------------|----------------------------|
| Tenant binding (`tenantRef`)    | 200                        |
| Group binding (`collectorGroupRef`) | 100                    |
| Default `PipelineConfig`        | 0                          |

The defaults reproduce the previous order. `status.effectivePriority` shows the value in use (printer column
`Priority`). The default config is only used when no binding applies.

`Overlap=True` now means another binding with the same target (same `tenantRef` or same `collectorGroupRef`) has the
same effective priority and wins the tie-break. Different priorities are an explicit ranking, not an overlap. Overlaps
between different groups that match the same collector through matchers are not detected; rank them with priority.

#### Canary example

Send org-1234's collectors on clusters `c-1` and `c-2` a canary config, ahead of any tenant binding:

```yaml
apiVersion: fleet.pkarakal.com/v1alpha1
kind: CollectorGroupBinding
metadata:
  name: canary-org-1234
spec:
  collectorGroupRef: canary-org-1234
  pipelineConfigRef: my-pipeline-canary
  priority: 300
```

Collectors outside the cohort keep resolving as before. A copy of this pair is in
`config/samples/fleet_v1alpha1_canary.yaml`.

#### Alloy Deployment example

Clustering enabled, `loki.source.kubernetes` for pod logs, and a `remotecfg` block whose attributes feed the matchers.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: alloy
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: alloy
rules:
  - apiGroups: [""]
    resources: ["pods", "pods/log"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: alloy
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: alloy
subjects:
  - kind: ServiceAccount
    name: alloy
    namespace: default
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: alloy-config
data:
  config.alloy: |
    discovery.kubernetes "pods" {
      role = "pod"
    }

    loki.source.kubernetes "pods" {
      targets    = discovery.kubernetes.pods.targets
      forward_to = []
      clustering {
        enabled = true
      }
    }

    remotecfg {
      url            = "http://alloy-remote-config.default.svc:8080"
      id             = sys.env("HOSTNAME")
      poll_frequency = "1m"
      attributes = {
        "cluster_id" = "c-1",
        "env"        = "prod",
        "tenant"     = "acme",
        "org_id"     = "org-1234",
      }
    }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: alloy
spec:
  replicas: 2
  selector:
    matchLabels:
      app: alloy
  template:
    metadata:
      labels:
        app: alloy
    spec:
      serviceAccountName: alloy
      containers:
        - name: alloy
          image: grafana/alloy:latest
          args:
            - run
            - /etc/alloy/config.alloy
            - --cluster.enabled=true
            - --cluster.join-addresses=alloy-cluster.default.svc
          volumeMounts:
            - name: config
              mountPath: /etc/alloy
      volumes:
        - name: config
          configMap:
            name: alloy-config
```

This example assumes a headless `alloy-cluster` Service selecting `app: alloy` for peer discovery, and that the
`remotecfg` URL matches your Service. Adjust both to your install.

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
`CollectorGroupBinding`. Registration is best-effort: `RegisterCollector` and `UnregisterCollector` always return OK
(Alloy treats a registration error as fatal for remotecfg), failures are logged and counted in
`alloy_remote_config_registry_errors_total` (new), and collectors without a `tenant` attribute are skipped silently.
This provides a live view of which tenants are active and what config they are receiving:

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

Collectors poll `GetConfig` every minute or so, so refreshing `lastSeenAt` on every poll would turn each poll into an
API write. Poll refreshes are therefore throttled: an entry is rewritten only when its `lastSeenAt` is older than a
quarter of the collector TTL (one minute when the TTL is `0`). `RegisterCollector` always writes. As a result:

- `lastSeenAt` has a granularity of about TTL/4, not per poll.
- An entry is one per tenant, not per collector. It shows the collector that wrote last, so when several collectors
  share a tenant (for example a clustered Alloy deployment) `collectorId` is whichever of them refreshed the entry most
  recently.
- Each binding holds at most 1000 `registeredTenants` entries. A new tenant beyond that is not recorded (logged once per
  binding per process, counted by `alloy_remote_config_registry_truncated_total`); existing entries keep refreshing.

### Collector apply status (new)

Alloy v1.12 and newer report whether it applied the config it fetched (`remote_config_status` on `GetConfig`).
The operator records this on the tenant's `status.registeredTenants` entry; older Alloy versions report nothing and
show `Unknown`. The effective config is not stored.

| Field (per entry)  | Meaning                                                                   |
|--------------------|---------------------------------------------------------------------------|
| `configStatus`     | `Unknown`, `Applying`, `Applied` or `Failed`                              |
| `lastError`        | Error from the last `Failed` report, truncated to 1024 bytes              |
| `statusChangedAt`  | When `configStatus` or `lastError` last changed                           |

The binding derives `status.appliedCount`, `status.failedCount` and `status.lastApplyError` (from the most recently
changed `Failed` entry), shown as the `Applied` and `Failed` columns:

```
$ kubectl get collectorgroupbindings
NAME     PHASE    CONFIG   HASH       APPLIED   FAILED   SYNCED   AGE
canary   Active   canary   3fa9c1...  4         1        12s      2d
```

- Counts are per tenant, not per Alloy pod. An entry shows whichever collector wrote last, so one failing pod among
  several sharing a tenant can be masked by a later healthy report.
- A status change is written at once, bypassing the heartbeat throttle; an unchanged status keeps the TTL/4 throttle.
- Alloy also reports `Failed` when `GetConfig` itself fails (for example a server-side `Unavailable` while a config
  is not ready). It reports `Applied` again after the next successful poll.
- `RegisterCollector` carries no status and leaves the stored status untouched.
- Metrics: `alloy_remote_config_config_status_reports_total{status}` is a per-replica counter, aggregate it with `sum`.
  `alloy_remote_config_registered_tenants_by_status{status}` is derived from the CRDs, so every replica reports the
  same value: aggregate it with `max`, not `sum`.

### Trusted attributes (new, opt-in)

By default a collector's identity is self-asserted: `GetConfig` and `RegisterCollector` read `tenant`,
`collector_group` and every other matcher attribute from the request body. This server does no authentication itself
(an ingress or proxy does), so any collector that passes your ingress can request any tenant's config and claim any
matcher attribute such as `cluster_id`.

`--trusted-attribute <key>=<Header-Name>` (repeatable) takes the attribute `<key>` from a request header that your proxy
sets after authenticating the caller:

```
--trusted-attribute=tenant=X-Scope-OrgID
--trusted-attribute=org_id=X-Org-ID
```

Without the flag nothing changes. With it, for each trusted key:

| RPC                   | Header missing or ambiguous | Attribute differs from header | Otherwise                                  |
|-----------------------|-----------------------------|-------------------------------|--------------------------------------------|
| `GetConfig`           | `Unauthenticated`           | `PermissionDenied`            | The header value replaces the attribute    |
| `RegisterCollector`   | Skipped, returns OK         | Skipped, returns OK           | Registered with the header value           |
| `UnregisterCollector` | Skipped, returns OK         | n/a (no attributes)           | Removes only an entry of the header tenant |

"Ambiguous" means the header was sent more than once with different values; an empty value counts as missing. An
attribute the collector did not send (or sent empty) is simply filled in from the header. `RegisterCollector` and
`UnregisterCollector` never fail because Alloy treats a Register error as fatal for `remotecfg`. `UnregisterCollector`
is only scoped when `tenant` is a trusted key. Rejections are counted in
`alloy_remote_config_trusted_attribute_rejections_total`.

**Requirement:** the proxy must overwrite or strip a client-supplied header of the same name. Otherwise a collector can
send the header itself and the server cannot tell it from the proxy's. Pass the flag through the chart's existing
`manager.args` value:

```yaml
manager:
  args:
    - --leader-elect
    - --trusted-attribute=tenant=X-Scope-OrgID
```

Matcher-defined cohorts should pin a trusted key (for example `org_id="..."`) so they cannot target other orgs'
collectors. This is not enforced yet; a global (unscoped) binding mode is deferred.

#### nginx sidecar or ingress with `auth_request`

```nginx
location / {
  auth_request /auth;
  # The auth service answers with X-Scope-OrgID for the authenticated caller.
  auth_request_set $org $upstream_http_x_scope_orgid;
  # Replaces any client-sent X-Scope-OrgID; an empty $org drops the header.
  proxy_set_header X-Scope-OrgID $org;
  proxy_pass http://alloy-remote-config-server:12345;
}
```

ingress-nginx is retired, but the equivalent annotation `nginx.ingress.kubernetes.io/auth-response-headers: X-Scope-OrgID`
exists there and forwards the auth service's response header to the upstream (replacing the client's).

#### Envoy gateway (not tested)

With the chart's Envoy gateway, use the `jwt_authn` filter's `claim_to_headers` to copy a verified claim into the
header, and put a `header_mutation` filter before `jwt_authn` that removes the client's copy so it cannot be spoofed
when the claim is absent:

```yaml
http_filters:
  - name: envoy.filters.http.header_mutation
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.http.header_mutation.v3.HeaderMutation
      mutations:
        request_mutations:
          - remove: x-scope-orgid
  - name: envoy.filters.http.jwt_authn
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.http.jwt_authn.v3.JwtAuthentication
      providers:
        idp:
          # issuer, remote_jwks, ...
          claim_to_headers:
            - header_name: x-scope-orgid
              claim_name: org
```

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
| `alloy_remote_config_registry_errors_total` (new)  | Counter   | `op`                | Swallowed registry errors; `op` is `register`, `unregister` or `refresh` |
| `alloy_remote_config_registry_truncated_total`     | Counter   | —                   | Registrations dropped because a binding hit the 1000-entry `registeredTenants` cap |
| `alloy_remote_config_config_status_reports_total` (new) | Counter | `status`          | GetConfig calls by reported apply status (`applied`, `applying`, `failed`, `unknown`); per replica, use `sum` |
| `alloy_remote_config_trusted_attribute_rejections_total` (new) | Counter | `procedure`, `reason` | Requests rejected or skipped by `--trusted-attribute`; `reason` is `missing`, `ambiguous` or `mismatch` |

### Resolution metrics

| Metric                                                    | Type      | Labels            | Description                                   |
|-----------------------------------------------------------|-----------|-------------------|-----------------------------------------------|
| `alloy_remote_config_config_resolution_total`             | Counter   | `path`, `outcome` | Resolution requests; `path` is the kind of the winning candidate (`tenant`, `collectorgroup`, `default`) |
| `alloy_remote_config_config_resolution_duration_seconds`  | Histogram | `path`            | Informer cache lookup latency per step        |

`path` is one of `tenant`, `collectorgroup`, or `default`.

### Resource metrics

| Metric                                           | Type  | Labels                   | Description                                        |
|--------------------------------------------------|-------|--------------------------|----------------------------------------------------|
| `alloy_remote_config_pipeline_configs`           | Gauge | `validity`               | PipelineConfig count by validity                   |
| `alloy_remote_config_collector_group_bindings`   | Gauge | `phase`                  | CollectorGroupBinding count by phase               |
| `alloy_remote_config_collector_groups`           | Gauge | —                        | Total CollectorGroup count                         |
| `alloy_remote_config_deletion_blocked_resources` | Gauge | `kind`                   | Resources blocked from deletion by active bindings |
| `alloy_remote_config_registered_tenants_by_status` (new) | Gauge | `status`          | Registered tenants by apply status; identical on every replica, use `max` not `sum` |
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
