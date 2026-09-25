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
