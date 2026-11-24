# Bootstrap netbooted machines from the LAN

## Summary

Metalman already downloads the netboot image and the machine (host) image. Everything
else a machine needs at first boot comes from the internet: the `unbounded-agent`
tarball from GitHub releases, the agent rootfs image from `ghcr.io`, the Kubernetes
binaries and container image archives from `dl.k8s.io` and GitHub releases, and the
DaemonSet images that run after the node joins.

This design makes Metalman the only content source for a netbooted machine. Metalman
pre-warms every artifact a Machine needs, serves them from the read-only endpoints it
already exposes, and tells the agent to use Metalman as its registry mirror. A node
then needs a route only to Metalman, the API server, and its NTP servers.

## Goals

- A Machine reaches `Ready` with no outbound internet access from the node.
- Metalman decides what to cache from Kubernetes state, so there is no separate
  allow list to maintain and no pull-through caching.
- A restarted Metalman pod serves its cache before it can reach any registry.
- No new Machine fields. Operators configure things once at the Site level.

## Non-goals

- Workload images. Those continue to come from the upstream registries (through
  Gantry where it is configured), and are outside the bootstrap window.
- Agent self-upgrades, which use a `downloadURL` from the upgrade operation.
- Metalman running without internet access. It still needs upstream registries and
  the agent release location to fill its cache.
- NTP. Machines are assumed to reach public NTP servers.

## Network contract

| Peer | Allowed destinations |
| --- | --- |
| Node, before joining | Metalman (DHCP, TFTP, HTTP on the Metalman port), API server, NTP |
| Node, after joining | The same, and nothing else for the images Metalman pre-warmed |
| Metalman | API server, OCI registries, the agent download base URL |

## Cache layout

The cache directory defaults to `$HOME/.unbounded/metalman/cache`. The operator mounts
an emptyDir at `/var/cache/metalman` and passes `--cache-dir=/var/cache/metalman`. An
emptyDir survives container restarts and is discarded when the pod is deleted, which
matches the desired lifetime.

```
{cacheDir}/
  index.json                       reference and architecture to digest
  layouts/{digest}/                OCI image layout, manifests and blobs
  oci/{digest}/{arch}/disk/        unpacked machine and netboot images
  bootmedia/{id}/                  VirtualMedia ISOs
  agent/{key}/unbounded-agent-linux-{arch}.tar.gz
  sha256/                          attestation material
```

Two changes make the cache survive a restart:

- `index.json` records the digest last resolved for each reference and architecture.
  `LoadIndex` restores the mapping at startup and drops entries whose content is
  gone, so a restarted Metalman serves files before it reaches a registry.
- The OCI layout is copied into the cache next to the unpacked image. The layout is
  what the read-only registry serves, and it is the only copy of the blobs.

## Desired set

Metalman derives the set from cluster state on every Machine, MachineOperation and
DaemonSet event. The controller is not leader-elected, so every replica warms its own
cache; status writes stay with the leader.

| Artifact | Source |
| --- | --- |
| Machine image and netboot image | The Machine's netboot spec, as before |
| Agent rootfs image | `spec.agent.image` when set, otherwise the goal state defaults for every supported distribution, GPU and non-GPU |
| Bootstrap artifact bundle | `--bootstrap-artifacts-ref`, stamped at build time as `ghcr.io/azure/unbounded/bootstrap-artifacts:<version>-k8s-{{.KubernetesVersion}}` and rendered with the cluster version |
| Agent tarball | `spec.agent.url`, otherwise `spec.agent.baseURL`, otherwise `--agent-base-url`, otherwise the GitHub releases default |
| Images of DaemonSets that schedule on the node | `nodeSelector` matched against the provider labels plus `spec.kubernetes.nodeLabels`; init and regular containers; images with `imagePullPolicy: Never` are skipped |

Everything except the DaemonSet images is architecture-specific and is pulled for the
Machine's target architecture.

The DaemonSet match is deliberately approximate. Including an image that the node
never pulls only costs cache space; leaving one out breaks a LAN-only node. A
DaemonSet event reconciles every Machine, so late additions are picked up.

Pulling an image has two steps. The OCI layout is copied into the cache for every
image. Netboot and machine images are also unpacked, because the file server reads
`/disk` from the unpacked tree. The other images stay packed and are served straight
from the layout.

## Read-only registry

Metalman serves the distribution API on the same port as the file server:

- `GET /v2/` returns an empty version check.
- `GET` and `HEAD` on `/v2/<name>/manifests/<tag|digest>` and
  `/v2/<name>/blobs/<digest>`.

Requests are answered only from content in the cache. A reference that is not cached
returns 404; there is no pull-through. The repository name is taken from the request.
The `?ns=<registry>` query parameter that containerd adds is tried first, and the
plain name second, so a client that keeps the upstream registry in the reference also
works. A registry host that has a port cannot appear as a repository path, because a
path segment cannot contain a colon, so the agent writes the colon as an underscore
and metalman decodes it before the cache lookup.

Manifests keep their media type and are served with `Docker-Content-Digest` and their
content length. Blobs are served with `http.ServeFile`, so range requests work. A
digest that is pinned in an image reference resolves to the digest in the request
rather than through a tag lookup, which keeps DaemonSets that reference `@sha256:`
working.

Metalman is served over plain HTTP. The agent and containerd both treat loopback and
private address ranges as plain HTTP registries, so a Metalman on a LAN address needs
no certificate. A Metalman exposed through a public DNS name would.

Because the same port answers for any client on the LAN, the endpoint exposes the
pre-warmed images to the local network. That is the intent of a LAN cache; it is not
an authenticated registry.

## Agent tarball

The install script downloads `unbounded-agent` before the agent can mirror anything,
so Metalman serves the tarball itself.

- Metalman resolves the source once per Machine as described above. A version of
  `latest`, an empty version, or the development version uses the release
  `latest/download` path; an explicit version uses `download/<version>`.
- The tarball is downloaded once per key into `agent/{key}`, hashed while it streams,
  and verified against the release `checksums.txt`. When the release has no checksum
  file, Metalman logs a warning and calculates the value itself. A mismatch fails the
  download and leaves nothing in the cache.
- `GET /unbounded-agent.tar.gz` looks the Machine up by source IP, the same way the
  cloud-init files are resolved, and serves the cached file. A machine whose tarball
  is not cached yet gets 503 with `Retry-After`.

The vendor-data install environment then exports
`AGENT_URL=<ServeURL>/unbounded-agent.tar.gz` instead of a GitHub URL. A Machine that
sets `spec.agent.url` keeps its own URL, because an explicit URL is a deliberate
opt-out.

## Agent changes

The agent config gains `RegistryMirror`. Metalman sets it to its own serve URL in the
config it writes for a netbooted Machine.

- `MirrorReference` rewrites a rootfs image reference to the mirror. An `oci://`
  reference becomes `oci://<mirror>/<repository>`, a reference without a scheme
  becomes `<mirror>/<reference>`, and other schemes such as `file://` or `https://`
  are left alone. A registry host with a port keeps its colon in the mirror
  authority but writes it as an underscore in the path, so
  `registry.lan:5000/agent:v1` through mirror `10.0.0.1:5000` becomes
  `10.0.0.1:5000/registry.lan_5000/agent:v1`. The rewrite happens when the goal
  state is resolved, so the preflight probes the mirror too.
- The rendered `OfflineArtifacts.Source` is rewritten the same way.
- Containerd's `_default` hosts file lists Gantry, unless it is disabled, and the
  mirror with `pull` and `resolve` capabilities. Gantry keeps its short dial timeout
  and refuses tag lookups, so containerd falls through to Metalman for tags and
  answers digest requests from Gantry when it has the content.

Metalman sets `OfflineArtifacts.Source` to `oci://<bundle ref>` only when the bundle's
layout is already in the cache and the Machine does not set
`spec.agent.downloads`. Advertising a bundle that is not cached would break a node
that could otherwise have bootstrapped, and a Machine that overrides the download
sources has asked to keep them.

## Host images

The host images must not need a package manager at first boot, because the agent
installs `systemd-container`, `nftables`, `curl` and `util-linux` when they are
missing. `systemd-container` in particular is not part of the Ubuntu cloud image.

- `host-ubuntu2404` is rebuilt from the Ubuntu cloud image root tarball instead of
  the prebuilt qcow2, the same flow `host-ubuntu2404-nvidia` uses. It installs the
  kernel, GRUB, shim, and the four agent packages, masks the daily apt, unattended
  upgrade, motd and snap refresh units, and assembles the disk with
  `images/host-azurelinux3/assets/disk.sh`.
- `host-ubuntu2404-nvidia` adds the same packages it was missing.
- Both validate the packages at build time. The Azure Linux images already contain
  them.

The Azure Linux and Ubuntu images keep their package repositories on disk. Nothing on
a LAN-only node runs the package manager, so the repositories stay unused.

## Failure behavior

- A Machine whose images are not cached answers 503 from the file server, so firmware
  retries instead of failing mid-install.
- A cached reference whose upstream tag moved is refreshed on the five minute resync.
  The old digest stays cached until it is no longer referenced.
- A missing bootstrap artifact bundle for the cluster version leaves the agent with
  its normal download sources. The node bootstraps if it has internet access and
  fails visibly if it does not.
- A registry that is unreachable during a resync does not invalidate the cache; the
  previous digest keeps serving.

## Tests

- Registry: manifests by tag and by digest, the `?ns=` parameter and the plain
  repository name, blobs with range requests, and 404 for content that is not cached.
- Cache index: round trip, and `LoadIndex` dropping entries whose layout and disk are
  gone.
- Agent artifact: source resolution for a URL, a base URL, a version and latest;
  checksum match, mismatch, and a missing checksum file.
- Install environment and offline bundle: Metalman served unless `spec.agent.url` is
  set, and the bundle advertised only when cached and not overridden.
- Desired set: explicit and default agent images, DaemonSet node selector matching,
  init containers, `imagePullPolicy: Never`, and pull secrets.
- Agent: mirror rewriting for each reference form, and the containerd hosts file
  combinations.
- Smoke test: a Metalman smoke run with node egress restricted to Metalman, the API
  server and NTP, for a GPU and a non-GPU host image.

## Out of scope

- Authenticated or TLS registry serving.
- Garbage collection beyond replacing a reference's digest on the next reconcile.
- Image pre-warming for Machines that are not netbooted.
- NTP and other host services.
