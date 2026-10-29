# Unbounded Agent v2: Host Changes

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

## 1. What v2 creates on the host

This is the complete list of what v2 creates or changes on a host, and what
`v2 reset` removes or reverses. It replaces scripts such as Flex's
`uninstall.sh` that hardcode agent paths.

**The agent:**

| Item | Location |
|---|---|
| Agent binary, shared with v1 | `/opt/unbounded/agent/bin/unbounded-agent{,-blue,-green,-current,-last-good}` |
| Binary activation lock, shared with v1 | `/run/unbounded-agent-upgrade.lock` |
| Install record and install lock | `/var/lib/unbounded/agent-v2/install.json`, `/run/unbounded-agent-v2-install.lock` |
| Node state | `/var/lib/unbounded/agent-v2/node-state.json` and `.sha256` |
| Artifact cache | `/var/lib/unbounded/agent-v2/artifacts/<sha256>` |
| Slots: saved bundle manifests, and on `host` the staged binaries and files | `/var/lib/unbounded/agent-v2/slots/{a,b}` |
| Configuration copy (`config.json`), daemon credentials, upgrade signal | `/etc/unbounded/agent-v2/` |
| Daemon units | `unbounded-agent-v2-daemon.service`, `unbounded-agent-v2-daemon-recovery.service` |
| First-boot unit, if a provisioning tool installed one | For example v1's Ignition unit, `unbounded-agent-bootstrap.service` (`internal/provision/script.go:13`) |
| Extension host units and files | As declared in each extension ([extensions, section 2](02-extensions.md#2-declarative-extensions)) |

**Host preparation (section 3), for both runtimes:**

| Item | Location |
|---|---|
| Undo log | `/var/lib/unbounded/agent-v2/hostprep.json` |
| Sysctls | `/etc/sysctl.d/99-kubernetes.conf`, plus values set at runtime by node components |
| nftables flush | `nftables-flush.service`, `/etc/unbounded/kube/nftables-clear.nft` |
| apt hardening | `/etc/apt/apt.conf.d/99-unbounded-no-restart-systemd`, `/etc/needrestart/conf.d/99-unbounded.conf` |
| Docker | `iptables: false` in `/etc/docker/daemon.json`, with a backup of the original; `docker.service` and `docker.socket` masked |
| Swap | Tagged comments on swap entries in `/etc/fstab`; swap units masked |
| `nspawn-container` only | The host's `containerd.service` and `kubelet.service` masked |

**Per runtime:**

| Runtime | Items |
|---|---|
| `nspawn-container` | `/var/lib/machines/kube{1,2}`, `/etc/systemd/nspawn/kube{1,2}.nspawn`, `systemd-nspawn@kube{1,2}.service.d/`, `unbounded-node-boot-hook@kube{1,2}.service`, `/run/bpffs/kube{1,2}` |
| Both, when LocalDNS is enabled | `unbounded-localdns-network.service` and `/opt/unbounded/agent/libexec/unbounded-localdns-network`, written before every node start and removed by the host network reset ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)) |
| `host` | `unboundednode.slice`, `unbounded-node.target`, `unbounded-node-prestart.service`, `containerd.service`, `kubelet.service`, `localdns.service` and its slice, `/run/cdi/nvidia.yaml`, the ownership and active-slot records and adoption backups under `/var/lib/unbounded/agent-v2/host-runtime/`, and the owned paths in [host runtime cleanup, section 4](07-host-runtime-cleanup.md#4-owned-resources) |

**Temporary:**

| Item | Location |
|---|---|
| Maintenance containerd during an image-preserving wipe (`host` only) | `/run/unbounded-agent-v2/containerd-maint/` |

On-host names are fixed. Consumers cannot rename units or paths. Only the
identity the cluster sees is configurable: the daemon CSR group
(`credentials.daemon.csr.group`).

## 2. Host network reset

The node manager runs this after `Stop(old)` during a repave, and during
Teardown, for both runtimes. It hardens v1's `CleanupNetwork`
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
| Mask the host's containerd and kubelet (`nspawn-container` only) | Unmask |
| Disable swap: mask units, `swapoff`, comment out fstab entries (`pkg/agent/phases/host/disable_swap.go:39-53`) | Uncomment only the fstab lines the agent commented, which are tagged, and unmask the units. Swap returns at the next boot; `swapon` is not run. |
| apt hardening drop-ins (`pkg/agent/phases/host/apt.go:39-95`) | Remove them |
| Packages installed by host preparation | Left installed and listed in the reset report; removing them could break other software |
