# Project Signal runner

This image is the baseline execution environment for Project Signal checks. It
contains Bash and core utilities, network and RDMA diagnostic tools, OpenMPI,
and SSH. NVIDIA-enabled workloads receive driver-matched compute and utility
libraries from the NVIDIA Container Runtime through
`NVIDIA_DRIVER_CAPABILITIES=compute,utility`.

The image intentionally does not include the CUDA Toolkit, NCCL tests, or
certification scripts. Checks and scripts are supplied separately at runtime,
so publishing this image does not run certification or mutate a Kubernetes
cluster.

The Ubuntu Noble base image is pinned by digest in `Containerfile`. Update that
digest deliberately when refreshing the base image rather than relying on a
mutable tag.

Build the image locally with:

```shell
make image-project-signal-runner-local
```

Override `CONTAINER_ENGINE`, `CONTAINER_REGISTRY`, or `SIGNAL_RUNNER_IMAGE` to
change the local engine or destination image reference.

The repository's generic image workflow publishes this image to GHCR. Push a
tag in the form `images/project-signal-runner/<version>` to publish
`ghcr.io/<repository-owner>/project-signal-runner:<version>` for amd64 and
arm64.
