# Racer dataplane image

Build from the repository root:

```sh
make image-racer-dataplane-local CONTAINER_ENGINE=docker VERSION=dev
make image-racer-dataplane-local CONTAINER_ENGINE=docker VERSION=dev-rdma RACER_NATIVE_RDMA=true
```

The default image contains the locked Rust release binary. The optional variant
also enables Cargo's `rdma` feature, compiles `native/rdma.c` against actual
`libibverbs-dev` headers/libraries, and installs `libracer_rdma.so.1`, `libibverbs1`,
and `ibverbs-providers`. Native build failure fails the image build. These are
local build targets; neither pushes an image. The build engine may fetch missing
base images, and Cargo/apt require their dependency sources or populated caches.

Both stages use Debian bookworm for compatible glibc/provider dependencies.
Rust is pinned to 1.96.0; Cargo resolves the checked-in lockfile with `--locked`.
For release provenance, override `RACER_RUST_IMAGE` and `RACER_RUNTIME_IMAGE` with
approved digest references and record the apt package versions. The default base
tags and apt repositories are mutable; `--locked` alone is not a bit-for-bit image
reproducibility guarantee. Builds are target-native (or engine-emulated), not
Go-style `TARGETARCH` cross-compilation. Version arguments label the image; they
are not injected into the Rust executable.

The process runs directly as UID/GID 65532, takes configuration from environment
variables, and receives SIGTERM directly. RDMA remains runtime-disabled by default.
Mount ownership must match the selected UID; image directory ownership does not
change bind mounts. The image does not contain cluster trust, tokens, key bundles,
or node identities. Root filesystem read-only operation requires writable slab,
identity, and `/run/racer` mounts.

See [deployment requirements](../../cmd/racer-dataplane/DEPLOYMENT.md) for storage,
kernel policy, mounts, CPU/memory budgets, and current RDMA activation limits.

## Dispatch a heap-profiling image

The image workflow accepts an optional boolean `heap_profiling`, defaulting to
`false`. Enable it only for `racer-dataplane`; other image names are rejected when
it is enabled. For example, from a checkout of the repository:

```sh
gh workflow run images.yaml --ref <branch-or-tag> \
  -f image=racer-dataplane -f platforms=linux/amd64 -F heap_profiling=true
```

This builds and pushes `ghcr.io/<owner>/racer-dataplane:<full-commit-sha>` with
`RACER_HEAP_PROFILING=true`, enabling the Cargo `heap-profiling` feature, release
debug information, and libunwind. It does not deploy the image or configure a
Parca scrape. The tag has no profiling suffix, so dispatching again at the same
commit with a different profiling setting overwrites that tag. Pin the resulting
digest when deploying. Omit `platforms` to build both `linux/amd64` and
`linux/arm64`. Omitting `heap_profiling` (or setting it to `false`) and tag-triggered
builds retain the normal non-profiling behavior.

Validate the workflow without building or pushing images:

```sh
timeout --signal=TERM --kill-after=10s 300s \
  python3 -B -m unittest discover -s hack/scripts -p test_images_workflow.py
```
