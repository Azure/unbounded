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
