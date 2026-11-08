# Compatibility golden suite

This suite pins what `CollectorService.GetConfig` serves today, so resolver
refactors (priorities, composition, and so on) can prove that existing setups
still get byte-identical config content and the same hash.

## What it does

- Starts envtest with a real manager running all three reconcilers, so field
  indexes, finalizers and `status.contentHash` are real. The reconcilers and
  the resolver run with `--content-validation=enforce` (the default), so the
  goldens also prove that enforce serves the same content and hash as before.
- Installs each directory under `fixtures/` into its own namespace and waits
  for reconciliation to settle.
- For every scenario and every attribute set (`tenant` only, `collector_group`
  only, both, neither, unknown tenant and group) records content, hash and
  error code in `golden/<scenario>.json`, then replays the recorded hash and
  expects `NotModified`.
- Schema upgrade test: creates the fixtures under the CRDs in `crds-old/`,
  applies the current `config/crd/bases` over them, does a no-op update of
  every object (proves it still validates) and expects the same goldens.

`04-two-tenant-bindings` has two bindings for one tenant. The oldest
creationTimestamp wins and the name breaks ties (envtest has 1s resolution, so
the name decides here), which makes the answer deterministic.

Other specs here: blank content is rejected on create, a blank object created
under `crds-old/` survives the CRD upgrade and answers Unavailable, the
`Overlap` condition appears on the losing binding and clears when the winner is
deleted, and `status.activeBindings` of `PipelineConfig` and `CollectorGroup` is
recounted when a binding moves and blocks deletion while referenced
(`counts_test.go`).

## Run

`make test` includes this package. Directly:

```
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir "$PWD/bin")" go test ./test/compat/...
```

## Regenerate goldens

```
go test ./test/compat/... -args -update     # or: COMPAT_UPDATE=1 go test ./test/compat/...
```

To bump `crds-old/` after a CRD change ships, copy the previous release's
`config/crd/bases/*.yaml` into it.

## Justified changes

- `04-two-tenant-bindings`, `tenant-only` and `both`: from "one of two
  candidates" to the deterministic winner `acme-config-a` (oldest binding, then
  lowest name). The served content and hash are the ones the old golden already
  listed as a candidate, so no existing config text or hash changed.
- `05-binding-missing-config`, `tenant-only` and `both`: `internal` becomes
  `unavailable`. A dangling binding is an expected, recoverable state, not a
  server fault. It still does not fall through to the group or default config.
- `06-not-ready-default` is removed. Empty content is now rejected by the CRD,
  so the scenario can no longer be created. The same behavior is covered in
  `schema_test.go`: a blank object created under `crds-old/` keeps validating
  after the upgrade (ratcheting) and resolves to Unavailable, and creating
  blank or whitespace content is rejected.

## Rules

- Goldens for working configs (content and hash) must never change. A diff
  there means existing users would receive different config.
- Error-code changes are allowed only with a justification in the PR
  description. See "Justified changes" above for the ones made so far.
