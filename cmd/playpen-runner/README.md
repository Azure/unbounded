# playpen-runner

`playpen-runner` hosts one disposable VM for metalman playpen smoke tests. It
serves a small HTTPS Redfish endpoint, reports connection metadata, and, by
default, builds the WireGuard/VXLAN/tap network path that connects a remote
client to the VM NIC.

The same binary also provides the `control-plane` subcommand used as the helper
container for pooled k3s control-plane pods.

## Services

The runner listens on `--listen-addr` (default `:8443`) using a self-signed
certificate stored in `--data-dir`.

| Endpoint | Purpose |
|----------|---------|
| `/redfish/v1/` | Minimal Redfish service for VM power, boot control, and serial console discovery |
| `/redfish/v1/Systems/1/Oem/Unbounded/SerialConsole/Stream` | Authenticated read-only VM serial console WebSocket stream |
| `/playpen/v1/info` | WireGuard, VXLAN, guest network, and Redfish metadata |
| `/healthz` | Liveness probe |
| `/readyz` | Readiness probe |

The Redfish implementation supports session tokens and basic authentication. It
can report power state, set boot override, and handle `ComputerSystem.Reset`:
`On` starts QEMU and `ForceOff` stops it. The default boot target is PXE.

The `ComputerSystem` resource advertises the VM serial console through the
standard `SerialConsole` property. The byte stream itself is an OEM WebSocket
protocol exposed under the same Redfish service and marked with
`ConnectTypesSupported: ["OEM"]`. The stream is read-only, requires Redfish auth,
and is reachable through the default public Redfish URL, normally the WireGuard
tunnel address `https://10.88.0.1:8443`.

## Networking

With `--configure-network=true`, startup creates these interfaces:

| Interface | Default | Purpose |
|-----------|---------|---------|
| WireGuard | `wg0` | Secure tunnel to the client |
| VXLAN | `vxlan0` | L2 overlay carried inside WireGuard |
| Bridge | `br0` | Connects VXLAN to the VM tap |
| Tap | `tap0` | QEMU NIC backend |

The runner reads or creates its WireGuard private key at
`--wireguard-private-key-file` and publishes the corresponding public key to its
pod's `playpen.unbounded-cloud.io/server-wireguard-public-key` annotation. A
client peer can be configured immediately with `--wireguard-client-public-key`.
In Kubernetes, the operator writes the client key into the runner pod's
`playpen.unbounded-cloud.io/client-wireguard-public-key` annotation and the
runner configures the peer after observing that annotation. In delayed mode,
`/readyz` returns `503` until the peer is configured.

The runner side is L2-only for the VM: it connects QEMU's tap to the bridge and
VXLAN, but does not assign the guest gateway address, enable forwarding, or NAT
guest traffic through the runner pod. The guest can only emit frames onto the
VXLAN overlay.

The default WireGuard addresses are `10.88.0.1/24` for the runner and
`10.88.0.2/32` for the client. The default guest metadata is MAC
`52:54:00:aa:bb:01`, IPv4 `192.168.200.10`, gateway `192.168.200.1`, and DNS
`8.8.8.8`. The client tunnel configures that gateway address on its side of the
VXLAN and NATs guest egress through the client's network namespace.

## VM Lifecycle

QEMU state lives in `--data-dir` (default `/var/lib/playpen-runner`). The runner
creates a qcow2 disk, copies OVMF vars, and starts `qemu-system-x86_64` on the
Redfish `On` reset action. By default, the VM has 2 vCPUs, 4096 MiB of memory, a
20G disk, UEFI firmware, and a software TPM. QEMU, serial, and swtpm logs are
written under the data directory.

The runner does not enforce a lifetime limit. In the Kubernetes playpen
deployment, the operator owns pod TTL enforcement: it deletes claimed pods on
release or `--playpen-ttl` expiration so the Deployment can create fresh runners.

## Run

Local HTTP and Redfish development can disable host networking setup:

```bash
go run ./cmd/playpen-runner \
  --configure-network=false \
  --listen-addr=127.0.0.1:8443 \
  --public-redfish-url=https://127.0.0.1:8443 \
  --redfish-username=admin \
  --redfish-password=secret
```

The full Kubernetes deployment runs this binary in a privileged container with
access to `/dev/kvm` and `/dev/net/tun`; see `deploy/playpen/04-runner.yaml.tmpl`.

## Control Plane Helper

The operator also runs `playpen-runner control-plane` next to each pooled k3s
server. The helper waits for the child API server, ensures minimal Kubernetes
discovery objects exist, and publishes the admin kubeconfig and guest-reachable
API server URL onto the parent pod annotations used during allocation.

```bash
playpen-runner control-plane \
  --kubeconfig=/etc/rancher/k3s/k3s.yaml \
  --guest-server=https://192.168.200.1:6443 \
  --kubernetes-version=v1.33.0
```

In Kubernetes, `POD_NAME` and `POD_NAMESPACE` must be provided through the
downward API so the helper can patch its own pod metadata.
