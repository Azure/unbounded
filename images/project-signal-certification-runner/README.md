# Project Signal certification runner

This image is the public, first-party runtime for Project Signal certification
workloads. It is built and published from `Azure/unbounded`; private
certification packages select it by immutable image digest.

The image contains pinned software prerequisites only. It does not contain
certification scripts, environment-specific values, credentials, thresholds,
or approval policy. Private package generation separately verifies every
embedded script against its authoritative source revision and SHA-256.

## Locked build

- Target: native `linux/amd64`, CUDA 12.8, B200/SM100.
- Base image, Ubuntu snapshot, Python wheels, and public source archives are
  pinned in `dependencies.lock.json`, `requirements.lock`,
  `ubuntu.sources`, and `ubuntu-InRelease.sha256`.
- Public workload tools include NCCL tests, NVBandwidth, GPU Burn, PyTorch,
  OpenMPI, fio, compiler/OpenMP tooling, SSH, and journal utilities.
- `runner-contract.json` declares the software tools and runtime gates supplied
  by this image. It intentionally contains no script revision or script hash
  inventory.
- Upstream source licenses are copied into
  `/usr/share/licenses/project-signal-certification-runner/`.

The image contains no SSH host private keys or `authorized_keys`. The fixed
`certification` account uses UID/GID 65532 and has no credentials. Runtime
authorization, writable mounts, host views, assigned GPUs, driver injection,
and environment policy remain caller responsibilities.

## Validate locked inputs

From the repository root:

```bash
python3 images/project-signal-certification-runner/build.py check
```

This standard-library-only check validates the selected build inputs, immutable
source references, official wheel URLs and hashes, target platform, and runner
contract. It does not fetch dependencies, build an image, or execute a
certification workload.

## Build and validate locally

Use a native Linux amd64 Docker or Podman engine:

```bash
python3 images/project-signal-certification-runner/build.py build \
  --engine docker \
  --output-directory /path/outside/the/repository/runner-evidence
```

The build driver rejects non-Linux or non-amd64 engines. A successful build
also runs the no-GPU validator with no network, no added capabilities, a
read-only root filesystem, and a separate UID/GID 65532 identity check.
Software validation is not hardware qualification.

The repository's standard `images.yaml` workflow publishes this image through
the same digest-producing GHCR path used by other `images/` definitions. Signal
manifests accept only an immutable `@sha256:` runner reference.
