# metalman

Metalman provisions bare-metal Kubernetes nodes with DHCP/TFTP or UEFI HTTP
boot, optional Redfish control, and TPM 2.0 attestation.

## Runtime Roles

Metalman is split into three roles:

| Command | Responsibility | Kubernetes access | Host networking |
|---------|----------------|-------------------|-----------------|
| `metalman controller` | MachineOperation, Redfish, immutable session creation, OCI preparation | Yes, leader-elected | No |
| `metalman server` | Session artifacts, callbacks, attestation, edge decision API | Yes | No |
| `metalman edge` | DHCP/TFTP on the provisioning LAN and HTTP proxying | No | Only when required for L2 |

Enabling `Site.spec.components.metalman.enabled` makes the operator deploy one
controller, two server replicas, a server Service, a PodDisruptionBudget, and a
shared capability-signing Secret. Endpoint resources determine edge placement.

## Endpoint Types

- `ManagedL2` creates a host-network edge on selected nodes. It can own DHCP,
  TFTP, and private HTTP on a directly attached provisioning network.
- `HTTP` creates replicated HTTP edge pods and a Service. Public endpoints must
  use HTTPS, either from a TLS Secret or external TLS termination.
- `ExternalL2` describes an edge outside the cluster. No edge workload is
  created. `kubectl unbounded site bootstrap-netboot` uses this mode.

WireGuard is an L3 transport and does not carry DHCP broadcasts. Keep a DHCP
edge or relay on the client LAN. TFTP, HTTP, and Redfish can use routed paths.

## Machine Example

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: node-01
  labels:
    unbounded-cloud.io/site: rack-a
spec:
  host:
    netboot:
      image: ghcr.io/azure/host-ubuntu2404:v1
      netbootImage: ghcr.io/azure/netboot:v1
      endpointRef: rack-a-l2
      transport: TFTP
      configurationSource: DHCP
      networkMode: DHCP
      dhcpLeases:
      - mac: aa:bb:cc:dd:ee:01
        ipv4: 10.0.0.11
        subnetMask: 255.255.255.0
        gateway: 10.0.0.1
        dns: [10.0.0.1]
      redfish:
        url: https://10.0.10.11
        username: admin
        passwordRef:
          name: bmc-node-01-pass
          namespace: default
          key: password
```

Supported boot combinations are:

| Transport | Configuration source | Network mode |
|-----------|----------------------|--------------|
| TFTP | DHCP | DHCP |
| HTTP | DHCP | DHCP |
| HTTP | Redfish | DHCP |
| HTTP | Redfish | Static |

TFTP with Redfish configuration and static networking without Redfish are
rejected by API validation.

## Durable Provisioning

Each HostReplace target gets an immutable `NetbootSession`. It snapshots the
Machine and operation identities, endpoint, boot settings, resolved OCI
digests, artifact allowlist, cluster settings, cloud-init data, and expiry.
Controller side effects wait until the session is ready.

Artifact and callback URLs contain an operation-scoped HMAC capability. HTTP,
TFTP, callbacks, and attestation resolve the exact session rather than trusting
source IP. Milestones are recorded on the exact session and operation target.
Artifacts are addressed by immutable digest and support HTTP ranges, allowing
an edge to resume a transfer through another server pod after disruption.

## Temporary First-Node Bootstrap

An administrator attached to the provisioning LAN can bootstrap the first Site
node without running Kubernetes controllers locally:

```bash
kubectl unbounded site bootstrap-netboot rack-a \
  --machine node-01 \
  --interface eno1 \
  --address 10.0.0.2
```

The command enables the in-cluster control and server planes, creates an
ephemeral ExternalL2 endpoint, port-forwards the local edge to a ready server
pod with reconnection, and stops when the designated Machine's Node is Ready.
The Machine endpoint reference is restored during cleanup.

Use repeatable `--routed-cidr` flags when the administrator host must also act
as a temporary unbounded-net gateway for BMC or provisioning subnets.

Run `metalman version` to print the binary version.
