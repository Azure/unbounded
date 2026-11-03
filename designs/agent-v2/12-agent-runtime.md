# Unbounded Agent v2: Agent runtime

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

Citations to AKS Flex Node's container install are to Azure/AKSFlexNode#370
at `c039f55`, written as `#370 <path>:<line>`.

## 1. Agent runtimes

The *agent runtime* is how the agent itself reaches a host and runs there. It
is independent of the *node runtime*, which is where the node runs
([node runtime, section 1](03-node-runtime.md#1-node-runtime)). A host is configured with one of each.

| | `host` | `containerd` | `systemd-nspawn` |
|---|---|---|---|
| What is delivered | A release archive with the agent binary | An OCI image with the agent binary and, for a consumer, its plugins and extensions (section 2) | The same image |
| Where it lives | Blue and green binaries under `/opt/unbounded/agent/bin` | The image, by digest, in the host's containerd, namespace `unbounded-agent` | The image, by digest, unpacked at `/var/lib/machines/unbounded-agent-<digest>` (section 4.2) |
| How the daemon runs | `unbounded-agent-daemon.service` runs `<current> daemon` | The same unit runs a container of the current image with `ctr run` (section 4.1) | The same unit runs a container of the current image with `systemd-nspawn` (section 4.2) |
| First install | The archive the provisioning script downloads | The install image, pulled by digest | A root file system archive of the install image, pinned by its SHA-256 (section 8) |
| AgentUpgrade | `downloadURL` of an archive and its `sha256` | `downloadURL` of `oci://<repository>` and the image digest as `sha256` (section 7) | The same as `containerd` |
| Rollback target | The `last-good` link | The last-good image in the selection file (section 3) | The same as `containerd` |
| Plugins | Staged beside each binary from the configuration or a distribution manifest, and run by host systemd | Run inside the agent's container, from the image (section 6) | The same as `containerd` |
| Target hosts | Any host the capability probes accept | Azure Container Linux | Container operating systems that run images with systemd-nspawn rather than containerd. Prototyped on Azure Container Linux only (section 4.2). |
| Node runtimes | `nspawn-container` and `host` | `nspawn-container` only (section 1.1) | `nspawn-container` only (section 1.1) |

`containerd` and `systemd-nspawn` are the *image agent runtimes*. They share
everything in sections 2, 3 and 5 to 9, and differ only in what section 4
lists.

The agent runtime is an install-time setting, `agentRuntime.type`
([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)). Provisioning always writes it from the Machine's
`spec.agent.agentRuntime` (section 9), and the agent never chooses one, so a
reinstall gets the same agent runtime. The capability probes only refuse a
host that cannot support it (section 10). A configuration file without
`agentRuntime`, such as a v1 file, means `host`. Changing the agent runtime
needs a reinstall, like changing the node runtime
([migrating from v1, section 10](10-migration-from-v1.md#10-changing-the-node-runtime-or-the-agent-runtime)).

Everything else in this design applies to every agent runtime unless a
section says otherwise. The agent's commands, configuration, state,
lifecycles, plugin protocol and on-host names are the same.

The `containerd` agent runtime targets Azure Container Linux (ACL). On ACL the
agent should not need anything of itself installed on the host to run a node,
and an image gives the agent, its plugins and its extensions one digest that
pins all of them. AKS Flex Node runs its current agent this way on ACL
(#370 `hack/acl-container/DESIGN.md:20-22`). The `systemd-nspawn` agent runtime
runs the same image on a container operating system that runs images with
systemd-nspawn instead of containerd. Nothing checks for an operating system by
name: an agent runtime runs exactly where its capability probes pass
(section 10), as the node runtimes do ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities)).

### 1.1 Why the image agent runtimes support only `nspawn-container`

Each agent runtime declares the node runtimes it supports, and configuration
validation rejects any other pair:

| Agent runtime | Node runtimes | Why |
|---|---|---|
| `host` | `nspawn-container`, `host` | |
| `containerd` | `nspawn-container` | The daemon runs on the host's containerd, and its images live in `/var/lib/containerd`. The `host` node runtime owns `containerd.service`, stops it with every node stop and gives each slot a `/var/lib/containerd` of its own ([host runtime cleanup, sections 4 and 6](07-host-runtime-cleanup.md#4-node-paths)), and its capability probes refuse a host whose own containerd cannot be disabled ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities)). An agent run by containerd would stop itself, and lose its images at the first slot change. |
| `systemd-nspawn` | `nspawn-container` | Nothing conflicts, but no one needs the pair, so it is neither supported nor tested |

The `host` node runtime is not supported on ACL under the `host` agent runtime
either: ACL's containerd is part of the OS image and holds ACL's own images,
which fails the `host` node runtime's probe ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities)).

## 2. Images

Each release publishes the agent image, `ghcr.io/azure/unbounded-agent`, for
`linux/amd64` and `linux/arm64`, beside the archives:

- It contains the v2 build at `/usr/local/bin/unbounded-agent`, the tools
  `host-exec` needs, `chroot` and `nsenter` (section 5), and no entrypoint
  script: the units run `unbounded-agent` directly.
- It contains an empty `/host`, and `/etc/resolv.conf` and `/etc/localtime`, as
  mount points: `systemd-nspawn` runs it read-only and cannot create them
  (section 4.2). `/etc/resolv.conf` is added with `COPY`. One made by a `RUN`
  step is not kept, because the build mounts its own file there while `RUN`
  runs (validation task V14, [testing, section 7](11-testing.md#7-validation-tasks)).
- It is built reproducibly from the commit's time, pushed by digest and given
  build provenance before it is tagged, as AKS Flex Node's agent image is
  (#370 `.github/workflows/agent-image-build.yml`).
- Release notes name it by digest, with the AgentUpgrade parameters that
  select it.

A consumer builds its own image `FROM` the agent image and adds its
*distribution*: its plugin executables and its extension definitions, with a
distribution manifest at `/usr/share/unbounded-agent/distribution.json`
([extensions, section 2.1](02-extensions.md#21-distributions)). The image digest then pins the agent, the
plugins and the extensions together, and an AgentUpgrade to a new image
changes all of them at once.

The `systemd-nspawn` agent runtime's first install starts from a *root file
system archive*: an uncompressed tar of an image's flattened file system,
published with its SHA-256 (section 8). It is made with standard OCI tooling,
`podman create` and then `podman export`, or `docker create` and
`docker export`. Each release publishes one of the agent image, and a consumer
makes one of its own image the same way. V14 found that `podman export` keeps
file owners, setuid modes and file capabilities, and leaves out files that a
later layer deleted. Nothing on the host can tell which image an archive was
made from, so the archive only starts the install, and the daemon always runs
an image pulled by digest (section 8).

## 3. Running on the host

The image agent runtimes run every container in one of two profiles:

| Profile | Properties | Used by |
|---|---|---|
| `daemon` | Privileged, with all capabilities; the host's network; the host root at `/host`; on an SELinux host, a domain that may talk to systemd over D-Bus (section 10); stopped with its unit | `unbounded-agent-daemon.service` |
| `task` | The same, short-lived, and able to run while the daemon or another task runs from the same image | The bootstrap and reset units, recovery, boot hooks, the LocalDNS network helper, and AgentUpgrade's checks of a candidate (section 7) |

The units follow AKS Flex Node's container install, which runs on ACL
(#370 `pkg/daemon/assets/aks-flex-node-agent-container.service:51-63`). Section 4
gives each agent runtime's command line for a profile:

| Unit | Runs |
|---|---|
| `unbounded-agent-daemon.service` | A `daemon` container of the current image running `unbounded-agent daemon`, which roots `hostfs` at `/host` |
| `unbounded-agent-daemon-recovery.service` | A `task` container of the last-good image running `recover-agent-upgrade`, which selects the last-good image again and restarts the daemon unit, as #370's recovery does (#370 `pkg/daemon/agent_upgrade_container.go:447-478`) |
| `unbounded-node-boot-hook@<m>.service` and the post-start command | A `task` container of the boot hook image running `boot-hook` (section 3.2) |
| `unbounded-localdns-network.service`, when LocalDNS is enabled | A `task` container of the boot hook image running `boot-hook localdns-network`, in place of the helper script that the `host` agent runtime writes under `/opt/unbounded/agent/libexec` ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)) |

The images in use are recorded in the selection file,
`/etc/unbounded/agent-v2/agent-image.env`, mode 0600:

| Variable | Meaning |
|---|---|
| `UNBOUNDED_AGENT_IMAGE` | The image the daemon unit runs |
| `UNBOUNDED_AGENT_LAST_GOOD_IMAGE` | The image recovery restores |
| `UNBOUNDED_AGENT_BOOT_HOOK_IMAGE` | The image boot hooks run (section 3.2) |
| `UNBOUNDED_AGENT_INSTALL_IMAGE` | The image the install started from, kept for a reinstall after reset |

Each is a repository and digest, except that under `systemd-nspawn` the install
image is the root file system archive's SHA-256 (section 8). Under
`systemd-nspawn` each variable also has a companion ending in `_DIR`, the
image's directory, because the units' command lines name a directory. The file
is written to a temporary name, synced and renamed, so a crash leaves the old
selection or the new one.

### 3.1 Readiness

The agent does not use systemd's readiness notification, `Type=notify` and
`sd_notify`, under any agent runtime ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)). Under
`containerd` it could not work: the daemon runs in a scope of its own, and
systemd accepts notifications only from members of the unit's control group
(`systemd.service(5)`, `NotifyAccess=`). Validation task V9 confirmed that a
containerized daemon's `READY=1` is ignored and the unit's start times out
([testing, section 7](11-testing.md#7-validation-tasks)). Under `systemd-nspawn` it would work, because
`--notify-ready=yes` passes the daemon's `READY=1` to the unit (V14), but one
mechanism serves every agent runtime.

Instead the daemon reports readiness by creating `/run/unbounded-agent/ready`,
and the unit waits for it:

```ini
[Service]
Type=exec
ExecStartPre=/bin/rm -f /run/unbounded-agent/ready
ExecStart=<the agent runtime's daemon command> unbounded-agent daemon
ExecStartPost=/bin/sh -c 'until [ -e /run/unbounded-agent/ready ]; do sleep 0.2; done'
TimeoutStartSec=90s
```

`TimeoutStartSec=` bounds `ExecStartPost=` as well, so a daemon that never
becomes ready fails its start, the start limit trips and the recovery unit runs,
as for any other start failure. V9 showed this on systemd 249 and 255 with
`ctr run`, with the container stopped each time, and V14 showed the same
under `systemd-nspawn`. The `host` agent runtime uses the same readiness file
and the same unit settings, so every agent runtime has one readiness mechanism
([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)).

Moving the `containerd` container's cgroup under the unit's with
`DelegateSubgroup=supervisor`, so that `sd_notify` would reach systemd, was
rejected: it exists only to make `sd_notify` work, and it replaces the scope
that AKS Flex Node's container install runs its agent in on ACL today (V9).

### 3.2 Boot hooks

Boot hooks must run a binary at least as new as the one that saved the slot's
bundle manifest ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)). Under the `host` agent runtime that is
the boot hook binary. Under an image agent runtime it is the boot hook image:
`Provision` sets `UNBOUNDED_AGENT_BOOT_HOOK_IMAGE` to the running image, and a
rollback does not change it. The hook units start that image, which is already
on the host, so a node start pulls nothing (#370 `pkg/daemon/assets/aks-flex-node-nspawn-pre-start-container@.service:18-30`).

The pre-start boot hook is a unit of its own. The post-start boot hook runs
from `ExecStartPost=` in the machine's unit override, so under
`systemd-nspawn` it runs in the machine unit's control group, under that
unit's device policy, as v1's post-start command does
(`pkg/agent/phases/rootfs/assets/service-override.conf:48`). V14 ran both
around a node machine while the daemon ran from the same image.

## 4. The image agent runtimes

The image agent runtimes share one implementation. Each supplies only three
things: where it keeps images, its command line for each profile, and its
capability probes.

| | `containerd` | `systemd-nspawn` |
|---|---|---|
| Image store | The host's containerd, namespace `unbounded-agent` | `/var/lib/machines/unbounded-agent-<digest>`, the first 12 hex digits of the image digest |
| Pull by digest | `ctr pull` through `host-exec` | The agent's own pull and unpack, `internal/agentv2/ociimage`, the code that also unpacks node root file systems ([v1 compatibility, section 3](08-v1-compatibility.md#3-code-copied-from-v1)) |
| List and remove | `ctr images ls` and `ctr images rm` | The image directories and their completion markers |
| `daemon` command | `ctr --namespace unbounded-agent run --rm --privileged --net-host --with-ns pid:/proc/1/ns/pid --mount type=bind,src=/,dst=/host,options=rbind:rslave`, in a scope of its own bound to the unit | `systemd-nspawn --quiet --register=no --keep-unit --read-only --capability=all --no-new-privileges=yes --selinux-context=system_u:system_r:spc_t:s0 --resolv-conf=bind-host --bind=/:/host -D <directory>` |
| `task` command | The same, with a container ID of its own | The same. Outside a unit, `--machine=` with a name of its own replaces `--keep-unit`. |
| What the container shares with the host | The network and PID namespaces | The network namespace. Its PID namespace is private. |
| Capability probes | Section 10 | Section 10 |

### 4.1 `containerd`

The `containerd` agent runtime follows AKS Flex Node's container install,
which runs on ACL today (#370 `pkg/daemon/assets/aks-flex-node-agent-container.service:51-63`). Its container
runs in a scope of its own, which the unit's stop removes. A daemon container
starts in about 630 ms (V14). Validation task V11 tests it end to end.

### 4.2 `systemd-nspawn`

Validation task V14's prototypes ran the `systemd-nspawn` agent runtime's
pieces on ACL 3.0.20261026, systemd 255, with SELinux enforcing. Its rules
come from what they found:

- **Images live under `/var/lib/machines`**, each unpacked to a temporary
  name with a completion marker, synced and renamed. A name never starts with
  a dot, because `machinectl clean` removes hidden images. The images appear in
  `machinectl list-images` beside the node's machines. v1 and v2 name machines
  only `kube1` and `kube2` (`pkg/agent/phases/host/preflight_existing_deployment.go:109`,
  `pkg/agent/phases/reset/machine.go:156`), so the names never collide.
- **The SELinux domain is always given.** Without `--selinux-context=`, the
  container's domain follows the label of its image's files. A tree that a
  host process unpacks is `container_var_lib_t` and runs as `spc_t`. A tree that
  the agent unpacks from its container, or that `restorecon` resets, is
  `var_lib_t` and runs as `kernel_t`, the domain that V12 found is denied
  `reset-failed`. With the option, the container runs as `spc_t` whatever the
  label.
- **All capabilities, and no new privileges.** systemd-nspawn's default
  capability set lacks `CAP_NET_ADMIN`, which the agent needs.
  `--no-new-privileges=yes` sets `no_new_privs` for everything in the
  container, as `containerd`'s containers already have it, so a plugin cannot
  regain capabilities through a setuid executable. The container still runs as
  `spc_t`, and `host-exec` works under it.
- **Read-only, with a scope of its own.** systemd-nspawn locks an image
  directory. A writable container is refused while another runs from the same
  tree, but read-only containers share it when each has its own scope:
  `--keep-unit` in a unit, or `--machine=` with a name of its own outside one.
  The daemon, boot hooks and recovery can then run from one image at once.
- **Not registered with machined.** `--register=no` keeps the agent out of
  `machinectl list`, where the node runtime counts the node's machines.
- **The host's resolver.** `--resolv-conf=bind-host` binds the host's
  `/etc/resolv.conf`. The default looks for systemd-resolved's stub file,
  which ACL does not have.
- **Stopped with its unit.** With `--keep-unit` the container is in the unit's
  control group, so stopping the unit ends everything in it, including a
  process that daemonized.

The container's PID namespace is private, so `host-exec` reaches the host's
init through `/host` (section 5), and plugins see only the container's
processes (section 6). A container starts in about 140 ms.

V14's prototypes have run only on ACL. A new target needs its own run, to
check its SELinux domain and systemd's private socket.

### 4.3 Adding an agent runtime

An agent runtime is added by:

1. implementing it in `internal/agentv2/agentruntime/<name>`
   ([implementation, section 1](09-implementation.md#1-packages)). An image agent runtime supplies the three
   pieces in section 4; any other kind implements the whole interface, as
   `host` does;
2. declaring the node runtimes it supports (section 1.1);
3. passing the agent runtime conformance suite in CI ([testing, section 1](11-testing.md#1-unit-conformance-and-golden-tests));
4. a validation task on its first target operating system ([testing, section 7](11-testing.md#7-validation-tasks));
5. adding its value to `agentRuntime.type` and the Machine API, which is
   additive.

## 5. Reaching the host

Under an image agent runtime nothing of the agent is written under `/opt` or
`/usr` on the host, and nothing the host runs comes from anywhere but the
image. The host gets only units, configuration, state and runtime files, under
`/etc`, `/var` and `/run`: the agent's state under `/etc/unbounded` and
`/var/lib/unbounded`, the plugins' state (section 6), the machines under
`/var/lib/machines`, and the files host preparation writes for any agent
runtime ([host changes, section 1](06-host-changes.md#1-what-v2-creates-on-the-host)). Everything that runs agent or plugin code, the
daemon, its recovery, boot hooks, the LocalDNS network helper and plugins,
runs from the image.

The daemon reaches the host in exactly two ways:

- **Files, through `/host` only.** Every host path the agent reads or writes is
  opened under `/host`, through one file system interface rooted there
  (`internal/agentv2/hostfs`). Nothing maps host paths into the container at
  their own paths. Under the `host` agent runtime the same interface is rooted
  at `/`.
- **Host tools, through `host-exec`.** Every host tool, such as `systemctl`,
  `machinectl`, `systemd-run`, `nft`, `ip`, `sysctl`, `modprobe`, and `ctr`
  under `containerd`, runs under `chroot /host`. `mount`, `umount` and
  `mountpoint` also enter the host's mount namespace, with `nsenter
  --mount=/proc/1/ns/mnt` inside the chroot, because mounts made in the
  container's namespace never reach the host. Inside the chroot `/proc` is the
  host's, so PID 1 is the host's init under both image agent runtimes, although
  the `systemd-nspawn` container's own PID namespace is private. AKS Flex Node
  runs `nsenter` outside the chroot (#370 `scripts/agent-image/host-exec:33-40`),
  which works only in the host's PID namespace. `systemctl` runs with
  `SYSTEMCTL_FORCE_BUS=1`, so it uses the system bus: systemd's private socket
  refuses a caller in another PID namespace. V14 showed that both work from a
  `containerd` and a `systemd-nspawn` container on ACL, including
  `reset-failed` on a unit backed by a unit file.

AKS Flex Node's entrypoint instead mounts a fixed list of host directories at
their own paths (#370 `scripts/agent-image/flex-agent-entrypoint:19-30`). A
path missing from the list is written into the container without any error. Two of
its known gaps are this: swap entries are commented out of the image's
`/etc/fstab`, not the host's (#370 `hack/acl-container/DESIGN.md:369-370`), and
GPU work finds the image's NVIDIA tools, not the host's (`:367-368`). v2's host
preparation writes several paths outside that list, such as `/etc/fstab`,
`/etc/docker/daemon.json`, `/etc/apt/apt.conf.d` and `/etc/iproute2/rt_tables`
([host changes, section 1](06-host-changes.md#1-what-v2-creates-on-the-host)). With one interface rooted at `/host`, a path that is
not handled fails instead of being written into the container.

Unit tests run every host task against a temporary `hostfs` root and fail on
any write outside it. Each image agent runtime's end-to-end suite checks that
nothing was written into the daemon's container after a full lifecycle
([testing, section 6](11-testing.md#6-ci)): under `containerd` its writable layer is empty,
and under `systemd-nspawn` its root is read-only.

## 6. Plugins

Under an image agent runtime the plugins are part of the image, and they run
where the image put them: each call is a child process of the daemon, inside
the agent's own container. Nothing of a plugin is copied to the host.

What a plugin sees is the same under every agent runtime
([extensions, section 3](02-extensions.md#3-plugin-protocol)): the request on stdin and the response on stdout,
a clean environment, `STATE_DIRECTORY` and `CACHE_DIRECTORY`, which persist
across calls, and `CREDENTIALS_DIRECTORY`, which holds its declared
credentials. How the plugin executor provides it differs:

| | `host` agent runtime | Image agent runtimes |
|---|---|---|
| Where a call runs | A transient systemd unit, from the plugin store on the host | A child process of the daemon, inside the agent's container, from the image |
| Pinning | The SHA-256 in the distribution, checked when the plugin is staged and before each call | The image digest. Each plugin is also checked against its SHA-256 in the image's distribution manifest when the image is activated. |
| User | A dynamic user of the plugin's own | A fixed unprivileged UID for each plugin, from a range the agent reserves, recorded in the install record. `root: true` keeps root. |
| Capabilities | Only those declared | Only those declared, as ambient capabilities. Processes in the agent's container run with `no_new_privs`, which `containerd` sets (V13) and `systemd-nspawn` sets with `--no-new-privileges=yes` (V14), so a setuid executable cannot raise them. |
| File system | systemd's sandbox; `permissions.paths` applies | The container's own file system, with no sandbox; `permissions.paths` does not apply. Host files are under `/host`, where a plugin that is not `root` reads and writes only what its UID may. |
| State and cache | `StateDirectory=` and `CacheDirectory=` named `unbounded-agent-plugins/<name>` | `/var/lib/unbounded-agent-plugins/<name>` and `/var/cache/unbounded-agent-plugins/<name>` on the host, reached under `/host` and owned by the plugin's UID |
| Credentials | `LoadCredential=` | The daemon reads each declared file under `/host` and writes it into a private directory in the container, readable only by the plugin's UID and removed when the call ends |
| Time limit and cleanup | `RuntimeMaxSec=`, and the unit's cgroup is stopped | The plugin runs as PID 1 of a PID namespace of its own, so every process it starts, even one that daemonizes, ends with it; the daemon kills it on a timeout, and it is killed if the daemon exits (`PR_SET_PDEATHSIG`) |
| Other processes | Hidden (`ProtectProc=invisible`) | Under `containerd`, visible, because the container shares the host's PID namespace. Under `systemd-nspawn`, only the container's own. |
| Linking | Static only | Any: the plugin uses the image's libraries |

Validation task V13 ran this executor on ACL 3.0.20261026 with SELinux
enforcing, from a privileged `containerd` container as the daemon runs, and
every property held ([testing, section 7](11-testing.md#7-validation-tasks)). Without the PID namespace, a
child the plugin daemonized kept running after the call. V14 ran it unchanged
inside a `systemd-nspawn` container.

There is no file system sandbox under an image agent runtime because the image
is the trust boundary. The consumer builds the image, the agent included
(section 2), so a sandbox inside it would not protect against the party that
supplied the plugins. An unprivileged UID still keeps a plugin from the agent's
credentials and from every other host file that only root may read or write.

### 6.1 Kubelet's exec credential

kubelet runs its exec credential itself, inside the node
([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)). It cannot use `/host`, which exists
only in the agent's container, and the plugin is not on the host, so the agent
copies the plugin's executable into the node, as AKS Flex Node does today:

- `Provision` writes it into the machine at
  `/usr/local/libexec/unbounded/plugins/<name>`, through
  `/host/var/lib/machines`, and kubelet's kubeconfig names that path.
- When an AgentUpgrade or a rollback changes the plugin, the daemon replaces
  the running machine's copy when it starts, if the SHA-256 differs, as AKS
  Flex Node's container install does with its own binary
  (#370 `pkg/daemon/agent_upgrade_container.go:548-574`). This is the one
  change the node manager makes to a running node outside `Provision`
  ([node runtime, section 1](03-node-runtime.md#1-node-runtime), rule 7).
- The copy runs in the node, with the node's libraries, so a plugin used as
  kubelet's exec credential must be statically linked under every agent
  runtime.

The node depends on nothing else from the agent to renew its credentials, so
it keeps working while the daemon restarts or recovers.

## 7. AgentUpgrade by image

The parameters are those of AKS Flex Node's container install, so its tooling
carries over (#370 `pkg/daemon/agent_upgrade_container.go:171-208`). They are
the same for both image agent runtimes:

| Parameter | Value |
|---|---|
| `downloadURL` | `oci://` and the fully qualified repository, without a tag or digest |
| `sha256` | The image digest |

A request without the digest, with a digest in the URL, or naming an archive
fails as `InvalidParameters`. Under the `host` agent runtime an `oci://` URL
fails the same way, so an operation sent to the wrong agent runtime changes
nothing.

The old daemon:

1. takes the install lock and the binary activation lock and marks the
   operation `InProgress`;
2. pulls the image by digest, through its agent runtime (section 4);
3. runs the candidate's `version` in a `task` container of the candidate, and
   requires `agent: v2`, as the `host` agent runtime does with a candidate
   binary ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade));
4. checks, in the same kind of container, each of the candidate's plugins
   against its SHA-256 in the candidate's distribution manifest;
5. writes the selection file with the candidate as current and the running
   image as last-good, and records the upgrade signal;
6. restarts the daemon unit through a transient timer, as #370 does
   (#370 `pkg/daemon/agent_upgrade.go:461-495`).

The new daemon reports ready and publishes the result, as under the `host`
agent runtime ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)). If it does not become ready, the
recovery unit runs the last-good image. V14 upgraded a `systemd-nspawn` daemon
by digest in about 2 seconds, from the request to the new daemon running.

Image cleanup runs when the daemon starts. It removes images the agent pulled
that are none of the four in the selection file. If it cannot tell which
images to keep, it removes none (#370 `pkg/daemon/agent_upgrade_container.go:576-613`).

The hidden `agent-upgrade` command takes `--image oci://<repository>` and
`--digest <sha256>` under an image agent runtime and does the same.

## 8. Bootstrap and reset

A host is provisioned by Ignition with:

- the selection file, `/etc/unbounded/agent-v2/agent-image.env`, naming the
  install image;
- the agent configuration, at the path `UNBOUNDED_AGENT_CONFIG_FILE` names;
- `unbounded-agent-bootstrap.service`, which puts the install image in place
  and runs its `start` in a `task` container, and retries until it succeeds
  or exits with the permanent-failure code, as #370's bootstrap unit does
  (#370 `scripts/aks-flex-node-agent-container.bu:94-153`);
- `unbounded-agent-reset.service`, which runs `reset` the same way.

How the bootstrap unit puts the install image in place depends on the agent
runtime:

- **`containerd`** pulls it by digest.
- **`systemd-nspawn`** unpacks the root file system archive
  (section 2) into `/var/lib/machines/unbounded-agent-install`, with
  `tar --numeric-owner --xattrs --xattrs-include=security.capability`, and
  records the archive's SHA-256 in a completion marker. The archive comes from
  one of two places. Ignition downloads it to
  `/var/lib/unbounded/agent-v2/install-rootfs.tar`, with `verification.hash`
  set to its SHA-256, where the provisioning path can add files to the
  Ignition config; otherwise the bootstrap unit downloads it with `curl` and
  checks it with `sha256sum -c`. The unit checks Ignition's copy again before
  unpacking it. An archive that does not match is refused and leaves nothing
  behind. Once the archive is unpacked, the unit removes it. `start` then pulls the current image, `agentRuntime.image`, by
  digest, so the daemon always runs an image pinned by digest. `machinectl
  pull-tar` is not used: it checks an archive only against a checksum file
  from the same server, which is not a pin. V14 ran all of this on ACL in
  about 2.6 seconds, with a 28 MB image.

`start` installs the daemon, recovery and boot hook units. `reset` removes
them, the selection file's current, last-good and boot hook images, and the
plugins' state and cache directories, and keeps the install image and the two
Ignition units, so a host can join again without being provisioned again.
Under `systemd-nspawn`, joining again needs the registry but not the archive's
server (V14).

## 9. Choosing the agent runtime

`spec.agent.agentRuntime` on the Machine takes `type: host`, the default,
`type: containerd` with `image`, or `type: systemd-nspawn` with `image` and
`installArchive`, the archive's `url` and `sha256`. `image` is the install
image by repository and digest. The field sits beside `spec.agent.nodeRuntime`
and follows the same rules ([CLI and configuration, section 2](01-cli-and-configuration.md#choosing-the-node-runtime)): every
provisioning path writes it into the agent configuration, a Machine the
daemon registers records it, and a change takes effect only at the next
reinstall, with `ReinstallPending=True` and reason `AgentRuntimeChanged` until
then. The field is not `spec.agent.image`, which already names the nspawn root
file system image (`api/machina/v1alpha3/machine_types.go:559-563`).

Provisioning a Machine with an image agent runtime writes the Ignition units
in section 8.

## 10. Host requirements

The image agent runtimes add these capability probes ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities)):

| Agent runtime | Capability | Why |
|---|---|---|
| `containerd` | The host's `containerd.service` is active and `ctr` is on the host | It runs the daemon, its recovery and the boot hooks. Nothing masks it ([host changes, section 3](06-host-changes.md#3-host-preparation-and-its-reversal)). |
| `containerd` | On an SELinux-enforcing host, a privileged container runs as a domain that may talk to systemd over D-Bus | On ACL, containers run as `spc_t`, which needs ACL's `allow spc_t kernel_t:dbus send_msg;` rule and labeled containerd content, in ACL 3.0.20261026 and later (#370 `hack/acl-container/DESIGN.md:324-340`) |
| `systemd-nspawn` | `systemd-nspawn` is on the host and the system bus is running | It runs every container, and `host-exec` reaches systemd over the system bus (section 5) |
| `systemd-nspawn` | On an SELinux-enforcing host, a test container started with `--selinux-context=system_u:system_r:spc_t:s0` runs in that domain and may talk to systemd over D-Bus | `spc_t` comes from the container SELinux policy, which a host may not have |

On ACL, the image agent runtimes also avoid a limit of the `host` agent
runtime. ACL's policy denies host processes, which all run as `kernel_t`, the
`service reload` permission that `systemctl reset-failed` needs on any unit
backed by a unit file, whether the file is `unlabeled_t`, as files written
under `/etc/systemd/system` are, or relabeled to the policy's
`systemd_unit_t`. The agent's daemon unit is such a unit, so recovery after a
failed upgrade cannot clear its start limit (Azure/AKSFlexNode#385). `spc_t`
containers are allowed, and so are transient units (validation task V12,
[testing, section 7](11-testing.md#7-validation-tasks)).

## 11. Open questions

**Registry credentials.** Pulling the agent image from a registry that needs
credentials is not supported. Under `containerd`, `ctr` takes credentials on
its command line, where any host process can read them
(#370 `hack/acl-container/DESIGN.md:360-363`); options are containerd's
registry host configuration or a credential helper. Under `systemd-nspawn`,
the agent's own pull could read them from a file only root can read, or from a
plugin.
