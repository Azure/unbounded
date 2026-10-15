---
title: "Cache an S3 Bucket with Racer"
weight: 10
description: "Read objects from an S3 bucket through Racer's pod-local S3 endpoint."
---

`racer-object` lets pods read an S3 bucket through the Racer cache. It is one
binary with two commands:

- `origin` runs on every Racer node and reads your bucket with your credentials.
- `sidecar` runs next to your application and serves a read-only S3 endpoint on
  pod loopback.

The read path is application -> sidecar -> Racer -> origin -> S3. Racer holds the
cached bytes. Neither command keeps its own cache.

This page is the shortest working path. For every flag, the exact S3 request
support, Azure Blob support, and the security model, see the
[racer-object reference]({{< relref "reference/racer-object" >}}).

## Before You Start

- Racer installed and running. Follow the [Racer guide]({{< relref "guides/racer" >}})
  through cluster preparation first.
- An S3 bucket, a region, and credentials that can read the objects you want.
- Permission to create cluster-scoped ClusterVolumes, and to run pods with
  hostPath mounts and UID 0 containers. The adapter does not need privileged mode.

Examples use the default operator namespace, `unbounded-system`. Substitute your
installation namespace if different.

## 1. Build the Image

From a source checkout:

```bash
make image-racer-object-local VERSION=dev
```

Push the image to a registry your nodes can pull from, then replace
`racer-object:dev` in the manifests below with your image reference, preferably
by digest. A local build alone does not reach cluster nodes.

The example manifests are in
[`deploy/racer-object`](https://github.com/Azure/unbounded/tree/main/deploy/racer-object).
They contain placeholder values. Edit them before applying.

## 2. Create the Cache Volume

```bash
kubectl apply -f deploy/racer-object/volume.yaml
kubectl get cvol racer-object
```

This creates a `ClusterVolume` named `racer-object`. Any ClusterVolume triggers
operator-managed Racer installation. ClusterVolume has no status field, so wait
on the workloads instead. Allow reconciliation to create them first:

```bash
kubectl -n unbounded-system rollout status deployment/racer-controller --timeout=5m
kubectl -n unbounded-system rollout status daemonset/racer-dataplane --timeout=5m
```

## 3. Give the Origin Credentials

The origin uses the AWS default credential chain. Prefer workload identity or
another short-lived credential provider, configured the way your provider
requires.

For a quick test with static keys, the example origin reads an optional Secret
named `racer-object-aws` as environment variables:

```bash
kubectl -n unbounded-system create secret generic racer-object-aws \
  --from-env-file="$AWS_ENV_FILE"
```

The file may contain `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and
`AWS_SESSION_TOKEN`. Never put credentials in manifests, flags, images, or source
control. Changing the Secret requires restarting the origin pods.

## 4. Deploy the Origin

Edit `deploy/racer-object/origin.yaml` and set these arguments:

| Argument | Set it to |
|----------|-----------|
| `--namespace=example-store` | A stable name for this bucket's store. See the warning below. |
| `--bucket=example-bucket` | Your bucket name. Repeat the flag for more buckets. |
| `--region=us-east-1` | Your bucket's region. |

Then apply it:

```bash
kubectl -n unbounded-system apply -f deploy/racer-object/origin.yaml
kubectl -n unbounded-system rollout status daemonset/racer-object-origin --timeout=5m
```

The origin is a DaemonSet because Racer can ask any eligible node for an origin
read, not just the node running your application. A running origin pod does not
prove upstream access; only a real object read does.

{{< callout type="warning" >}}
`--namespace` is **not** a Kubernetes namespace. It is a stable identifier for
the upstream store, and it is part of every cache key. Use the same value on the
origin and the sidecar. Change it whenever you point the adapter at a different
account, provider, or endpoint, or Racer will serve bytes cached from the old
store.
{{< /callout >}}

`--bucket` is an allowlist. If you omit it, every bucket is allowed. It limits
what the adapter will fetch; it is not a per-user access policy.

For an S3-compatible store that is not AWS, also set `--endpoint` to its URL on
the origin. Prefer HTTPS. Clients never choose the endpoint.

## 5. Read an Object

Edit `deploy/racer-object/sidecar-pod.yaml` so `--namespace` and `--bucket` match
the origin, then apply it and read an object:

```bash
kubectl -n unbounded-system apply -f deploy/racer-object/sidecar-pod.yaml
kubectl -n unbounded-system wait --for=condition=Ready pod/racer-object-example --timeout=5m
kubectl -n unbounded-system exec racer-object-example -c app -- \
  curl --fail --show-error --path-as-is --max-time 300 \
  http://127.0.0.1:8080/example-bucket/example-key
```

Replace `example-key` with a real object key. The URL is path-style:
`http://127.0.0.1:8080/BUCKET/KEY`. Use `curl --path-as-is` so the exact key path
is preserved. Add `--head` for metadata only, or
`-H 'Range: bytes=0-1023'` for a byte range.

The example application container has no credentials and no Racer socket. Only
the sidecar container mounts the socket, and only the origin pods hold the AWS
credentials.

{{< callout type="warning" >}}
The sidecar does not authenticate callers. Any `Authorization` header is ignored,
not checked. Every process in the pod can read anything the sidecar exposes. Keep
it on loopback: never put it behind a Service or host networking.
{{< /callout >}}

## Using an AWS SDK

Point the SDK at `http://127.0.0.1:8080` with path-style addressing and any
placeholder credentials. Also turn off optional checksum validation, which the
sidecar rejects with `501 NotImplemented`:

- Any SDK: set `AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED` on the application.
- Go: set `s3.Options.ResponseChecksumValidation` to
  `aws.ResponseChecksumValidationWhenRequired`.

This is a read-only subset of S3. `GET` and `HEAD` on a single object work.
Listing, writes, multipart, and presigned URLs do not. See the
[reference]({{< relref "reference/racer-object" >}}#supported-s3-requests) for the
full list.

## If a Read Fails

Start with the origin logs:

```bash
kubectl -n unbounded-system logs daemonset/racer-object-origin
```

Common causes:

- `403`: the key's bucket is not in `--bucket`, `--namespace` differs between the
  origin and the sidecar, or the origin credentials cannot read the object.
- `404`: no such object in the bucket.
- `412`: the object changed during the read, or the requested ETag no longer matches.
- `503`: Racer is not ready on that node. Application pods must run on nodes
  running Racer.

Pod readiness does not prove the cache or the bucket is reachable. The examples
have no HTTP readiness probe because Kubernetes probes target the pod IP, not pod
loopback.

## Next Steps

- [racer-object reference]({{< relref "reference/racer-object" >}}): all flags,
  request support, Azure Blob, and the security model.
- [Racer reference]({{< relref "reference/racer" >}}): cache sizing, storage, and
  metrics.
