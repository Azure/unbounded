# Unbounded Agent v2: Host Changes

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

## 1. What v2 creates on the host

This is the complete list of what v2 creates or changes on a host, and what
`reset` removes or reverses. It replaces scripts such as Flex's
`uninstall.sh` that hardcode agent paths.

**The agent:**

| Item | Location |
|---|---|
| Agent binary, shared with v1 (`host` agent runtime) | `/opt/unbounded/agent/bin/unbounded-agent{,-blue,-green,-current,-last-good}` |
| Binary activation lock, shared with v1 | `/run/unbounded-agent-upgrade.lock` |
| Plugin store, under the `host` agent runtime only: the plugin set beside each of the blue and green binaries, each with a manifest of plugin names and SHA-256 digests. Under an image agent runtime the plugins stay in the image. | `/opt/unbounded/agent/plugins/{blue,green}/` ([extensions, section 3](02-extensions.md#3-plugin-protocol)) |
| Each plugin's state and cache directories | Under the `host` agent runtime, `/var/lib/private/unbounded-agent-plugins/<name>` and `/var/cache/private/unbounded-agent-plugins/<name>`, created by systemd for the plugin's dynamic user. Under an image agent runtime, `/var/lib/unbounded-agent-plugins/<name>` and `/var/cache/unbounded-agent-plugins/<name>`, owned by the plugin's UID ([agent runtime, section 6](12-agent-runtime.md#6-plugins)). |
| Transient units, one per plugin call, gone when the call ends, under the `host` agent runtime | `unbounded-agent-plugin-<name>-<id>.service` |
| Kubelet's exec credential, when it is a plugin, on `nspawn-container` | `/usr/local/libexec/unbounded/plugins/<name>` inside the machine ([agent runtime, section 6.1](12-agent-runtime.md#61-kubelets-exec-credential)) |
| Install record and install lock | `/var/lib/unbounded/agent-v2/install.json`, `/run/unbounded-agent-v2-install.lock` |
| Node state | `/var/lib/unbounded/agent-v2/node-state.json`, with its checksum inside ([control and state, section 4](04-control-and-state.md#4-state)) |
| Artifact cache | `/var/lib/unbounded/agent-v2/artifacts/<sha256>` |
| Slots: saved bundle manifests, and on `host` the staged binaries and files | `/var/lib/unbounded/agent-v2/slots/{a,b}` |
| Configuration copy (`config.json`), daemon credentials, upgrade signal | `/etc/unbounded/agent-v2/` |
| Readiness file | `/run/unbounded-agent/ready` ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)) |
| Daemon units | `unbounded-agent-daemon.service`, `unbounded-agent-daemon-recovery.service`. The names are the same as v1's. Under the `host` agent runtime the daemon unit runs `<current> daemon`, as v1's does; under an image agent runtime it runs a container of the current image ([agent runtime, section 3](12-agent-runtime.md#3-running-on-the-host)). Either way it waits for the readiness file, with the start timeout and start limit in [lifecycles, section 5](05-lifecycles.md#5-agentupgrade), and, as v1's does, treats the deferred exit code as success (`cmd/agent/internal/daemon/assets/unbounded-agent-daemon.service:17-22`). An extension's `agentDependencies` add a drop-in, `unbounded-agent-daemon.service.d/<extension>.conf` ([extensions, section 2](02-extensions.md#2-declarative-extensions)). |
| Boot hook binary, which a binary rollback does not revert (`host` agent runtime) | `/opt/unbounded/agent/bin/unbounded-agent-boot-hook` ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)) |
| Under an image agent runtime: the selection file, the agent's images, and the bootstrap and reset units Ignition installs | `/etc/unbounded/agent-v2/agent-image.env`; images in the host containerd's `unbounded-agent` namespace under `containerd`, or `/var/lib/machines/unbounded-agent-<digest>` and `/var/lib/machines/unbounded-agent-install` under `systemd-nspawn`, with the root file system archive at `/var/lib/unbounded/agent-v2/install-rootfs.tar` until bootstrap unpacks it; `unbounded-agent-bootstrap.service`, `unbounded-agent-reset.service` ([agent runtime, sections 3, 4 and 8](12-agent-runtime.md#3-running-on-the-host)) |
| First-boot unit, if a provisioning tool installed one | For example v1's Ignition unit, `unbounded-agent-bootstrap.service` (`internal/provision/script.go:13`) |
| Extension host units and files | As declared in each extension ([extensions, section 2](02-extensions.md#2-declarative-extensions)) |
| On a host migrated from v1: the migration record, v2's staged files until the commit, and v1's files and binary from the commit until v2's first repave | `/var/lib/unbounded/agent-v2/migration.json`, `/var/lib/unbounded/agent-v2/migration/staged/`, `/var/lib/unbounded/agent-v2/migration/v1-backup/` ([migrating from v1, sections 6 and 8](10-migration-from-v1.md#6-steps)) |
| While a rollback, or the adoption of another consumer's machine, is in progress | `unbounded-agent-migrate.service` |

**Host preparation (section 3), for both runtimes:**

| Item | Location |
|---|---|
| Undo log | `/var/lib/unbounded/agent-v2/hostprep.json` |
| Sysctls | `/etc/sysctl.d/99-kubernetes.conf`, plus values set at runtime by node components |
| nftables flush | `nftables-flush.service`, `/etc/unbounded/kube/nftables-clear.nft` |
| apt hardening | `/etc/apt/apt.conf.d/99-unbounded-no-restart-systemd`, `/etc/needrestart/conf.d/99-unbounded.conf` |
| Docker | `iptables: false` in `/etc/docker/daemon.json`, with a backup of the original; `docker.service` and `docker.socket` masked |
| Swap | Tagged comments on swap entries in `/etc/fstab`; swap units masked |
| `nspawn-container` only | The host's `kubelet.service` masked. The host's `containerd.service` is left as it is (section 3). |

**Per runtime:**

| Runtime | Items |
|---|---|
| `nspawn-container` | `/var/lib/machines/kube{1,2}`, `/etc/systemd/nspawn/kube{1,2}.nspawn`, `systemd-nspawn@kube{1,2}.service.d/`, `unbounded-node-boot-hook@kube{1,2}.service`, `/run/bpffs/kube{1,2}` |
| Both, when LocalDNS is enabled | `unbounded-localdns-network.service`, and under the `host` agent runtime its helper `/opt/unbounded/agent/libexec/unbounded-localdns-network`, written before every node start and removed by the host network reset. Under an image agent runtime the unit runs the helper from the boot hook image ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)). |
| `host` | `unboundednode.slice`, `unbounded-node.target`, `unbounded-node-prestart.service`, `containerd.service`, `kubelet.service`, `localdns.service` and its slice, one mount unit for each per-slot node path, `/run/cdi/nvidia.yaml`, the installed record under `/var/lib/unbounded/agent-v2/host-runtime/`, the slots' state directories and `active` link, and the mount points it creates for the node paths in [host runtime cleanup, section 4](07-host-runtime-cleanup.md#4-node-paths) |

**Temporary:**

| Item | Location |
|---|---|
| Maintenance containerd during an image-preserving reset (`host` only) | `/run/unbounded-agent-v2/containerd-maint/` |

On-host names are fixed. Consumers cannot rename units or paths. Only the
identity the cluster sees is configurable: the daemon CSR group
(`credentials.daemon.csr.group`).

Names that operators and other tools rely on, such as unit names and commands,
are the final names and carry no `v2` suffix. Where v1 has the same unit, v2
uses v1's name. v2's internal state keeps its `-v2` names, such as
`/var/lib/unbounded/agent-v2`, `/etc/unbounded/agent-v2` and the install lock,
because nothing outside the agent depends on them.

## 2. Host network reset

The node manager runs this after `Stop(old)` during a repave, and after
`Teardown` during a reset, for both runtimes. It hardens v1's `CleanupNetwork`
(`pkg/agent/phases/reset/network.go:48-54`).

| Step | v1 | v2 |
|---|---|---|
| LocalDNS: unit, `ip unbounded_localdns` table, `localdns` dummy link | Yes (`network.go:67-129`) | Same |
| WireGuard links | Names `wg51820` to `wg51899` only (`network.go:225-241`, `pkg/agent/phases/reset/routes.go:19-22`) | Name `wg<N>` with N of 51820 or higher, and link kind `wireguard`. Gateway port allocation has no upper bound (`internal/net/controller/gatewaypool_controller.go:411-415`). |
| `geneve0`, `vxlan0`, `ipip0`, `unbounded0`, `cbr0` | Name only (`network.go:25-31`) | Name and expected kind. A foreign link with the same name is reported, not deleted. |
| Policy routing tables and rules | Tables 51820 to 51899 | Tables of 51820 or higher that match removed WireGuard ports |
| Declared extension links, tables and chains | Not applicable | Yes ([host runtime cleanup, section 13](07-host-runtime-cleanup.md#13-extension-cleanup-declarations)) |
| `UNBOUNDED-*`, `KUBE-*` and `CNI-*` iptables chains | Not removed; v1 relies on the flush at the next start | Teardown only: delete rules that jump to them, then flush and delete the chains, for IPv4 and IPv6, on both the iptables-nft and iptables-legacy backends. The `UNBOUNDED-*` chains are defined in `internal/net/netlink/masquerade_manager.go:22`, `mss_clamp_manager.go:17`, `forward_manager.go:17`, `notrack_manager.go:19` and `gateway_policy_manager.go:76-77`. |

The whole-ruleset flush at node start is kept for both runtimes. v1's
`nftables-flush.service` runs `flush ruleset`
(`pkg/agent/phases/host/assets/nftables-clear.nft:6`). It deliberately also
clears a host firewall's default-drop policy, which would otherwise make the
kubelet unreachable (`nftables-flush.service:7-12`). In v2 it stays a
prerequisite of each runtime's boot unit, so it runs on every node start
([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)).

The shared package that implements it:

```go
package hostnet // internal/agentv2/hostnet, used by nodemanager for both runtimes

type Scope int

const (
	ScopeRepave   Scope = iota // links, routes, rules, LocalDNS
	ScopeTeardown              // ScopeRepave plus owned iptables chains and nftables tables
)

func Reset(ctx context.Context, log *slog.Logger, scope Scope, decl []extensions.Cleanup) ([]runtime.Leftover, error)
```

## 3. Host preparation and its reversal

Host preparation records an undo step in its undo log,
`/var/lib/unbounded/agent-v2/hostprep.json`, before each change. Reset replays
the steps in reverse order, for both runtimes. This makes v2's reset do what
v1's help text promises ("restore the host to its original state",
`cmd/agent/internal/cmd/reset.go:22-24`) but v1 does not do
(`cmd/agent/internal/daemon/reset.go:168-195`).

| Change (v1 source) | Undo step |
|---|---|
| `/etc/sysctl.d/99-kubernetes.conf` and `sysctl --system` (`pkg/agent/phases/host/os.go:310-335`) | Remove the file and restore the original sysctl values |
| Node component sysctls: forwarding, `rp_filter` and `src_valid_mark` on all, default and each existing interface, conntrack settings (`images/net/node-agent-init.sh:17-45`, `internal/net/controller/kubeproxy_controller.go:858-862`) | Restore the original values, recorded before the first node start |
| `nftables-flush.service` and `nftables-clear.nft` (`pkg/agent/phases/host/configure_nftables.go:65-128`) | Disable and remove them, then `systemctl try-restart nftables.service iptables.service ip6tables.service` so the host firewall returns |
| Mask docker (`pkg/agent/phases/host/disable_systemd_unit.go:30-33,59-75`) and set `iptables: false` in `/etc/docker/daemon.json` (`pkg/agent/phases/host/configure_docker.go:30-66`) | Restore the backed-up `daemon.json`, or remove it if it did not exist, and unmask |
| Mask the host's kubelet (`nspawn-container` only) | Unmask |
| Disable swap: mask units, `swapoff`, comment out fstab entries (`pkg/agent/phases/host/disable_swap.go:39-53`) | Uncomment only the fstab lines the agent commented, which are tagged, and unmask the units. Swap returns at the next boot; `swapon` is not run. |
| apt hardening drop-ins (`pkg/agent/phases/host/apt.go:39-95`) | Remove them |
| Packages installed by host preparation | Left installed and listed in the reset report; removing them could break other software |

v1 also stops and masks the host's `containerd.service` for `nspawn-container`
(`cmd/agent/internal/cmd/bootstrap.go:109-111`). v2 does not. v1 added the step
in #563 together with the `kubelet-bind-address` and
`containerd-metrics-bind-address` preflight checks, which catch the real
conflict between the host's services and the node's in the shared network
namespace. The node's containerd has its own `/run` and `/var/lib/containerd`
inside the machine, so the host's containerd does not otherwise collide with
it *(inference)*. Masking it breaks hosts where it does other work: on Azure
Container Linux it ships enabled in the read-only image and holds ACL's own
Arc agent image (`hack/acl-container/README.md:228,248` at Flex #370
`c039f55`), and under the `containerd` agent runtime it runs the agent itself
([agent runtime, section 10](12-agent-runtime.md#10-host-requirements)). AKS Flex Node has never masked it
(`pkg/daemon/start.go:16-28` at Flex `2c149e991`) and runs nspawn nodes beside
a running host containerd on ACL, including repave and NodeReboot (Flex #370's
`hack/acl-container/harness.py check`). The host's kubelet is still masked: it
would contend for the kubelet port and register its own Node.
