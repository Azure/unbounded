# Racer Controller

The Racer controller keeps track of Racer nodes and named caches in Kubernetes.

## ClusterCache

A `ClusterCache` gives a Racer cache a name. It applies to the whole cluster,
not a single namespace, and has no `spec` fields to configure.

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: images
```

Use a lowercase DNS name, such as `images` or `team.images`. Names can be up to
82 characters long, with at most 63 characters in each dot-separated part.

List caches with:

```sh
kubectl get clustercaches
```

Applications access a cache through its client socket at
`/run/racer/<name>/client/socket`. Pod volume mounts control access; creating a
`ClusterCache` does not grant every Pod access to it.

Deleting and recreating a `ClusterCache` creates a new cache identity, even if
you reuse the name.

## Logging

The controller writes JSON logs to stderr. `--log-level` takes `debug`,
`info` (default), `warn` or `error`; `info-1` enables logr `V(1)` messages
without the rest of `debug`. `--log-format` takes `json` (default), `text` or
`prettytext`. Error logs include a `stacktrace` field.

The operator runs the controller with no arguments. To set either flag, append
it to the `controller` container through the operator's component overrides
with `extraArgs`.
