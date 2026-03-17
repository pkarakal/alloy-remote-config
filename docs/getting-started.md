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
