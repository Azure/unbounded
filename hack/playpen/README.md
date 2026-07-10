# Playpen end-to-end test

`e2e.sh` builds the playpen binary, creates a local kind cluster, and boots
an Alpine netboot kernel through dnsmasq and VXLAN. It requires Docker, kind,
kubectl, KVM, dnsmasq, GRUB EFI netboot files, and passwordless sudo.

```sh
hack/playpen/e2e.sh
```

The client process runs its command in a dedicated network namespace. By
default it atomically claims a ready pod from the playpen deployment and uses
that pod's IP. Use a unique namespace and endpoint prefix for every concurrent
instance:

```sh
sudo bin/playpen client \
  --namespace pxe-1 \
  --endpoint-cidr 172.30.1.2/30 \
  --gateway-ip 172.30.1.1 \
  --pod-namespace unbounded-kube \
  --bridge-cidr 192.168.100.1/24 \
  --vxlan-vni 101 \
  -- dnsmasq --keep-in-foreground --interface=br-playpen
```

The endpoint and playpen pod must already have bidirectional L3 connectivity.
The endpoint CIDR is an underlay address; the bridge CIDR is the isolated PXE
network visible to the VM.

To connect through an unbounded-net gateway with a public IP, add `--site`,
`--gateway-pool`, a unique `--node-ip`, and a unique `--node-cidr`. The node IP
must fall within the selected Site's `nodeCidrs` and must be outside the
endpoint CIDR used for veth transport. An enabled `SiteGatewayPoolAssignment`
must connect that Site to an External GatewayPool. The node CIDR must be an
aligned per-node block from the Site's `podCidrAssignments` and must not
overlap the endpoint prefix, bridge, remote pod, or an existing Node pod CIDR:

```sh
sudo bin/playpen client \
  --namespace pxe-outside \
  --endpoint-cidr 172.30.1.2/30 \
  --gateway-ip 172.30.1.1 \
  --node-ip 172.31.1.2 \
  --node-cidr 10.250.1.0/24 \
  --site outside \
  --gateway-pool public \
  --pod-namespace unbounded-kube \
  -- dnsmasq --keep-in-foreground --interface=br-playpen
```

The client creates an unschedulable core Kubernetes Node containing the
unbounded-net Site label, WireGuard public key, internal IP, and pod CIDR. It
deletes that exact temporary Node when the command exits. The selected
kubeconfig identity therefore also needs permission to get Sites and
GatewayPools, list GatewayPools and SiteGatewayPoolAssignments, list Nodes,
create and delete Nodes, and update `nodes/status`. GatewayPool selectors must
not match the temporary Node's labels.

Scale the StatefulSet to create a pool, for example with
`PLAYPEN_REPLICAS=4 make playpen-manifests`. A client claims a pod by setting
the `playpen.unbounded-cloud.io/claimed-by` annotation and removes only its own
claim when it exits. The Kubernetes identity in the selected kubeconfig needs
permission to list, get, and update pods. Pass `--remote` to bypass pod claiming
and connect directly to an IP.

The StatefulSet derives each VM MAC from its stable namespaced pod name. This
keeps the MAC stable across pod restarts and gives every replica a distinct
Machine/DHCP identity. On a successful claim, `playpen client` writes these
shell-compatible values to stderr and exports them to the child command:

```text
PLAYPEN_CLAIMED_POD=unbounded-kube/playpen-0
PLAYPEN_REMOTE_IP=10.244.0.8
PLAYPEN_VM_MAC=02:xx:xx:xx:xx:xx
```

Use `PLAYPEN_VM_MAC` as the claimed Machine's `spec.pxe.dhcpLeases[].mac`.
The `playpen server --mac` flag still overrides derived or random MACs,
but must not be used as one shared value in a multi-replica claim pool. Custom
pool manifests must use the same `--mac-identity=<namespace>/<pod-name>` scheme
as the supplied StatefulSet; direct `--remote` clients do not report a VM MAC.

Each StatefulSet replica has its own persistent writable raw guest disk. The
network device remains first in the firmware boot order and the disk is second,
so metalman can PXE install and then stop serving PXE to allow disk boot. Set
`PLAYPEN_DISK_SIZE` for the guest-visible disk size and
`PLAYPEN_DISK_STORAGE` for the per-replica PVC request.

Playpen also gives each guest a TPM 2.0 device backed by `swtpm`. Its state is
stored with the guest disk so the TPM identity survives pod restarts and the
installed agent can attest through `/dev/tpmrm0`.

Each replica exposes an HTTPS Redfish BMC on port 8443. The generated TLS key
pair is stored on the replica's PVC, so metalman's captured certificate
fingerprint remains valid across pod restarts. The default connection details
for replica 0 are:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: playpen-bmc
  namespace: unbounded-kube
stringData:
  password: playpen
---
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: playpen-0
spec:
  pxe:
    image: ghcr.io/azure/host-ubuntu2404:v1
    architecture: amd64
    dhcpLeases:
      - mac: "<PLAYPEN_VM_MAC from the claim>"
        ipv4: "<the PXE lease address>"
        subnetMask: "<the PXE subnet mask>"
    redfish:
      url: https://playpen-0.playpen.unbounded-kube.svc:8443
      username: admin
      deviceID: "1"
      passwordRef:
        name: playpen-bmc
        namespace: unbounded-kube
        key: password
```

Set `PLAYPEN_BMC_PORT`, `PLAYPEN_BMC_USERNAME`, `PLAYPEN_BMC_PASSWORD`, and
`PLAYPEN_BMC_DEVICE_ID` when rendering manifests to change these values. This
BMC implements the metalman operations needed for repave and reboot: power
state, `On`/`ForceOff`, and PXE/HDD boot overrides including `Once`.
