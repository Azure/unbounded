<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# CRD Manifests

The YAML files in this directory are generated artifacts.

- Generate or refresh them with `make generate`.
- Do not edit `*.yaml` files here manually.
- If you need schema changes, update API types under `pkg/apis/unboundednet/v1alpha1` and then run `make generate`.

The deployment workflow applies these manifests from this directory.
