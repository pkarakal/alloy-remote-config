# alloy-remote-config

A Kubernetes operator for fleet-wide management of [Grafana Alloy](https://grafana.com/docs/alloy/) telemetry pipeline
configurations.

Alloy supports a [`remotecfg`](https://grafana.com/docs/alloy/latest/reference/config-blocks/remotecfg/) block that
polls an API endpoint for its pipeline configuration at runtime. This project implements that API alongside a Kubernetes
operator that lets you manage pipeline configs and collector group assignments as first-class Kubernetes resources: no
config file rollouts, no restarts.

## Contents

- [Overview](#overview)
- [Installation](#installation)
- [Upgrading](#upgrading)
- [Configuration](#configuration)
- [Architecture](#architecture)
- [Custom resources](#custom-resources)
  - [PipelineConfig](#pipelineconfig) and [content validation](#content-validation)
  - [CollectorGroup](#collectorgroup)
  - [CollectorGroupBinding](#collectorgroupbinding)
  - [Matcher cohorts and binding priority](#matcher-cohorts-and-binding-priority-new-opt-in)
  - [Additive pipelines and binding expiry](#additive-pipelines-and-binding-expiry-new-opt-in)
  - [Conditions reference](#conditions-reference)
- [Validating configs in CI (recommended)](#validating-configs-in-ci-recommended)
- [Remote-config API](#remote-config-api)
- [Observability](#observability)
- [Project layout](#project-layout)
- [Getting started (development)](#getting-started-development)

---

## Overview

The operator manages three custom resources under `fleet.pkarakal.com/v1alpha1`:

- **`PipelineConfig`**: stores raw Alloy pipeline config content and computes its SHA-256 hash, which collectors use to
  detect changes without fetching the full payload.
- **`CollectorGroup`**: a named logical group of collectors. A collector is a member by naming the group in its
  `collector_group` attribute, or by matching the group's optional `spec.matchers`.
- **`CollectorGroupBinding`**: binds a `PipelineConfig` to either a `CollectorGroup` (all collectors in the group) or a
  specific tenant by ID (`tenantRef`).

### Config resolution

When a collector polls for its config, the server ranks every candidate that applies to it by effective priority. The
top candidate decides.

| Candidate                                                                                  | Default effective priority |
|--------------------------------------------------------------------------------------------|----------------------------|
| Tenant binding (`tenantRef` equals the collector's `tenant` attribute)                     | 200                        |
| Group binding (`collectorGroupRef` names a group the collector belongs to)                 | 100                        |
| Default fallback: the `PipelineConfig` labeled `fleet.pkarakal.com/default-pipeline-config: "true"` | 0 (used only when no binding applies) |

With no `spec.priority` set, this is the familiar order: tenant, then group, then default. Set `spec.priority`
(0 to 1000) on a binding to rank it explicitly, see [Matcher cohorts and binding priority](#matcher-cohorts-and-binding-priority-new-opt-in).
Ties go to the oldest `creationTimestamp`, then the lowest name. Optional `Additive` bindings are appended on top of the
winner, see [Additive pipelines](#additive-pipelines-and-binding-expiry-new-opt-in).

---

## Installation

### Helm (recommended)

The chart lives in [`charts/fleet-management`](charts/fleet-management). From a checkout of this repository:

```bash
helm install fleet ./charts/fleet-management --namespace fleet-system --create-namespace
```

The chart is also published as an OCI artifact at `oci://ghcr.io/pkarakal/charts/fleet-management`
(`helm install fleet oci://ghcr.io/pkarakal/charts/fleet-management --version <chart-version> ...`).

With the defaults this installs the manager, a `ClusterIP` Service for the Connect-RPC API
(`<release>-fleet-management-api:12345`), the PipelineConfig validating webhook with a self-signed certificate that Helm
generates (cert-manager is not required), an HTTPS metrics endpoint on `:8443`, and a default `CollectorGroup`,
`PipelineConfig` and `CollectorGroupBinding` (see `defaults.*` in the values). Content validation is on and set to
`enforce`. Replace `defaults.pipelineConfig.content` with your own pipeline.

The manager resolves and serves only the namespace it runs in (`--namespace`, which defaults to the pod namespace), so
create your `PipelineConfig`, `CollectorGroup` and `CollectorGroupBinding` objects in the release namespace
(`fleet-system` above). Objects in other namespaces are ignored.

Requirements: Kubernetes 1.30 or newer (CRD validation ratcheting, see [Upgrading](#upgrading)).

The chart can also put an Envoy gateway in front of the API (`gateway.enabled`, with `none`, `jwt`, `basic` or `mtls`
authentication), an Ingress (`ingress.enabled`), a ServiceMonitor (`prometheus.enable`) and PodDisruptionBudgets. See
[`values.yaml`](charts/fleet-management/values.yaml), every value is documented there.

### Kustomize

```bash
export IMG=<registry>/<image>:<tag>
make docker-build docker-push IMG=$IMG
make deploy IMG=$IMG
```

`make deploy` installs the CRDs and the manager into `alloy-remote-config-system`. It runs with the default
`--content-validation=enforce` but without the admission webhook (`config/webhook` is not part of `config/default`), so
invalid content is caught by the reconciler only. It does not create a Service for the Connect-RPC port; create one or
use `kubectl port-forward`. Create your resources in that namespace, for example
`kubectl apply -k config/samples/ -n alloy-remote-config-system`.

---

## Upgrading

1. **Apply the CRDs yourself.** Helm installs the files in `crds/` on first install only and never upgrades them. After
   `helm upgrade`, apply the CRDs of the chart version you installed:

   ```bash
   # From a checkout of the matching version
   kubectl apply --server-side --force-conflicts -f charts/fleet-management/crds/
   # Or straight from the chart
   helm show crds oci://ghcr.io/pkarakal/charts/fleet-management --version <chart-version> \
     | kubectl apply --server-side --force-conflicts -f -
   ```

   `--force-conflicts` is needed because Helm created the CRDs and still owns `.spec.versions`. Without the flag the server
   reports a field-manager conflict on `.spec.versions` and applies nothing. With old CRDs the new status fields (`lastValid*`,
   `appliedCount`, ...) are pruned and `enforce` serves like `warn`.
2. **Kubernetes 1.30 or newer.** The CRD now rejects blank `spec.content`. Objects stored before that rule keep working
   through validation ratcheting (enabled by default since Kubernetes 1.30). Such an object is treated as not ready and the API answers `Unavailable`.
3. **Content validation is on by default.** The chart defaults are `manager.contentValidation=enforce` and
   `webhook.enabled=true` with `webhook.failurePolicy=Fail`. PipelineConfig objects that predate `enforce` and still
   parse are seeded with their current content. One that does not parse keeps being served and gets
   `SeededUnvalidated=True`, so nothing that works today stops working. To change the behavior:

   ```bash
   # Report syntax errors as admission warnings, but admit and serve the content
   helm upgrade fleet ./charts/fleet-management -n fleet-system --reuse-values --set manager.contentValidation=warn
   # No webhook; the reconciler still validates and, in enforce, keeps serving the last valid content
   helm upgrade fleet ./charts/fleet-management -n fleet-system --reuse-values --set webhook.enabled=false
   # Turn the check off
   helm upgrade fleet ./charts/fleet-management -n fleet-system --reuse-values \
     --set manager.contentValidation=off --set webhook.enabled=false
   ```

   With `failurePolicy: Fail`, creating a PipelineConfig or editing its content is refused while the manager is down.
   Updates that leave `spec.content` alone (status, finalizers, metadata) are not sent to the webhook.
4. **The `fleet.pkarakal.com/skip-validation` label.** The chart puts it on its own default `PipelineConfig` (only when
   `webhook.enabled`), because Helm creates that object before the manager can answer the webhook. The webhook
   configuration excludes any object that carries the label (an `objectSelector`), so anyone who may create
   PipelineConfigs can use it to bypass admission. The reconciler still validates such objects: under `enforce`,
   content that never parsed is not served (`Unavailable`), and an invalid edit keeps serving the last valid content.

---

## Configuration

### Manager flags

| Flag                          | Default       | Description                                                                                         |
|-------------------------------|---------------|-----------------------------------------------------------------------------------------------------|
| `--content-validation`        | `enforce`     | `off`, `warn` (check and report, still serve) or `enforce` (reject at admission, keep serving the last valid content). Chart value `manager.contentValidation`. |
| `--enable-webhooks`           | `false`       | Serve the PipelineConfig validating webhook. Needs a certificate, see `--webhook-cert-path`. The chart sets it when `webhook.enabled`. |
| `--webhook-cert-path`         | none          | Directory that contains the webhook certificate. `--webhook-cert-name` (`tls.crt`) and `--webhook-cert-key` (`tls.key`) name the files. |
| `--connect-bind-address`      | `:12345`      | Address of the Connect-RPC server.                                                                  |
| `--namespace`                 | pod namespace, else `default` | Namespace whose resources are resolved and served. Collectors only see resources in this namespace. |
| `--collector-ttl`             | `5m`          | Evict a registered tenant entry that was not refreshed for this long. `0` disables eviction. Chart value `collectorTTL`. |
| `--trusted-attribute`         | none          | Repeatable `<key>=<Header-Name>`: take the attribute from a header set by a trusted proxy, see [Trusted attributes](#trusted-attributes-new-opt-in). |
| `--metrics-bind-address`      | `0` (off)     | Metrics address: `:8443` for HTTPS, `:8080` for HTTP. The chart sets `:<metrics.port>` (default `:8443`). `make run` uses `:8080`. |
| `--metrics-secure`            | `true`        | Serve metrics over HTTPS with authn/authz. `make run` sets `false`. `--metrics-cert-path`, `--metrics-cert-name` and `--metrics-cert-key` supply a certificate. |
| `--health-probe-bind-address` | `:8081`       | Address of the `/healthz` and `/readyz` probes.                                                      |
| `--leader-elect`              | `false`       | Leader election, for running more than one replica. The chart passes it through `manager.args`.     |
| `--enable-http2`              | `false`       | Allow HTTP/2 on the metrics and webhook servers.                                                    |
| `--zap-*`                     |               | Standard controller-runtime logging flags (`--zap-log-level`, `--zap-devel`, ...).                  |

### Passing flags through the chart

`manager.args` is appended after the flags the chart already sets (metrics, probes, `--collector-ttl`,
`--content-validation`, the webhook flags). Setting it replaces the default list, which is only `--leader-elect`, so keep
that entry:

```yaml
manager:
  args:
    - --leader-elect
    - --trusted-attribute=tenant=X-Scope-OrgID
    - --zap-log-level=debug
```

### Key chart values

| Value                              | Default    | Description                                                                |
|------------------------------------|------------|----------------------------------------------------------------------------|
| `manager.contentValidation`        | `enforce`  | `--content-validation` mode.                                               |
| `webhook.enabled`                  | `true`     | Install the PipelineConfig validating webhook.                             |
| `webhook.failurePolicy`            | `Fail`     | `Fail` refuses content changes while the manager is down, `Ignore` admits them. |
| `webhook.selfSigned.validityDays`  | `3650`     | Validity of the Helm-generated certificate (reused on upgrade).            |
| `webhook.certManager.enabled`      | `false`    | Use cert-manager instead, optionally with `webhook.certManager.issuerRef`. |
| `collectorTTL`                     | `5m`       | `--collector-ttl`.                                                         |
| `service.enabled`, `service.port`  | `true`, `12345` | The Connect-RPC API Service.                                          |
| `gateway.enabled`                  | `false`    | Envoy gateway in front of the API, see `gateway.auth.type`.                |
| `metrics.enable`, `metrics.port`   | `true`, `8443` | HTTPS metrics endpoint.                                                |
| `prometheus.enable`                | `false`    | ServiceMonitor (needs the Prometheus Operator).                            |
| `defaults.*`                       | enabled    | Default `CollectorGroup`, `PipelineConfig` and `CollectorGroupBinding`.    |

---

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    Kubernetes API                       │
│  ┌──────────────┐  ┌───────────────┐  ┌──────────────┐  │
│  │PipelineConfig│  │CollectorGroup │  │  CGB         │  │
│  └──────┬───────┘  └───────┬───────┘  └──────┬───────┘  │
└─────────┼──────────────────┼─────────────────┼──────────┘
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
│  ┌────────────────┐                          │          │
│  │Validating      │                          │          │
│  │webhook (opt.)  │                          │          │
│  └────────────────┘                          │          │
│  ┌───────────────────────────────────────────▼───────┐  │
│  │ Informer cache with field indexes:                │  │
│  │  .spec.tenantRef                                  │  │
│  │  .spec.collectorGroupRef                          │  │
│  │  .spec.pipelineConfigRef                          │  │
│  └────────────────────────┬──────────────────────────┘  │
│                           │  indexed lookups            │
│                           ▼                             │
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
API server uses them to look up the tenant and group bindings of a collector, and lists the namespace's
`CollectorGroup` objects from the same cache to evaluate matchers. There is no separate in-memory store.

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

`sys.env` reads the environment of the Alloy process, so set `OTLP_ENDPOINT` on your collectors. Alloy loads the content
as a module, not as the root config: it must not contain `logging`, `tracing`, `http`, `remotecfg` or `livedebugging`
blocks.

**Status fields:**

| Field                 | Description                                                                                                         |
|-----------------------|---------------------------------------------------------------------------------------------------------------------|
| `contentHash`         | SHA-256 of `spec.content` in every validation mode. Collectors compare it against their local hash to detect changes. |
| `activeBindings`      | Number of `CollectorGroupBinding` resources currently referencing this config.                                      |
| `lastValidContent`    | Last `spec.content` that parsed. Only set under `--content-validation=enforce`, which serves it instead of an invalid edit. |
| `lastValidHash`       | SHA-256 of `lastValidContent`.                                                                                      |
| `validatedGeneration` | Generation last checked under `enforce`. `0` means never checked, which tells objects that predate `enforce` apart. |
| `observedGeneration`  | Last generation the reconciler processed.                                                                           |
| `conditions`          | `ContentValid`, and `SeededUnvalidated` or `ContentTooLarge` when they apply, see [Conditions reference](#conditions-reference). |

`kubectl get pipelineconfigs` shows the `ContentValid` condition in the `Valid` column.

Deletion is blocked while `activeBindings > 0`. The count is derived from the bindings on every reconcile, so it self-heals after drift.

**Empty content is rejected.** `spec.content` must contain at least one non-whitespace character (enforced by
`minLength` and a CEL rule). Objects stored before this rule keep working through validation ratcheting, which needs
Kubernetes 1.30 or newer. Such a blank object is treated as not ready: its `contentHash` is cleared and the API answers
`Unavailable` instead of serving it.

#### Content validation

The operator parses `spec.content` with the Alloy syntax parser. The mode is set with `--content-validation`
(chart value `manager.contentValidation`), and the default is `enforce`:

| Mode      | `ContentValid` condition                                    | Admission webhook                    | What is served                                                                               |
|-----------|-------------------------------------------------------------|--------------------------------------|----------------------------------------------------------------------------------------------|
| `off`     | `True` for non-blank content                                | allows, no warnings                  | `spec.content`                                                                               |
| `warn`    | `True`, or `False` with reason `SyntaxError` and the parser message | allows, with an admission warning | `spec.content`, even when invalid (Alloy then reports Failed and keeps its last good config) |
| `enforce` | as `warn`                                                   | rejects invalid `spec.content`       | the last valid content (`status.lastValidContent`), for base and additive configs            |

`status.contentHash` is the SHA-256 of `spec.content` in every mode.

**What the check catches.** Syntax only: unterminated blocks, bad expressions, stray braces. It does not catch unknown
components, wrong arguments or broken references; Alloy reports those itself (the binding shows `Failed`). To catch
them before they reach the cluster, see [Validating configs in CI](#validating-configs-in-ci-recommended).
The parser is a Go module versioned separately from Alloy (`github.com/grafana/alloy/syntax`), so it can lag behind a
newer Alloy and flag syntax your Alloy accepts. In that case use `warn` or `off`, which keep serving the content.

**Enforce keeps the last valid content.** Every valid parse is copied into `status.lastValidContent`/`lastValidHash`
(with `status.validatedGeneration`). An invalid edit that gets past admission, for example while the webhook is down or
disabled, leaves collectors on the previous valid content, new ones included. Content that was never valid answers
`Unavailable`.

- **Upgrade seeding.** An object that predates `enforce` and still parses is seeded with its current content. One that
  does not parse keeps being served as is, and gets the condition `SeededUnvalidated=True`, so nothing that works today
  stops working. A valid edit clears it.
- **Size guard.** Content over 600 KiB is not copied into status (etcd rejects objects over about 1.5 MiB). Such an
  object gets `ContentTooLarge=True` and is served as in `warn`.
- **CRDs must be current.** Helm never upgrades `crds/`, see [Upgrading](#upgrading). On older CRDs the `lastValid*`
  fields are pruned and `enforce` serves like `warn`.

**Admission webhook.** The operator serves a validating webhook for `PipelineConfig` create and update, only when
started with `--enable-webhooks` (so `make run` and `make deploy` need no certificates). It re-checks only when
`spec.content` changed, so status and metadata updates of an existing invalid object are never blocked. The Helm chart
enables it by default (`webhook.enabled: true`) with a self-signed certificate that Helm generates
(`webhook.selfSigned`, reused on upgrade), so cert-manager is not required. To use cert-manager instead, set
`webhook.certManager.enabled=true` (optionally with `webhook.certManager.issuerRef`).

`webhook.failurePolicy` defaults to `Fail`: while the manager is unavailable, creating a `PipelineConfig` or changing
its content is refused, so invalid content cannot slip in. Set `Ignore` to admit changes instead; `enforce` still keeps
serving the last valid content then. The chart's own default `PipelineConfig` carries the label
`fleet.pkarakal.com/skip-validation`, which makes the webhook skip it, because Helm creates it before the manager can
answer. The label bypasses admission for any object that carries it, but not the reconciler, see
[Upgrading](#upgrading). To switch modes or drop the webhook, see the `helm upgrade` commands there.

---

### CollectorGroup

A named group that collectors join through their `remotecfg` `collector_group` attribute, or through `spec.matchers`.

```yaml
apiVersion: fleet.pkarakal.com/v1alpha1
kind: CollectorGroup
metadata:
  name: production-collectors
spec:
  description: "All production Alloy instances in the core pipeline"
```

A group without matchers contains the collectors whose `collector_group` attribute equals its name, even when no
`CollectorGroup` object of that name exists.

**Status fields:**

| Field            | Description                                                                   |
|------------------|-------------------------------------------------------------------------------|
| `activeBindings` | Number of `CollectorGroupBinding` resources currently referencing this group. |
| `conditions`     | `MatchersValid`, only when `spec.matchers` is set (see below).                |

Deletion is blocked while `activeBindings > 0`. The count is derived from the bindings on every reconcile, so it self-heals after drift.

---

### CollectorGroupBinding

Assigns a `PipelineConfig` to a `CollectorGroup` or to a specific collector by tenant ID. Exactly one of
`collectorGroupRef` or `tenantRef` must be set (enforced by the CRD).

```yaml
# Group-wide binding (effective priority 100 unless spec.priority is set)
apiVersion: fleet.pkarakal.com/v1alpha1
kind: CollectorGroupBinding
metadata:
  name: production-binding
spec:
  collectorGroupRef: production-collectors
  pipelineConfigRef: my-pipeline
```

```yaml
# Tenant-specific binding (effective priority 200 unless spec.priority is set)
apiVersion: fleet.pkarakal.com/v1alpha1
kind: CollectorGroupBinding
metadata:
  name: tenant-override
spec:
  tenantRef: "acme-corp-collector"
  pipelineConfigRef: my-pipeline-v2
```

| Spec field          | Description                                                                                         |
|---------------------|-----------------------------------------------------------------------------------------------------|
| `collectorGroupRef` / `tenantRef` | The target. Exactly one must be set.                                                  |
| `pipelineConfigRef` | Name of the `PipelineConfig` to serve.                                                              |
| `priority`          | Optional, 0 to 1000. Defaults to 200 (tenant) or 100 (group). See [Priority](#priority).            |
| `mode`              | `Replace` (default) or `Additive`. See [Modes](#modes).                                             |
| `expiresAt`         | Optional time after which the binding is no longer a candidate. See [Expiry](#expiry).              |

**Status fields:**

| Field                | Description                                                                                                                                                              |
|----------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `phase`              | `Pending` (not reconciled yet), `Active` (`RefsValid` is true) or `Degraded` (`RefsValid` is false)                                                                       |
| `configHash`         | Hash of the config currently being served for this binding                                                                                                                |
| `lastSyncedAt`       | Time of the last successful reconcile                                                                                                                                     |
| `conditions`         | `RefsValid`, `Overlap`, `Expired`, `CompositionValid`, `NameCollision`, see [Conditions reference](#conditions-reference)                                                  |
| `effectivePriority`  | `spec.priority`, or 200 (tenant) / 100 (group) when unset                                                                                                                 |
| `registeredTenants`  | Tenants currently registered to this binding. Each entry has the tenant `id`, the collector instance `collectorId`, `lastSeenAt` and the apply status fields (see [Collector apply status](#collector-apply-status-new)). Entries are upserted on `RegisterCollector` and `GetConfig` and evicted after the collector TTL window elapses without a refresh. |
| `appliedCount`, `failedCount`, `lastApplyError` | Derived from the apply status of `registeredTenants`, see [Collector apply status](#collector-apply-status-new). |
| `observedGeneration` | Last generation the reconciler processed.                                                                                                                                 |

`kubectl get collectorgroupbindings` shows `Phase`, `Config`, `Mode`, `Priority`, `Expires`, `Hash`, `Applied`, `Failed`,
`Synced` and `Age`.

**Dangling references.** If the binding chosen for a tenant or group points at a `PipelineConfig` that does not exist,
`GetConfig` answers `Unavailable` (not `Internal`) and does not fall through to the group or default config.

**Deterministic tie-break.** Among candidates with the same effective priority, the oldest by
`metadata.creationTimestamp` wins, then the lowest name. The same rule applies to multiple default-labeled
`PipelineConfig` resources (the operator logs `Multiple default PipelineConfigs found, using oldest`), and the config
resolver and collector registry always pick the same binding.

**Overlap condition.** A binding that loses to another binding with the same target (same `tenantRef` or same
`collectorGroupRef`) and the same effective priority gets `Overlap=True` with reason `DuplicateTarget` and a message
naming the winner. Other bindings get `Overlap=False`. The phase is not affected, and the condition clears when the
winner is deleted or retargeted. Different priorities are an explicit ranking, not an overlap. Overlaps between
different groups that match the same collector through matchers are not detected; rank them with priority.

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

Each item is `key OP "value"` (at most 16 items, 200 characters each; the CRD rejects other shapes). Keys may contain
dots, so Alloy's built-in `collector.os` and `collector.version` attributes work. Alloy always sends both of them, on top
of the attributes you set.

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

| Candidate                           | Default effective priority |
|-------------------------------------|----------------------------|
| Tenant binding (`tenantRef`)        | 200                        |
| Group binding (`collectorGroupRef`) | 100                        |
| Default `PipelineConfig`            | 0                          |

The defaults reproduce the order without priorities. `status.effectivePriority` shows the value in use (printer column
`Priority`). The default config is only used when no binding applies.

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

### Additive pipelines and binding expiry (new, opt-in)

Both features are optional. Without `spec.mode: Additive` bindings and without `spec.expiresAt`, served content and
hashes are byte-identical to before.

#### Modes

`CollectorGroupBinding.spec.mode` is `Replace` (the default when unset) or `Additive`.

- `Replace` competes for the base config as described in [Priority](#priority).
- `Additive` never competes and never affects `Overlap`. Its `PipelineConfig` is appended to the base config of every
  collector it targets (`tenantRef` equal to the collector's tenant, or `collectorGroupRef` of a group the collector
  belongs to). Priority is irrelevant for additives.

Additive bindings are appended in binding name order. If no `Replace` binding or default config applies, the additives
alone are served.

#### Composition format

Alloy loads the response as one document, so the operator appends one block per additive after the unchanged base
content. For a binding named `debug-logs` the appended text is exactly:

```
<base content>
// alloy-remote-config: additive pipeline from CollectorGroupBinding "debug-logs"
declare "arc_debug_logs" {
<additive content>
}
arc_debug_logs "default" { }
```

(a blank line precedes the comment, because every block starts with a newline). The label is `arc_` plus the binding name
with every character outside `[A-Za-z0-9_]` replaced by `_`. If two additives derive the same label, the first by
name is kept and the others are dropped from that composition; they get `NameCollision=True`. The hash is the SHA-256
of the composed document. The base components keep their IDs, so Alloy keeps running them when an additive is
added or removed.

#### Additive content must be self-contained

A `declare` body sees only its own components and arguments. It cannot reference components of the base config, and
top-level config blocks (`logging`, `tracing`, `http`, `remotecfg`, `livedebugging`) are not allowed inside it. An
additive pipeline therefore brings its own sources and exporters, for example using `sys.env` or
`remote.kubernetes.secret` for credentials. There is no wiring into the base pipeline yet.

The operator checks syntax with the Alloy parser and rejects the forbidden blocks listed above. It cannot detect
references to base components or other load-time errors, so `CompositionValid=True` means "syntactically valid".
[Validating configs in CI](#validating-configs-in-ci-recommended) catches the rest.

- The binding reconciler sets `CompositionValid` on additive bindings. `False` carries the parse error (line numbers
  count the three wrapper lines that precede your content).
- At serve time an additive that does not parse is left out; the base is never changed because of an additive. If the
  composed document still fails to parse, the base is served alone and the operator logs an error once per distinct
  composed hash.
- `NameCollision` is evaluated across the namespace, so it can be `True` for two additives that never meet on one
  collector.

#### Expiry

`spec.expiresAt` (optional, both modes) removes the binding from consideration once the time is reached. The operator
sets `Expired=True` and requeues at `expiresAt`; the phase is unchanged and the object is never deleted automatically
(GitOps first, delete it when you are ready). An expired `Replace` binding is not a candidate, so collectors fall back
to the next candidate. This is a deliberate exception to the "no fallthrough" rule, which only applies to a live
winner whose `PipelineConfig` is unusable.

Printer columns `Mode` and `Expires` show both fields. The gauge `alloy_remote_config_binding_expires_in_seconds{binding}`
is emitted only for bindings with an `expiresAt` and goes negative once it has passed.

#### Apply status

A collector registers on its base binding and on every additive binding composed into its config, so `appliedCount`,
`failedCount` and `lastApplyError` appear on additive bindings too. A `Failed` report for a composed config counts
against the base binding and every contributing additive. Additives left out at serve time are not counted.

#### Flows

**Canary.** A `Replace` binding with a high priority and an `expiresAt`: the cohort tries the new config, and the
fleet reverts by itself when the time passes. See `config/samples/fleet_v1alpha1_canary_expiring.yaml`.

**Incident.** An `Additive` debug pipeline for one cohort with a short `expiresAt`: only that cohort gets the extra
pipeline, and it disappears from the served config after the deadline. See
`config/samples/fleet_v1alpha1_additive_debug.yaml`.

**Promotion.** When the canary looks good, point the environment group's binding at the new `PipelineConfig`, then
delete the canary binding (it is not deleted automatically).

Metric `alloy_remote_config_resolution_additive_pipelines` (histogram) observes how many additives were composed into
each resolved config. The resolution `path` label is `additive` when only additives supply the config.

### Conditions reference

| Kind                   | Condition           | Reasons                                                      | Meaning                                                                                              |
|------------------------|---------------------|--------------------------------------------------------------|------------------------------------------------------------------------------------------------------|
| `PipelineConfig`       | `ContentValid`      | `ContentValid` (True); `SyntaxError`, `EmptyContent` (False) | Content is non-blank and, in `warn` and `enforce`, parses. The message of `SyntaxError` is the parser error. |
| `PipelineConfig`       | `SeededUnvalidated` | `SeededUnvalidated` (True)                                   | Pre-`enforce` content that does not parse is still served until a valid edit replaces it.            |
| `PipelineConfig`       | `ContentTooLarge`   | `ContentTooLarge` (True)                                     | Content is over 600 KiB, so no last valid copy is kept and it is served as in `warn`.                |
| `CollectorGroup`       | `MatchersValid`     | `MatchersCompiled` (True); `InvalidMatcher` (False)          | Only set when `spec.matchers` is present. When False the group has legacy membership only.           |
| `CollectorGroupBinding` | `RefsValid`        | `RefsResolved` (True); `PipelineConfigNotFound`, `PipelineConfigFetchError`, `CollectorGroupNotFound`, `CollectorGroupFetchError` (False) | The referenced resources exist. Drives `phase`. |
| `CollectorGroupBinding` | `Overlap`          | `UniqueTarget`, `NotCompeting` (False); `DuplicateTarget` (True) | A same-target, same-priority binding wins the tie-break. `NotCompeting` marks additive and expired bindings. |
| `CollectorGroupBinding` | `Expired`          | `NotExpired` (False); `ExpiresAtPassed` (True)               | Only set when `spec.expiresAt` is set.                                                               |
| `CollectorGroupBinding` | `CompositionValid` | `SyntaxValid` (True); `InvalidSyntax` (False)                | Additive bindings only. False means the content is left out of composed configs.                     |
| `CollectorGroupBinding` | `NameCollision`    | `UniqueLabel` (False); `DuplicateLabel` (True)               | Additive bindings only. True means an earlier binding derives the same `declare` label.              |

---

## Validating configs in CI (recommended)

The operator only checks syntax (see [Content validation](#content-validation)). Unknown components, unknown arguments
and broken references (`forward_to = [loki.write.typo.receiver]`) are found by Alloy when it loads the config, which
means on your collectors, and the binding then shows `Failed` with the error in `status.lastApplyError`. Run
[`alloy validate`](https://grafana.com/docs/alloy/latest/reference/cli/validate/) in CI first, before the manifests are
applied.

Use the same Alloy version as your fleet and pin it. Pass the same `--stability.level` and
`--feature.community-components.enabled` flags your collectors run with, or components outside the default stability
level are reported as unknown.

The script below extracts `spec.content` of every `PipelineConfig` in the manifests you pass (plain YAML, or the output
of `kustomize build` or `helm template`, which also covers overlays and templated content) and validates each one.
It needs `docker` and [`yq` v4](https://github.com/mikefarah/yq). Save it as `scripts/validate-pipelineconfigs.sh`:

```bash
#!/usr/bin/env bash
# Usage: validate-pipelineconfigs.sh manifest.yaml [more.yaml ...]   ("-" reads stdin)
# Runs "alloy validate" on the spec.content of every PipelineConfig found.
set -euo pipefail

# Pin the version your fleet runs, and add the flags your collectors use
# (for example --stability.level=public-preview --feature.community-components.enabled).
ALLOY_IMAGE="${ALLOY_IMAGE:-grafana/alloy:v1.21.3}"
ALLOY_FLAGS="${ALLOY_FLAGS:---stability.level=generally-available}"
# Blocks Alloy rejects (or would apply to the whole collector) in remotecfg content.
FORBIDDEN='^(logging|tracing|http|remotecfg|livedebugging)[[:space:]]*\{'

dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
alloy() { docker run --rm -v "$dir:/cfg:ro" "$ALLOY_IMAGE" "$@"; }
status=0

for manifest in "$@"; do
  if [ "$manifest" = "-" ]; then manifest="$dir/stdin.yaml"; cat >"$manifest"; fi
  ids="$(yq eval 'select(.kind == "PipelineConfig") | (.metadata.namespace // "default") + "/" + .metadata.name' "$manifest")"
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    file="${id//\//.}.alloy"
    ns="${id%%/*}"
    name="${id#*/}"
    yq eval "select(.kind == \"PipelineConfig\" and (.metadata.namespace // \"default\") == \"$ns\" and .metadata.name == \"$name\") | .spec.content" \
      "$manifest" >"$dir/$file"

    # "alloy fmt" fails on syntax errors and puts top-level blocks at column 0.
    if ! formatted="$(alloy fmt "/cfg/$file" 2>&1)"; then
      echo "FAIL $id: $formatted"; status=1; continue
    fi
    if grep -Eq "$FORBIDDEN" <<<"$formatted"; then
      echo "FAIL $id: logging, tracing, http, remotecfg and livedebugging blocks are not allowed in PipelineConfig content"
      status=1; continue
    fi

    # argument and export blocks are only reported because validate runs in root
    # mode; remotecfg loads the content as a module, where they are legal.
    # shellcheck disable=SC2086
    if ! out="$(alloy validate $ALLOY_FLAGS "/cfg/$file" 2>&1)"; then
      real="$(grep '^Error: ' <<<"$out" | grep -v -e 'validation failed' -e 'blocks only allowed inside a module' || true)"
      if [ -n "$real" ]; then
        echo "FAIL $id:"; echo "$real"; status=1; continue
      fi
    fi
    echo "ok   $id"
  done <<<"$ids"
done
exit "$status"
```

```bash
chmod +x scripts/validate-pipelineconfigs.sh
scripts/validate-pipelineconfigs.sh config/samples/*.yaml
# ok   default/debug-logs
# ok   default/pipelineconfig-sample

kustomize build overlays/prod | scripts/validate-pipelineconfigs.sh -
helm template my-release ./chart -f values-prod.yaml | scripts/validate-pipelineconfigs.sh -
```

A failing config looks like this (the exit code is 1):

```
FAIL default/ref:
Error: /cfg/default.ref.alloy:3:17: component "loki.write.y.receiver" does not exist or is out of scope
```

The same as a GitHub Actions job (example, adapt to your repository, paths and runner):

```yaml
name: validate-alloy-configs
on:
  pull_request:
    paths: ["manifests/**"]
jobs:
  alloy-validate:
    runs-on: ubuntu-latest   # has docker and yq preinstalled
    env:
      ALLOY_IMAGE: grafana/alloy:v1.21.3
      ALLOY_FLAGS: --stability.level=generally-available
    steps:
      - uses: actions/checkout@v4
      - name: Validate PipelineConfig content
        run: scripts/validate-pipelineconfigs.sh manifests/*.yaml
```

Caveats:

- **Module, not root.** Alloy loads `PipelineConfig` content through `remotecfg` as a module, but `alloy validate`
  checks a root config. It therefore reports `argument` and `export` blocks (`argument blocks only allowed inside a
  module`), which are legal in remotecfg content, and the script ignores exactly that error. In the other direction it
  accepts `logging`, `tracing`, `http`, `remotecfg` and `livedebugging` blocks that remotecfg content must not have, so
  the script rejects them itself.
- **Additive content** is validated like any other `PipelineConfig`. Remember that it cannot reference components of
  the base config at runtime. `alloy validate` runs on the additive on its own, so a reference to a base component is
  reported as a missing component, which is what Alloy would do inside the `declare` block.
- **`declare` bodies.** Alloy v1.21.3 checks component names, attribute names, types and references inside `declare`
  bodies, also when the block is never instantiated. Anything that depends on the arguments given at instantiation is
  only known to Alloy at runtime.
- **Component constraints and environment.** `alloy validate` loads the config but does not start components. Rules a
  component enforces itself (for example `scrape_timeout` larger than `scrape_interval`, or an empty URL) and everything
  that depends on the host (`sys.env` values, `remote.*` components, files, unreachable endpoints, credentials) are not
  checked and still appear as `Failed` on the binding, with the error in `status.lastApplyError`.

---

## Remote-config API

The operator embeds a [Connect-RPC](https://connectrpc.com/) server that implements the
[Grafana Alloy remote config API](https://github.com/grafana/alloy-remote-config). Alloy collectors configure a
`remotecfg` block pointing at this server; the server then resolves and returns the correct `PipelineConfig` content.

The server listens on `:12345` by default (configurable via `--connect-bind-address`) over plain HTTP, with HTTP/1.1 and
unencrypted HTTP/2 (h2c). Put TLS and authentication in front of it with the chart's Envoy gateway, an Ingress or your
own proxy.

### RPC methods

| Method                | Description                                                                                                  |
|-----------------------|--------------------------------------------------------------------------------------------------------------|
| `GetConfig`           | Resolves and returns the pipeline config for a collector, with hash-based caching. Also refreshes the collector's registration and records its apply status. |
| `RegisterCollector`   | Upserts the collector's tenant into the matching `CollectorGroupBinding.status.registeredTenants`            |
| `UnregisterCollector` | Removes the collector's tenant entry from its binding's `registeredTenants`                                  |

`GetConfig` accepts a `hash` field. If the collector's cached hash matches the current config hash, the server returns
`notModified: true` and omits the config body, because the collector already has it.

**Response codes.**

| Code                | When                                                                                                                                       |
|---------------------|--------------------------------------------------------------------------------------------------------------------------------------------|
| `OK`                | A config (or `notModified`) is returned. `RegisterCollector` and `UnregisterCollector` always return OK, see [Collector registration](#collector-registration). |
| `NotFound`          | `GetConfig`: no binding applies and no default `PipelineConfig` exists.                                                                    |
| `Unavailable`       | `GetConfig`: the winning candidate cannot be served: its `PipelineConfig` is missing, blank or has no hash yet, or (under `enforce`) never parsed and has no earlier valid content. There is no fallthrough to a lower candidate. |
| `Internal`          | `GetConfig`: any other resolution failure (for example a cache listing error). Logged as an error.                                          |
| `Unauthenticated`   | `GetConfig` with `--trusted-attribute`: the proxy header is missing or ambiguous.                                                           |
| `PermissionDenied`  | `GetConfig` with `--trusted-attribute`: the client's attribute differs from the proxy header.                                               |

### Configuring Alloy

Add a `remotecfg` block to your Alloy configuration pointing at the operator's Connect-RPC address:

```alloy
remotecfg {
  url = "http://fleet-fleet-management-api.fleet-system.svc.cluster.local:12345"

  // Identifies this collector instance
  id = constants.hostname

  poll_frequency = "1m"

  // "tenant" is matched against tenantRef, "collector_group" against collectorGroupRef,
  // and any attribute can be used in CollectorGroup matchers.
  attributes = {
    "tenant"          = "acme",
    "collector_group" = "production-collectors",
  }
}
```

The URL is `http://<release>-fleet-management-api.<namespace>.svc.cluster.local:12345` for the chart's Service (the gateway
Service is `<release>-fleet-management-gateway`, port 8080 by default). The `tenant` attribute maps to `tenantRef`
lookups; the `collector_group` attribute maps to `collectorGroupRef` lookups and to group membership.

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
      url            = "http://fleet-fleet-management-api.fleet-system.svc.cluster.local:12345"
      id             = constants.hostname
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
          image: grafana/alloy:v1.21.3
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
`remotecfg` URL matches your API Service (`<release>-fleet-management-api` in the release namespace, here release `fleet`
in `fleet-system`). Adjust both to your install. All replicas share the attribute `tenant = "acme"`, so they share one
`registeredTenants` entry.

### Collector registration

When a collector calls `RegisterCollector`, the operator upserts it into `status.registeredTenants` of the matching
`CollectorGroupBinding`. `GetConfig` refreshes the same entry on every poll, because Alloy calls `RegisterCollector` only
once on startup. Registration is best-effort: `RegisterCollector` and `UnregisterCollector` always return OK (Alloy
treats a registration error as fatal for remotecfg), failures are logged and counted in
`alloy_remote_config_registry_errors_total`, and collectors without a `tenant` attribute, or whose tenant matches no
binding (for example collectors served only by the default `PipelineConfig`), are skipped silently.
This provides a live view of which tenants are active and what config they are receiving:

```bash
# See all registered tenants for a binding
kubectl get collectorgroupbinding production-binding \
  -o jsonpath='{.status.registeredTenants}' | jq .

# [
#   { "id": "acme", "collectorId": "alloy-prod-1", "lastSeenAt": "2026-03-25T10:00:00Z" },
#   { "id": "contoso", "collectorId": "alloy-prod-2", "lastSeenAt": "2026-03-25T10:01:00Z" }
# ]

# Tenants per binding
kubectl get collectorgroupbindings -o custom-columns='NAME:.metadata.name,TENANTS:.status.registeredTenants[*].id'
```

Entries are evicted by the `CollectorGroupBinding` reconciler after the collector TTL window elapses without a refresh
(default: `5m`, configurable via `--collector-ttl` or the chart value `collectorTTL`). Setting `--collector-ttl=0`
disables eviction entirely. Evictions are counted in `alloy_remote_config_registered_tenants_evicted_total`.

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
NAME     PHASE    CONFIG   MODE      PRIORITY   EXPIRES   HASH       APPLIED   FAILED   SYNCED   AGE
canary   Active   canary   Replace   300                  3fa9c1...  4         1        12s      2d
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
  proxy_pass http://fleet-fleet-management-api.fleet-system.svc:12345;
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

The Helm chart creates a labeled default `PipelineConfig`, `CollectorGroup`, and `CollectorGroupBinding` out of the box
(see `defaults.*` in the values). The default `CollectorGroup` only contains collectors that set
`collector_group = "default"`; collectors that belong to no group and no tenant binding get the labeled default
`PipelineConfig`. If multiple resources carry the label, the oldest is used (then the lowest name) and the operator logs
a message.

---

## Observability

The operator exposes Prometheus metrics under the `alloy_remote_config` namespace, next to the controller-runtime
metrics, on the metrics endpoint (see `--metrics-bind-address`; HTTPS on `:8443` in the chart, which needs a token with
the `metrics-reader` role). Enable the ServiceMonitor with `prometheus.enable=true` in the chart, or apply the
bundled one with `kubectl apply -k config/prometheus/` for a `make deploy` install.

### RPC and HTTP metrics

| Metric                                                    | Type      | Labels                | Description                                                              |
|-----------------------------------------------------------|-----------|-----------------------|--------------------------------------------------------------------------|
| `alloy_remote_config_rpc_requests_total`                  | Counter   | `procedure`, `code`   | RPC requests by procedure and Connect status code (`ok`, `not_found`, ...) |
| `alloy_remote_config_rpc_request_duration_seconds`        | Histogram | `procedure`, `code`   | RPC request latency                                                      |
| `alloy_remote_config_http_requests_total`                 | Counter   | `method`, `code`      | All HTTP requests to the Connect-RPC server, including routes that never reach a handler |
| `alloy_remote_config_http_request_duration_seconds`       | Histogram | `method`, `code`      | HTTP request latency                                                     |
| `alloy_remote_config_config_served_total`                 | Counter   | none                  | GetConfig calls returning a fresh config body                            |
| `alloy_remote_config_config_not_modified_total`           | Counter   | none                  | GetConfig calls returning notModified                                    |
| `alloy_remote_config_registry_errors_total`               | Counter   | `op`                  | Swallowed registry errors; `op` is `register`, `unregister` or `refresh` |
| `alloy_remote_config_registry_truncated_total`            | Counter   | none                  | Registrations dropped because a binding hit the 1000-entry `registeredTenants` cap |
| `alloy_remote_config_registered_tenants_evicted_total`    | Counter   | none                  | Registered tenant entries evicted by the collector TTL                   |
| `alloy_remote_config_config_status_reports_total` (new)   | Counter   | `status`              | GetConfig calls by reported apply status (`applied`, `applying`, `failed`, `unknown`) |
| `alloy_remote_config_trusted_attribute_rejections_total` (new) | Counter | `procedure`, `reason` | Requests rejected or skipped by `--trusted-attribute`; `procedure` is `GetConfig`, `RegisterCollector` or `UnregisterCollector`, `reason` is `missing`, `ambiguous` or `mismatch` |

All counters and histograms are per replica: aggregate them with `sum`.

### Resolution metrics

| Metric                                                    | Type      | Labels            | Description                                                                                    |
|-----------------------------------------------------------|-----------|-------------------|------------------------------------------------------------------------------------------------|
| `alloy_remote_config_config_resolution_total`             | Counter   | `path`, `outcome` | One increment per resolution; `path` is the kind of the winning candidate, `outcome` is `found`, `not_found`, `not_ready` or `error` |
| `alloy_remote_config_config_resolution_duration_seconds`  | Histogram | `path`, `outcome` | Resolution latency (informer cache lookups and composition), one observation per request        |
| `alloy_remote_config_resolution_additive_pipelines` (new) | Histogram | none              | Additive pipelines composed per resolved config (buckets 0, 1, 2, 3, 5, 8)                      |

`path` is one of `tenant` (tenant binding won), `collectorgroup` (group binding won), `default` (the labeled default, or
no candidate), or `additive` (only additive bindings supplied the config). It describes the winner, so a matcher-based
group binding that outranks a tenant binding counts as `collectorgroup`. `outcome` maps to the response:
`found` is OK, `not_found` is `NotFound`, `not_ready` is `Unavailable`.

### Resource metrics

These gauges are derived from the CRDs in the watched namespace and carry a constant `namespace` label. Every replica
reports the same values: aggregate them with `max` (or `avg`), never `sum`.

| Metric                                                    | Type  | Labels      | Description                                                    |
|-----------------------------------------------------------|-------|-------------|----------------------------------------------------------------|
| `alloy_remote_config_pipeline_configs`                    | Gauge | `validity`  | PipelineConfig count by validity (`valid`, `invalid`)          |
| `alloy_remote_config_collector_group_bindings`            | Gauge | `phase`     | CollectorGroupBinding count by phase (`pending`, `active`, `degraded`) |
| `alloy_remote_config_collector_groups`                    | Gauge | `bindings`  | CollectorGroup count by binding activity (`active`, `inactive`) |
| `alloy_remote_config_deletion_blocked_resources`          | Gauge | `kind`      | Resources blocked from deletion by active bindings (`pipeline_config`, `collector_group`) |
| `alloy_remote_config_registered_tenants`                  | Gauge | none        | Registered tenant entries across all bindings                  |
| `alloy_remote_config_registered_tenants_by_status` (new)  | Gauge | `status`    | Registered tenants by apply status (`applied`, `applying`, `failed`, `unknown`) |
| `alloy_remote_config_binding_expires_in_seconds` (new)    | Gauge | `binding`   | Seconds until `spec.expiresAt`, only for bindings that set one; negative once expired |

Other metrics: `alloy_remote_config_resource_scrape_errors_total{kind}` (counter, failures listing resources during a
scrape) and `alloy_remote_config_build_info{version, goversion, git_revision}` (gauge, always 1).

---

## Project layout

```
.
├── api/v1alpha1/               # CRD type definitions and generated DeepCopy methods
├── cmd/main.go                 # Entry point: controller-manager + Connect-RPC server
├── internal/
│   ├── controller/             # Reconciliation logic for each CRD
│   ├── webhook/                # PipelineConfig validating webhook
│   ├── validate/               # Content validation mode and what gets served
│   ├── resolution/             # Pure ranking: matchers, priority, expiry
│   ├── selection/              # Deterministic oldest-then-name tie-break
│   ├── composition/            # Additive composition and Alloy syntax checks
│   ├── service/                # Connect-RPC CollectorService implementation
│   ├── server/                 # HTTP server wrapping Connect-RPC (manager.Runnable)
│   ├── adapter/
│   │   ├── kubernetes/         # Informer-cache-backed ConfigResolver and KubernetesCollectorRegistry
│   │   └── noop/               # No-op CollectorRegistry (kept for testing)
│   ├── port/                   # ConfigResolver and CollectorRegistry interfaces
│   ├── metrics/                # Prometheus metric definitions and resource collector
│   └── interceptor/            # Connect-RPC metrics and trusted-attribute interceptors
├── charts/fleet-management/    # Helm chart (CRDs, manager, webhook, gateway)
├── config/
│   ├── crd/bases/              # Generated CRD manifests (do not edit)
│   ├── rbac/                   # Generated RBAC manifests (do not edit)
│   ├── manager/                # Operator Deployment manifests
│   ├── prometheus/             # ServiceMonitor for Prometheus Operator
│   └── samples/                # Example CRs
├── docs/                       # Development guide
└── test/
    ├── compat/                 # Golden compatibility suite (envtest)
    └── e2e/                    # End-to-end tests (Kind)
```

---

## Getting started (development)

See [docs/getting-started.md](docs/getting-started.md).
