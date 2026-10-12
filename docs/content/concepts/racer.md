---
title: "Racer Distributed Cache"
weight: 4
description: "How Racer is designed to let nodes share cached object data and reduce repeated reads from its source."
---

{{< callout type="note" >}}
Racer is under development. The `ClusterCache` API, Go SDK, and controller are
on `main`; the dataplane application and operator deployment integration are
still pending. This page describes the intended architecture, not a deployment
available from `main` today.
{{< /callout >}}

## Why Racer?

Many workloads read the same large files on many nodes at once: container image
layers, model weights, datasets. Without a shared cache, every node downloads
its own copy from the same registry or object store. This:

- Multiplies egress cost.
- Overloads the source when a large job starts.
- Makes scale-out as slow as the slowest download.

**Racer** is a read-through cache designed to run on each participating node.
Nodes share cached data with each other to reduce repeated reads from the
source. Apps read through a local Unix socket. When no node has the data yet,
Racer asks an **origin**, an adapter that fetches objects from your storage.
An integration can provide this adapter, or you can write one with the SDK.

Sharing and combining reads reduces source traffic; it does not guarantee
exactly one source read. Retries, eviction, expiry, and node failures can cause
another fetch.

## Architecture

Racer's planned deployment has two components:

**Controller** (`racer-controller`) -- A Deployment with one elected leader:

- Builds the member list from Nodes and dataplane pods, and publishes it to
  every node.
- Enrolls each dataplane pod and issues it a short-lived certificate.
- Creates and rotates the encryption keys for each cache.

**Dataplane** (`racer-dataplane`) -- A DaemonSet on participating nodes:

- Listens on each cache's client socket for app reads.
- Keeps pages in memory and on local disk.
- Fetches pages from other nodes over TCP, or over RDMA within the same Site
  when both nodes have compatible RDMA devices and rails (network paths).
  RDMA is not a cross-Site transport.
- Connects to the cache's origin socket on its own node when this node owns a
  page that no node has yet.

You provide the other two parts, both built with the Go SDK (`pkg/racersdk`):

- **App** -- Reads objects with `racersdk.Client`.
- **Origin** -- Fetches objects from your storage. It runs
  `racersdk.ServeOrigin`.

See [Sockets](#sockets) for how these parts connect.

The planned operator integration will deploy the controller and dataplane
when the first `ClusterCache` is created. Creating a `ClusterCache` on `main`
does not yet install these workloads.

## Core Concepts

### API

A **ClusterCache** represents a logical cache namespace:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
```

The name (`models`) is the cache name. Apps and origins pass it to the SDK, and
it is part of the socket paths below. The `ClusterCache`'s Kubernetes UID is its
identity, so deleting and recreating it gives it a new cache identity.

### Sockets

Each cache has two Unix sockets on every participating node. They point in
opposite directions:

```text
             client socket                    origin socket
   app  ------------------->  dataplane  ------------------->  origin  --->  your storage
```

| Socket | Path | Listens | Connects | Used for |
|--------|------|---------|----------|----------|
| Client | `/run/racer/<name>/client/socket` | Dataplane | App (`racersdk.Client`) | Reading objects (`Get`, `Stat`) |
| Origin | `/run/racer/<name>/origin/socket` | Origin (`racersdk.ServeOrigin`) | Dataplane | Fetching missing data from your storage |

- **Client socket** -- The app's only way into the cache. The app never talks
  to the origin or to other nodes. Racer answers from its cache, or gets the
  data from another node or an origin.
- **Origin socket** -- Racer's way out to your storage. The dataplane uses it
  only when its node owns a page that no node has yet. Apps do not use it.

One process can use both sockets. Gantry, for example, reads through the client
socket and serves the origin socket from the same process.

Access rules:

- There is no access list on the `ClusterCache`. A pod gets access by mounting
  the socket directory from the host. Mount only the directory the pod needs:
  `client/` for apps, `origin/` for origins.
- Anyone who can connect to the client socket can read all cached objects.
- `ServeOrigin` does not create the `origin/` directory; it must already exist.
  The origin does not need root, but it needs write and search permission on
  the directory.
- `ServeOrigin` creates the socket with mode `0600`, owned by the origin's
  user. The dataplane must run as the same user, or have `CAP_DAC_OVERRIDE`.
- If the origin restarts and finds an old socket file, `ServeOrigin` fails.
  Set `RecoverStaleSocket` to replace the old socket instead. This requires the
  directory to be owned by the origin's user and not writable by group or
  others.

### Objects, Keys, and Versions

- **Key** -- 32 bytes chosen by the app, often a content digest such as a
  SHA-256.
- **Version** -- One fixed set of bytes for a key, named by a strong **ETag**
  that the origin returns. The bytes of a version must never change. Every page
  of a read comes from the same version.
- **Expiry** -- The origin must set `ExpiresAt` to define freshness. Pinning a
  read to an ETag selects that exact version but does not guarantee the version
  remains available.

### Pages

Racer splits every object into **16 MiB pages**. A page is the unit for caching,
transfer between nodes, placement, and origin reads.

### Origins

An origin answers two kinds of requests:

- **HEAD** -- Returns the size, ETag, and expiry for a key.
- **GET** -- Returns one page-aligned range of a version.

Origins do not push data into Racer. Any participating node can own a page, so
the planned deployment runs the origin on each such node, usually as a DaemonSet
that mounts the cache's origin socket directory.

The client can pass `Metadata` and `Authorization` values with each read. Racer
forwards them to the origin unchanged on a cache miss. Use them to tell the
origin where to find the object and to pass the caller's credentials.

A cache hit does not call the origin to reauthorize the caller. These values
are not a per-read access check for cached data. Only share a cache among
readers allowed to read its contents; enforce any further authorization before
the request reaches Racer.

### Owners and Shares

Each page is intended to have up to **three owner nodes**. Racer picks them
with weighted rendezvous hashing over node IDs. Each node has a weight called
**shares** (default `4`). A node with more shares owns more pages. Set it with
the Node annotation `racer.unbounded-cloud.io/shares`.

Owners do not depend on the version, so a new version of an object lands on the
same nodes as the old one while membership and shares stay the same. Only
owners call the origin. Every other node gets the page from an owner.

To keep a node out of Racer, label it `racer.unbounded-cloud.io/exclude`.

## How a Read Works

The planned read path, for an app on node A and a page owned by node B:

```text
Node A                                 Node B (owns the page)

app                                    origin  --->  your storage
 |                                       ^
 | Get (client socket)                   | HEAD / GET (origin socket)
 v                                       |
dataplane  <---- TCP or RDMA ---->  dataplane
```

1. The app calls `Get`. The SDK sends it through the client socket to the
   dataplane on node A.
2. The dataplane on node A checks memory, then local disk. On a hit, it sends
   the page to the app. The origin is not called.
3. On a miss, node A asks the page's owners in rank order. Nodes connect only
   to a limited set of neighbors, so a request may pass through a few other
   nodes on the way. If an owner has the page, it sends it back.
4. If no owner has the page, an owner (node B) sends the request through its
   own origin socket to its own origin. The origin gets the app's `Key`,
   `Metadata`, and `Authorization`. Node B encrypts the page it gets back and
   makes it available to readers.
5. The page streams back to the app. The SDK holds back the last byte until
   Racer confirms the whole range arrived intact.

The origin that runs is usually not on the app's node. So every origin must be
able to reach your storage, and the `Metadata` and `Authorization` the app
sends must work from any node.

If many readers on one node want the same page at the same time, the dataplane
is designed to combine their requests and share the result.

### What Stays on Disk

The planned disk policy makes owner pages eligible for retention on the first
read. Other nodes' pages become eligible after a second read. This reduces the
chance of one-off reads pushing out useful data. This policy is called
**second-sight**. Eligibility does not guarantee retention: admission limits,
available capacity, and eviction still apply.

The planned default storage is slab files under `/var/lib/racer/slabs`, with
raw block devices as an alternative. Checkpoints are intended to let a node
recover cached pages after a restart. The cache is not durable storage; keep
the source available to refill missing pages.

## Using Racer from an App

These steps show the planned workflow. The SDK and `racersdktest` work on
`main` today; the dataplane that serves the sockets is still pending.

### 1. Create the Cache

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
```

Use `models` as `Cache` in the SDK configs below.

### 2. Decide What the App Sends

Each `Get` carries a `racersdk.Request`:

- `Key` -- Names the object. Use the same key for the same object everywhere,
  for example the SHA-256 of its content or of its URL.
- `Metadata` -- Tells the origin where the object is, for example a URL or a
  bucket and path. Up to 8 KiB.
- `Authorization` -- A credential the origin uses with your storage. Up to
  8 KiB.

Racer does not read `Metadata` or `Authorization`. It passes them to the origin
on a miss.

### 3. Write the Origin

The origin is a function that reads from your storage. `ServeOrigin` listens on
the origin socket and calls it for each request.

```go
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	err := racersdk.ServeOrigin(ctx, racersdk.OriginConfig{Cache: "models"}, fetch)
	if err != nil {
		log.Fatal(err)
	}
}

// store stands for your storage client.
func fetch(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
	obj, err := store.Lookup(ctx, r.Metadata, r.Authorization)
	if err != nil {
		// Wrap an SDK error, such as ErrNotFound or ErrForbidden.
		return racersdk.Metadata{}, nil, fmt.Errorf("%w: %w", racersdk.ErrNotFound, err)
	}

	md := racersdk.Metadata{
		Size:      obj.Size,
		ETag:      obj.ETag, // Must change when the bytes change.
		ExpiresAt: time.Now().Add(time.Hour),
	}

	if r.ETag != "" && r.ETag != md.ETag {
		return racersdk.Metadata{}, nil, racersdk.ErrVersionMismatch
	}

	if r.Head {
		return md, nil, nil
	}

	// Return at most one page, starting at r.Offset.
	start := min(r.Offset, md.Size)

	length := min(r.Length, md.Size-start)
	if length == 0 {
		return md, nil, nil
	}

	body, err := obj.ReadRange(ctx, start, length)

	return md, body, err
}
```

Rules for the origin:

- Answer `Head` with metadata only.
- Otherwise return exactly the bytes asked for, from the version you report.
- Set `ExpiresAt` on every answer. Racer stops serving the version from cache
  after this time.

### 4. Run the Origin on Every Node

Any Racer node can own a page, so run the origin as a DaemonSet on the same
nodes as the dataplane. Mount only the cache's `origin/` directory, at the same
path as on the host:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: models-origin
spec:
  selector:
    matchLabels:
      app: models-origin
  template:
    metadata:
      labels:
        app: models-origin
    spec:
      containers:
        - name: origin
          image: example.com/models-origin:latest
          volumeMounts:
            - name: racer-origin
              mountPath: /run/racer/models/origin
      volumes:
        - name: racer-origin
          hostPath:
            path: /run/racer/models/origin
            type: Directory
```

Each origin must be able to reach your storage. Run it as the same user as the
dataplane, unless the dataplane has `CAP_DAC_OVERRIDE`.

### 5. Read from the App

Mount the cache's `client/` directory at the same path as on the host. The SDK
always uses `/run/racer/<name>/client/socket`.

```yaml
      containers:
        - name: app
          volumeMounts:
            - name: racer-client
              mountPath: /run/racer/models/client
      volumes:
        - name: racer-client
          hostPath:
            path: /run/racer/models/client
            type: Directory
```

Create one `Client` per cache and share it. It is safe for concurrent use. It
does not connect until the first request, so the app can start before the
dataplane is ready.

```go
client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: "models"})
if err != nil {
	return err
}
defer client.Close()

url := "https://store.example.com/models/weights.bin"

object, err := client.Get(ctx, racersdk.Request{
	Key:           racersdk.Key(sha256.Sum256([]byte(url))),
	Metadata:      url,
	Authorization: "Bearer " + token,
})
if err != nil {
	return err
}
defer object.Close()

// io.Copy to a file or socket moves data without copying it through the app.
_, err = io.Copy(file, object)
```

Tips:

- `object.Metadata()` gives the size and ETag. `client.Stat` returns only
  metadata.
- To read part of an object, pass `racersdk.ReadOptions` with `Offset` and
  `Length`. Set `ETag` to keep reading the same version across calls.
- For objects of 16 MiB or less, set `SmallObject` so they do not wait behind
  large reads.
- Check errors with `errors.Is`. Check `ErrDestination` first: it means your
  writer failed, so do not retry Racer. `ErrUnavailable` is temporary and a
  retry may work.
- A read that stops early always returns an error, so a nil error means the
  app got every byte.

### 6. Test Without a Cluster

`racersdktest.NewClient` runs an in-process fake Racer that calls your origin
directly. It does not cache, but it uses the same request checks as
`ServeOrigin`.

```go
func TestRead(t *testing.T) {
	client := racersdktest.NewClient(t, fetch)

	object, err := client.Get(t.Context(), racersdk.Request{Key: key, Metadata: url})
	// ...
}
```

For a complete integration, see Gantry (`cmd/gantry/agent_racer.go`). It reads
image content through Racer and serves a registry origin from the same process.

## Security

The security model combines controller-issued credentials with planned
dataplane protections:

- **Enrollment** -- Each dataplane pod proves its identity to the controller
  with a projected ServiceAccount token. The controller checks that the pod
  belongs to the Racer DaemonSet, then issues a short-lived certificate
  (24 hours by default).
- **Encryption** -- Pages are encrypted with a per-cache key, both between
  nodes and on disk. The controller rotates keys every 24 hours by default.
- **Signed requests** -- Every node signs each request it sends or forwards to
  another node.
- **Credentials** -- `Authorization` and `Metadata` are intended to be sealed
  when they cross nodes and kept out of disk storage. They do not authorize
  cache hits; access to the cache socket remains a trust boundary.

## See Also

- **[Gantry Guide]({{< relref "guides/gantry" >}})** -- Container image
  distribution with Gantry.
- **[Architecture]({{< relref "reference/architecture" >}})** -- Unbounded's
  components and deployment model.
