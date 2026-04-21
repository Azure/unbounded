# Contributing to Unbounded Kubernetes

This project welcomes contributions and suggestions. Most contributions require you to agree to a
Contributor License Agreement (CLA) declaring that you have the right to, and actually do, grant us
the rights to use your contribution. For details, visit https://cla.opensource.microsoft.com.

When you submit a pull request, a CLA bot will automatically determine whether you need to provide
a CLA and decorate the PR appropriately (e.g., status check, comment). Simply follow the instructions
provided by the bot. You will only need to do this once across all repos using our CLA.

This project has adopted the [Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/).
For more information see the [Code of Conduct FAQ](https://opensource.microsoft.com/codeofconduct/faq/) or
contact [opencode@microsoft.com](mailto:opencode@microsoft.com) with any additional questions or comments.

## How to Contribute

### Reporting Issues

If you find a bug or have a feature request, please open an issue on the
[GitHub Issue Tracker](https://github.com/Azure/unbounded-kube/issues). Include as much
detail as possible: steps to reproduce, expected behavior, actual behavior, and relevant logs.

### Submitting Pull Requests

1. Fork the repository and create your branch from `main`.
2. If you've added code that should be tested, add tests.
3. Ensure the build passes: run `make machina` or `make metalman` as appropriate for the component
   you changed. These targets run formatters, linters, tests, and the Go build.
4. Make sure your code follows the existing style and conventions.
5. Write a clear PR description explaining what your change does and why.

### Coding Standards

- Do not cross `cmd/` package boundaries. For example, `cmd/agent` should not import from
  `cmd/machina`. Shared code belongs in `internal/`.
- Add tests for new behavior. Cover success, failure, and edge cases.
- Include `// Copyright (c) Microsoft Corporation.` and `// Licensed under the MIT License.`
  headers at the top of new Go source files.

### Build Instructions

- `make machina` -- build the machina controller (with format, lint, test)
- `make metalman` -- build the metalman controller (with format, lint, test)
- `make machina-build` / `make metalman-build` -- build without lint/test (used in container builds)
- `make kubectl-unbounded` -- build the kubectl plugin

### Testing the Release Pipeline Locally

Before pushing a tag, you can rehearse the GitHub Actions release workflow on
your workstation with:

```
./hack/test-release-local.sh
```

This mirrors `.github/workflows/release.yaml`: it runs `goreleaser check`,
builds the frontend, downloads CNI plugins, renders the combined manifest
tarball, runs `goreleaser release --snapshot` (skipping publish, sign, sbom,
docker), invokes `hack/test-goreleaser-hook.sh` to assert manifests and
binaries are stamped with the test tag, and `docker buildx build`s the
container images.

Useful flags: `--multi-arch` (also build linux/arm64 via QEMU),
`--include-host` (also build the large host-ubuntu2404 image), `--skip-net`
(skip net image builds entirely), `--keep-dist` (preserve `dist/` and
`build/` after the run). Override the snapshot tag with `TAG=...`.
