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
