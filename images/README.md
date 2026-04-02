# Container Images

This directory contains container image definitions for the project. Each subdirectory with a
`Containerfile` represents a buildable image.

## Directory Structure

```
images/
  <image-name>/
    Containerfile       # required — defines the container build
    ...                 # any supporting assets
```

The directory name becomes the image name in the registry. For example, `images/agent-ubuntu2404/`
produces `ghcr.io/<owner>/agent-ubuntu2404`.

## Building Images

Images are built automatically by the **Build Container Images** GitHub Actions workflow
(`.github/workflows/images.yaml`). All builds produce multi-arch images for `linux/amd64` and
`linux/arm64`.

### Tagged Release

Push a git tag matching the pattern `images/<image-name>/<version>`:

```bash
git tag images/agent-ubuntu2404/v1.0.0
git push origin images/agent-ubuntu2404/v1.0.0
```

This builds and pushes:

```
ghcr.io/<owner>/agent-ubuntu2404:v1.0.0
```

The `<version>` portion of the tag is used as-is for the image tag, so use whatever versioning
scheme fits (e.g. `v1.0.0`, `v2.0.0-rc.1`, `20260401`).

### On-Demand Build (workflow_dispatch)

You can also trigger a build manually from **Actions > Build Container Images > Run workflow**.
Provide the image directory name (e.g. `agent-ubuntu2404`) as the input. The resulting image is
tagged with the full git commit SHA:

```
ghcr.io/<owner>/agent-ubuntu2404:<commit-sha>
```

## Adding a New Image

1. Create a new directory under `images/` with a descriptive name.
2. Add a `Containerfile` in that directory.
3. The workflow discovers it automatically — no workflow changes needed.
4. Tag and push to build: `git tag images/<your-image>/v0.1.0 && git push origin images/<your-image>/v0.1.0`.
