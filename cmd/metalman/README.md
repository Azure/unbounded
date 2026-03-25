# metalman

Join bare metal nodes to a Kubernetes cluster using PXE and (optionally) Redfish.

Metalman commands are available through the `kubectl unbounded` plugin. A
dedicated `metalman` binary is also shipped as a container image for running
the PXE server inside a cluster.

## Usage

```bash
# Apply a pre-built Image manifest
kubectl apply -f https://github.com/TODO/releases/latest/download/ubuntu-24-04.yaml

# Create a Machine
kubectl apply -f - <<EOF
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: node-01
spec:
  pxe:
    imageRef:
      name: ubuntu-24-04
    dhcpLeases:
    - mac: "aa:bb:cc:dd:ee:01"
      ipv4: "10.0.0.11"
      subnetMask: "255.255.255.0"
      gateway: "10.0.0.1"
      dns: ["10.0.0.1"]
    redfish:
      url: https://10.0.10.11
      username: admin
      passwordRef:
        name: bmc-node-01-pass
        namespace: default
EOF

# Store the BMC's Redfish password
kubectl create secret generic bmc-pass --from-literal=password=example-password

# Reboot the node into PXE
kubectl unbounded reboot node-01 --reimage
```


## Concepts

### Controller

`kubectl unbounded serve-pxe` runs a single long-lived process that provides
everything needed to PXE-boot and manage bare metal hosts:

| Service | Default Port | Protocol | Purpose |
|---------|-------------|----------|---------|
| DHCP    | 67/udp      | DHCPv4   | Static leases derived from Machine NIC specs |
| TFTP    | 69/udp      | TFTP     | Initial bootloader delivery (e.g. shimx64.efi) |
| HTTP    | 8880/tcp    | HTTP     | Artifact serving, templated configs, attestation endpoints |
| Health  | 8081/tcp    | HTTP     | Liveness/readiness probes |

The controller also runs reconcilers for Image resources (downloading and
caching artifacts) and Machine resources with Redfish BMC specs (power
management, boot order configuration).

When deployed inside a cluster, the metalman container image uses the same
command:

```bash
metalman serve-pxe [flags]
```

#### DHCP Modes

The DHCP server operates in one of two modes depending on whether
`--dhcp-interface` is set:

- **Interface mode** (`--dhcp-interface=eth0`): Binds to a network interface
  and listens for broadcast DHCP traffic. Use this when the controller is
  directly attached to the provisioning network. This mode implies leader
  election — two instances on the same network might cause conflicts.

- **Relay mode** (no `--dhcp-interface`): Listens on a UDP port for unicast
  packets only. Use this when a DHCP relay agent forwards requests from a
  remote subnet. Leader election is not required because relay agents send
  unicast traffic to a specific address.

#### Security Model

A mostly-trusted network between the controller and the bare metal hosts is
assumed. Bootstrap tokens (Kubernetes ServiceAccount tokens) are issued to
nodes based on source IP — the controller looks up the Machine whose NIC
matches the requesting IP and issues a short-lived token for that node.

Bootstrap tokens are delivered using the standard TPM 2.0 credential encryption workflow.
The client's endorsement key (EK) public key is stored in `status.tpm.ekPublicKey` when first seen.
So it's possible to prove that the bootstrap token was delivered only to trusted hosts.

#### Pools

The `--pool` flag scopes a `serve-pxe` instance to a subset of Machines. The
value is matched against the `unbounded-kube.io/pool` label on Machine
resources:

```bash
# Manage only Machines labeled pool=rack-a
kubectl unbounded serve-pxe --pool=rack-a --dhcp-interface=eth0

# Manage only unlabeled Machines (the default)
kubectl unbounded serve-pxe --dhcp-interface=eth0
```

Each pool gets its own leader-election lease (`metalman-<pool>`), so
multiple pools can coexist on one cluster with independent HA. A `serve-pxe`
instance with no `--pool` manages Machines that do not have the pool label
at all.

### Images

An Image is a cluster-scoped custom resource that declares the set of files
served during PXE boot. Files can come from three sources:

- **HTTP** — downloaded from a URL, SHA256-verified, and cached locally on disk.
- **Template** — Go templates rendered per-node at serve time (e.g. grub.cfg
  with node-specific IPs and kernel parameters).
- **Static** — inline content embedded in the CR (plain text or base64).

See [`images/ubuntu24/build.py`](images/ubuntu24/build.py) for the build script
that produces a complete example Image CR booting Ubuntu 24.04 from upstream
netboot artifacts. Run `make images/ubuntu24` to generate `image.yaml`.

### Machine

A Machine is a cluster-scoped custom resource representing a single bare metal
host. At minimum it needs a NIC (MAC + static IP) and a PXE image reference:

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: node-01
spec:
  pxe:
    imageRef:
      name: ubuntu-24-04
    dhcpLeases:
    - mac: "aa:bb:cc:dd:ee:01"
      ipv4: "10.0.0.11"
      subnetMask: "255.255.255.0"
      gateway: "10.0.0.1"
```

This is enough for the DHCP server to issue a lease and for TFTP/HTTP to serve
boot artifacts. The node must be manually PXE-booted (or have PXE as its
default boot option).

#### BMC

Adding a `redfish` block enables remote power management. The controller will
manage boot order and execute reboot cycles without physical access:

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: node-01
spec:
  pxe:
    imageRef:
      name: ubuntu-24-04
    dhcpLeases:
    - mac: "aa:bb:cc:dd:ee:01"
      ipv4: "10.0.0.11"
      subnetMask: "255.255.255.0"
      gateway: "10.0.0.1"
    redfish:
      url: https://bmc-node-01.example.com
      username: admin
      passwordRef:
        name: bmc-node-01-pass
        namespace: default
```

The BMC password is read from a Secret in the same namespace (key: `password`).
On first connection, the controller captures the BMC's TLS certificate
fingerprint and pins it in `status.redfish.certFingerprint` for subsequent
requests.

To reimage a node with BMC access:

```bash
kubectl unbounded reboot --reimage node-01
```

This increments `spec.operations.reimageCounter` and `spec.operations.rebootCounter`. The
controller handles the rest — it configures the boot order to PXE, executes a
ForceOff/On power cycle, and clears the condition once the node is back up.
