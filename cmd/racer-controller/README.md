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

## ClusterVolume compatibility

The controller also accepts `ClusterVolume` objects with `spec.type: Cache`.
Both kinds keep their own Kubernetes UID as the cache identity. Names and UIDs
must be unique across both kinds; a collision rejects the whole catalog update.
Other volume types do not enter the cache catalog.

Install the CRDs before starting the controller. Either kind may be absent.
The controller needs list and watch permission for every installed kind; a
permission error does not silently drop that kind from the catalog. Restart
the controller after installing an additional catalog CRD so it watches it.

SDK callers may set either `Cache` or `Volume` in client and origin configs.
If both fields are set, they must match.
