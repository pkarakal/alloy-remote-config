# Compatibility golden suite

This suite pins what `CollectorService.GetConfig` serves today, so resolver
refactors (priorities, composition, and so on) can prove that existing setups
still get byte-identical config content and the same hash.

## What it does

- Starts envtest with a real manager running all three reconcilers, so field
  indexes, finalizers and `status.contentHash` are real.
- Installs each directory under `fixtures/` into its own namespace and waits
  for reconciliation to settle.
- For every scenario and every attribute set (`tenant` only, `collector_group`
  only, both, neither, unknown tenant and group) records content, hash and
  error code in `golden/<scenario>.json`, then replays the recorded hash and
  expects `NotModified`.
- Schema upgrade test: creates the fixtures under the CRDs in `crds-old/`,
  applies the current `config/crd/bases` over them, does a no-op update of
  every object (proves it still validates) and expects the same goldens.

`04-two-tenant-bindings` is nondeterministic today (the first binding in cache
list order wins), so its golden lists both candidates and the test asserts the
served answer is one of them.

## Run

`make test` includes this package. Directly:

```
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir bin)" go test ./test/compat/...
```

## Regenerate goldens

```
go test ./test/compat/... -args -update     # or: COMPAT_UPDATE=1 go test ./test/compat/...
```

To bump `crds-old/` after a CRD change ships, copy the previous release's
`config/crd/bases/*.yaml` into it.

## Rules

- Goldens for working configs (content and hash) must never change. A diff
  there means existing users would receive different config.
- Error-code changes are allowed only with a justification in the PR
  description. Known planned ones: a binding to a missing PipelineConfig moves
  from Internal to Unavailable, and empty content gets rejected by CEL.
