# Getting started

This guide is for developing the operator. To install it, see [Installation](../README.md#installation) in the README.

## Prerequisites

| Tool                                                                | Minimum version | Purpose                                                  |
|---------------------------------------------------------------------|-----------------|----------------------------------------------------------|
| [Go](https://go.dev/dl/)                                            | 1.27            | Build and test                                           |
| [kubectl](https://kubernetes.io/docs/tasks/tools/)                  | 1.35            | Interact with the cluster                                |
| [Kind](https://kind.sigs.k8s.io/)                                   | 0.20            | Local cluster for development and e2e tests (use a node image for Kubernetes 1.30 or newer) |
| [Helm](https://helm.sh/docs/intro/install/)                         | 3.x             | Install the chart (e2e tests install it)                 |
| Docker or Podman                                                    | none            | Build and load container images; run `alloy validate`    |
| [kubebuilder](https://book.kubebuilder.io/quick-start#installation) | 4.x             | Only to scaffold new APIs and controllers                |

`controller-gen`, `golangci-lint`, and `setup-envtest` are fetched and managed locally by `make`, so they do not need to
be installed globally. The cluster must run Kubernetes 1.30 or newer (CRD validation ratcheting).

## Running locally

### 1. Clone and install dependencies

```bash
git clone https://github.com/pkarakal/alloy-remote-config.git
cd alloy-remote-config
go mod download
```

### 2. Create a local cluster

```bash
kind create cluster --name alloy-remote-config
```

### 3. Install the CRDs

```bash
make install
```

### 4. Run the operator

```bash
make run
```

This runs the controller-manager in your terminal against whatever cluster your current `kubeconfig` context points to.
The Connect-RPC API server starts on `:12345` alongside the controller-manager process, and metrics are served over
plain HTTP on `:8080`. Content validation defaults to `enforce`, but the admission webhook is only served with
`--enable-webhooks` (it needs a certificate), so locally invalid content is caught by the reconciler only.

To pass other flags, run the manager directly. `make run` takes no extra arguments:

```bash
go run ./cmd/main.go --metrics-bind-address=:8080 --metrics-secure=false --connect-bind-address=:9000
```

The full list of flags is in the [README](../README.md#manager-flags). The operator resolves and serves resources from
`--namespace`, which defaults to `default` outside a cluster.

### 5. Apply the sample resources

In a separate terminal:

```bash
kubectl apply -k config/samples/
```

This creates two `PipelineConfig`s (`pipelineconfig-sample`, `debug-logs`), three `CollectorGroup`s
(`collectorgroup-sample`, `canary-org-1234`, `cluster-c-1`) and four `CollectorGroupBinding`s: a plain group binding, the
canary pair (see [Matcher cohorts](../README.md#matcher-cohorts-and-binding-priority-new-opt-in)) and an expiring
additive debug pipeline (see [Additive pipelines](../README.md#additive-pipelines-and-binding-expiry-new-opt-in)).
The pipeline reads `OTLP_ENDPOINT` from the collector's environment, and the additive one reads `DEBUG_LOKI_URL`.

> **Tip:** To designate a `PipelineConfig` as the default fallback (used when no tenant or group binding matches),
> add the label `fleet.pkarakal.com/default-pipeline-config: "true"` to it. The Helm chart does this automatically
> for the resources created under `defaults`.

After the first reconcile you should see something like this (hashes shortened):

```bash
kubectl get collectorgroupbindings
# NAME                           PHASE    CONFIG                  MODE       PRIORITY   EXPIRES                HASH       APPLIED   FAILED   SYNCED   AGE
# canary-expiring                Active   pipelineconfig-sample   Replace    400        2030-01-01T00:00:00Z   a8cefa...                     4s       4s
# canary-org-1234                Active   pipelineconfig-sample              300                               a8cefa...                     4s       4s
# collectorgroupbinding-sample   Active   pipelineconfig-sample              100                               a8cefa...                     4s       4s
# debug-logs-c-1                 Active   debug-logs              Additive   100        2030-01-01T00:00:00Z   ed8e89...                     4s       4s

kubectl get pipelineconfigs
# NAME                    VALID   AGE
# debug-logs              True    4s
# pipelineconfig-sample   True    4s
```

`kubectl get collectorgroups -o custom-columns=NAME:.metadata.name,BINDINGS:.status.activeBindings` shows how many
bindings reference each group.

### 6. Fetch a config

Query the Connect-RPC server with any Connect-compatible client, `grpcurl`, or plain HTTP:

```bash
# grpcurl (requires server reflection, enabled by default)
grpcurl -plaintext localhost:12345 list

# curl (Connect unary JSON). Attributes go in "localAttributes"; the legacy "attributes" field is ignored.
curl -X POST http://localhost:12345/collector.v1.CollectorService/GetConfig \
  -H "Content-Type: application/json" \
  -d '{"id": "my-collector", "localAttributes": {"tenant": "test-tenant", "collector_group": "collectorgroup-sample"}}'
```

The reply has the config `content` and its `hash`. Repeat the call with `"hash": "<hash>"` to get `{"notModified": true}`.
A collector in no group and with no tenant binding gets `404 not_found` unless a default `PipelineConfig` exists.
The `GetConfig` call also registers `test-tenant` on the binding it was served from:

```bash
kubectl get collectorgroupbindings -o custom-columns='NAME:.metadata.name,TENANTS:.status.registeredTenants[*].id'
```

Idle entries are evicted after the collector TTL, see [Collector registration](../README.md#collector-registration).

## Deploying to a cluster

For a real install use the Helm chart, see the README [Installation](../README.md#installation). To deploy with
Kustomize:

```bash
export IMG=<registry>/<image>:<tag>
make docker-build docker-push IMG=$IMG
make deploy IMG=$IMG
```

`make deploy` runs the manager with `--content-validation=enforce` and no admission webhook, with metrics over HTTPS on
`:8443`. To verify the operator is running:

```bash
kubectl logs -n alloy-remote-config-system \
  deployment/alloy-remote-config-controller-manager \
  -c manager -f
```

The manager serves the namespace it runs in, so create your resources there:
`kubectl apply -k config/samples/ -n alloy-remote-config-system`.

The Kustomize install has no Service for the Connect-RPC port (`12345`). Create one or use `kubectl port-forward`:

```bash
kubectl port-forward -n alloy-remote-config-system \
  deployment/alloy-remote-config-controller-manager 12345:12345
```

Change the bind address with `--connect-bind-address` in `config/manager/manager.yaml`, and the collector registration
TTL with `--collector-ttl` (default `5m`, `0` disables eviction). With the Helm chart these are `service.port` and
`collectorTTL`, and extra flags go in `manager.args` (keep `--leader-elect`).

### Metrics

`make deploy` and the chart serve metrics over HTTPS on `:8443`; `make run` serves plain HTTP on `:8080`. The flag
default is `--metrics-bind-address=0`, which disables them. The endpoint exposes the controller-runtime metrics and the
custom `alloy_remote_config_*` family, described in the README [Observability](../README.md#observability) section. If
you have the Prometheus Operator installed, apply the bundled `ServiceMonitor`:

```bash
kubectl apply -k config/prometheus/
```

## Development workflow

### After editing `*_types.go` or kubebuilder markers

```bash
make manifests generate
```

The chart ships a copy of the CRDs in `charts/fleet-management/crds/`. Keep it in sync with `config/crd/bases` when the
CRDs change.

### Running tests

Unit tests use [envtest](https://book.kubebuilder.io/reference/envtest.html), a real Kubernetes API server and etcd, so
no cluster is required. They include the golden compatibility suite in [`test/compat`](../test/compat/README.md):

```bash
make test
```

The e2e tests need Kind, kubectl, Helm and Docker. They create their own cluster (`alloy-remote-config-test-e2e`),
install the chart, run real Alloy collectors against it and delete the cluster afterwards, so do not point them at a
cluster you care about:

```bash
make test-e2e
```

### Linting

```bash
./bin/golangci-lint run --fix
```

### Common make targets

```bash
make manifests    # Regenerate CRDs and RBAC from kubebuilder markers
make generate     # Regenerate DeepCopy methods
make fmt          # Run go fmt
make vet          # Run go vet
make test         # Run unit tests
make build        # Build the operator binary (bin/manager)
make run          # Run the operator locally against the current kubeconfig context
make install      # Install the CRDs into the current cluster
make deploy       # Deploy to cluster via Kustomize (requires IMG to be set)
make undeploy     # Remove the operator from the cluster
```
