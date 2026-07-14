# ACR Credential Provider Fixture

This directory contains a standalone internal test fixture for validating
private Azure Container Registry pulls through the kubelet exec credential
provider API. It is not part of Gantry and can be installed and validated on
its own.

## Layout

- `cmd/acr-credential-provider/` - kubelet exec plugin entrypoint.
- `internal/acrcredentialprovider/` - request handling and Azure/ACR token exchange.
- `installer/Containerfile` - public bootstrap image for the provider binary.
- `installer/install.sh` - host installer for the binary and kubelet configuration.
- `installer/daemonset.yaml` - privileged installer DaemonSet for existing and future Linux nodes.

## Published Installer

The installer DaemonSet defaults to this public immutable image:

```text
ghcr.io/azure/acr-credential-provider-installer:53b9d94e
```

Build and publish a different immutable tag when validating provider source
changes:

```bash
VERSION=$(git rev-parse --short HEAD)

GOTOOLCHAIN=auto make image-acr-credential-provider-installer-push \
  VERSION="$VERSION" \
  CONTAINER_REGISTRY=ghcr.io/azure \
  CONTAINER_ENGINE='podman --events-backend=file --cgroup-manager=cgroupfs'
```

The publisher must already be logged in to GHCR with package write access. New
GHCR packages default to private; make the package public in its GitHub package
settings and verify anonymous pulls before installing the DaemonSet.

## Validation

```bash
GOTOOLCHAIN=auto go test ./hack/acr-credential-provider/...
GOTOOLCHAIN=auto make acr-credential-provider-build
bash -n hack/acr-credential-provider/installer/install.sh
kubectl apply --dry-run=client \
  -f hack/acr-credential-provider/installer/daemonset.yaml \
  -o name
```

The installer mutates host kubelet configuration and may restart kubelet. Use
it only on clusters intended for this validation.
