# Architecture Tool

This directory contains the entirely client-side Architecture Tool published
at `/architecture-tool/`. It does not call an API or require a running controller.

```bash
npm ci
npm test
npm run build
```

The Hugo configuration mounts `dist/` at `/architecture-tool/`. From the repository
root, `make docs-build` builds both this app and the Hugo site.
