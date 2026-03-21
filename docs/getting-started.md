# Getting started

## Prerequisites

| Tool                                                                | Minimum version | Purpose                                     |
|---------------------------------------------------------------------|-----------------|---------------------------------------------|
| [Go](https://go.dev/dl/)                                            | 1.25            | Build and test                              |
| [kubectl](https://kubernetes.io/docs/tasks/tools/)                  | 1.35            | Interact with the cluster                   |
| [kubebuilder](https://book.kubebuilder.io/quick-start#installation) | 4.x             | Scaffold new APIs and controllers           |
| [Kind](https://kind.sigs.k8s.io/)                                   | 0.20            | Local cluster for development and e2e tests |
| Docker or Podman                                                    | —               | Build and load container images             |

`controller-gen`, `golangci-lint`, and `setup-envtest` are fetched and managed locally by `make` — they do not need to
be installed globally.

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
make manifests
kubectl apply -k config/crd/
```

### 4. Run the operator

```bash
make run
```

This runs the controller-manager in your terminal against whatever cluster your current `kubeconfig` context points to.
The Connect-RPC API server starts on `:12345` alongside the controller-manager process.

### 5. Apply the sample resources

In a separate terminal:

```bash
kubectl apply -k config/samples/
```

This creates a `PipelineConfig`, a `CollectorGroup`, and a `CollectorGroupBinding` wiring them together. After the first
reconcile you should see:

```bash
kubectl get collectorgroupbindings -o wide
# NAME                         PHASE   CONFIG                HASH       SYNCED
# collectorgroupbinding-sample Active  pipelineconfig-sample abc123...  <time>

kubectl get pipelineconfigs -o wide
# NAME                   HASH       ACTIVE-BINDINGS
# pipelineconfig-sample  abc123...  1
```

## Deploying to a cluster

```bash
export IMG=<registry>/<image>:<tag>
make docker-build docker-push IMG=$IMG
make deploy IMG=$IMG
```

To verify the operator is running:

```bash
kubectl logs -n alloy-remote-config-system \
  deployment/alloy-remote-config-controller-manager \
  -c manager -f
```

### Accessing the Connect-RPC API

The Connect-RPC server is exposed on port `12345` of the controller-manager Pod. Create a `Service` or use
`kubectl port-forward` to reach it locally:

```bash
kubectl port-forward -n alloy-remote-config-system \
  deployment/alloy-remote-config-controller-manager 12345:12345
```

You can then query it with any Connect-compatible client, `grpcurl`, or plain HTTP:

```bash
# grpcurl (requires server reflection, enabled by default)
grpcurl -plaintext localhost:12345 list

# curl (Connect unary JSON)
curl -X POST http://localhost:12345/collector.v1.CollectorService/GetConfig \
  -H "Content-Type: application/json" \
  -d '{"id": "my-collector", "attributes": {"tenant": "test-tenant", "collector_group": "production-collectors"}}'
```

### Changing the default bind address

Pass `--connect-bind-address` to the manager to override the default `:12345`:

```bash
make run ARGS="--connect-bind-address=:9000"
```

In a deployed cluster, set the flag in `config/manager/manager.yaml`.

### Metrics

The operator metrics endpoint (`:8080/metrics` by default) exposes both controller-runtime metrics and the custom
`alloy_remote_config_*` family. If you have the Prometheus Operator installed, apply the bundled `ServiceMonitor`:

```bash
kubectl apply -k config/prometheus/
```

## Development workflow

### After editing `*_types.go` or kubebuilder markers

```bash
make manifests generate
```

### Running tests

Unit tests use [envtest](https://book.kubebuilder.io/reference/envtest.html) — a real Kubernetes API server and etcd, no
cluster required:

```bash
make test
```

E2e tests require a running Kind cluster:

```bash
kind create cluster --name e2e
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
make build        # Build the operator binary
make run          # Run the operator locally against the current kubeconfig context
make deploy       # Deploy to cluster via Kustomize (requires IMG to be set)
make undeploy     # Remove the operator from the cluster
```
