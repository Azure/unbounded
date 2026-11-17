---
title: "Bare Metal (PXE)"
weight: 3
description: "Provision bare metal servers with metalman over PXE, UEFI HTTP boot, or virtual media, from Machine declaration to joined Node."
---

## Overview

Bare metal provisioning is the path for a physical server that has no operating system you control. The host has a network interface, firmware that can boot from the network, and usually a baseboard management controller (BMC). By the end of the process, the host has an operating system image written to disk, an Unbounded agent installed, a bootstrap credential that only that hardware could decrypt, and a `Node` object in your cluster.

The `metalman` controller performs the provisioning. It runs inside the cluster on the site's provisioning network and combines the DHCP, TFTP, and HTTP artifact servers with a health endpoint in one process. It also runs a Redfish client and reconcilers that pull and cache OCI images, pin the BMC's TLS certificate, and advance `MachineOperation` resources such as repaves and power changes.

The mental model has three parts. A `Machine` resource declares one physical host: its network lease, its image, its optional BMC, and its Kubernetes settings. `metalman` matches the host to that declaration by MAC address and by the source address of the host's requests. A `MachineOperation` records one attempt to change the host, and its target status is where you watch the install progress.

![Bare metal PXE boot flow showing firmware, DHCP, TFTP, HTTP, the installer, and first boot](../../img/bare-metal-pxe-boot.svg)

This guide walks an operator through the whole path. It explains which subsystem owns which fact, how to plan the provisioning network and DHCP, what the two OCI images contain, how to write the `Machine`, how the boot sequence advances through its phases, how virtual media differs from PXE, what TPM attestation proves, how to verify a completed install, and what to check when a host stalls.

## Inventory and Metalman Responsibilities

Unbounded ships a separate inventory subsystem, and it is easy to assume it drives provisioning. It does not. Inventory is an observation system: `inventory-agent` runs as root on a host, reads sysfs, procfs, DMI, IPMI, and optional utilities, stores the result in a local SQLite database, and publishes batches over gRPC to `inventory-aggregator`. The aggregator persists devices and neighbors to PostgreSQL. `inventory-viewer` serves a read-only web UI and JSON endpoints for devices and neighbors. `inventory-inspector` runs as a CronJob and reports conflicts, currently duplicate device serial numbers and duplicate LLDP upstream ports.

That data is useful before you write a `Machine`. You need exact permanent MAC addresses for the ethernet declarations and the DHCP leases, and you need to know which physical port is cabled to which switch. The agent records chassis identity, BMC data, CPUs, memory, disks, NICs, GPUs, LLDP neighbors, and NVIDIA link peers. Devices are upserted by serial number and neighbors by host identifier plus local interface, so repeated runs refresh a record instead of duplicating it.

The boundary is strict. `metalman` does not read the inventory database, and adding a device to inventory does not create a `Machine`. There is no import controller that turns a discovered NIC into a lease. You still declare the host in Kubernetes, and you still choose which MAC and address the host will use. The practical workflow is to run or consult inventory to gather facts, then write the `Machine` by hand or with your own automation.

The same boundary applies after provisioning. Inventory can tell you where a host is cabled and what hardware it contains, but it does not report whether a repave succeeded. Boot and power progress belongs to the `MachineOperation`, and node health belongs to Kubernetes. When a host does not appear, check the operation and the `metalman` logs first, not the inventory database.

## Prerequisites

You need a Kubernetes cluster that serves the `unbounded-cloud.io/v1alpha3` API and has the Unbounded operator installed. The `kubectl unbounded` plugin must be able to reach the cluster. If the API discovery check fails, run the operator install first; the PXE guide assumes the operator is already present.

The target server must have UEFI firmware that can network boot. The netboot image ships EFI artifacts: `shimx64.efi` and `grubx64.efi` for amd64, or `shimaa64.efi` and `grubaa64.efi` for arm64. Legacy BIOS PXE is not part of this path. The firmware also needs a working PXE or UEFI HTTP boot entry, and you need a way to select it once if the `Machine` has no Redfish block.

For an unattended repave you need a BMC that speaks Redfish, credentials that `metalman` can read from a Secret, and HTTPS reachability from the controller to the BMC. Without Redfish you can still provision, but a person has to put the host into network boot and power it on. For TPM attestation you need TPM 2.0 enabled in firmware and exposed to Linux through the kernel resource manager at `/dev/tpmrm0`.

Plan the network before you write any YAML. The controller must reach the host's provisioning segment, the host must reach the controller's HTTP server, and the BMC must reach the controller if you use virtual media. The following ports and paths are the ones that matter.

| Flow | Protocol | Port | Purpose |
| --- | --- | --- | --- |
| Controller to Kubernetes API | HTTPS | 6443 | Watches `Machine` and `MachineOperation`, mints tokens, approves CSRs |
| Controller to BMC | HTTPS | 443 | Redfish power control, boot override, virtual media |
| Host to controller DHCP | UDP | 67 | Lease offer and boot file or boot URL |
| Host to controller TFTP | UDP | 69 | Initial bootloader, and ISOs for rack-manager virtual media |
| Host to controller HTTP | TCP | 8880 | Kernels, initrds, disk image, templates, callbacks, attestation |
| Probe to controller health | TCP | 8081 | Liveness and readiness checks |

The provisioning network should be mostly trusted. `metalman` identifies a machine by the source address of a request, so a host that can source the lease address at the right moment can ask for a bootstrap token. TPM attestation limits where that token can be used, but it does not authenticate the network path. Keep the provisioning segment separate from untrusted networks, and do not place an HTTP proxy in front of `metalman`. The controller ignores `X-Forwarded-For`, `X-Real-IP`, and `Forwarded` and uses the direct TCP peer address.

## Deploy Metalman

`metalman` is a per-site component. You do not install it with a separate operator flag. Install the Unbounded operator once, then create or update a `Site` with the metalman component enabled. The operator creates the per-site Deployment, the shared ServiceAccounts, the RBAC, and the admission policy that restricts bootstrap tokens.

Install the operator if it is not already present:

```bash
kubectl unbounded install
```

Then initialize a site with metalman enabled. The CIDR values below are examples; use ranges that do not overlap the cluster site or any other site:

```bash
kubectl unbounded site init \
  --name rack-a \
  --cluster-node-cidr 10.0.0.0/16 \
  --cluster-pod-cidr 10.244.0.0/16 \
  --node-cidr 10.10.0.0/16 \
  --pod-cidr 10.245.0.0/16 \
  --enable-metalman
```

`site init` writes `components.metalman.enabled: true` on the named site. It does not turn on automatic DHCP interface detection, so the default deployment starts in relay mode. If the controller is attached directly to the provisioning segment, enable auto-detection:

```bash
kubectl patch site rack-a --type=merge \
  -p '{"spec":{"components":{"metalman":{"enabled":true,"dhcpAutoInterface":true}}}}'
```

If you need a named interface rather than auto-detection, leave `dhcpAutoInterface` off and append `--dhcp-interface=<name>` to the `metalman` container through the `unbounded-component-overrides` ConfigMap. The Site component exposes only the auto-detection switch, but workload overrides can append any supported flag. See [Workload overrides]({{< relref "reference/workload-overrides" >}}) for the document format and for the warning that a mistyped flag becomes a crash loop.

### Installation Components

The operator creates the namespace `unbounded-system` if it does not exist. The metalman support set includes the `metalman-controller` ServiceAccount used by the Deployment, the `metalman-bootstrap` ServiceAccount whose tokens are handed to booting hosts, the RBAC that lets the controller manage `Machine` and `MachineOperation` resources, and the `ValidatingAdmissionPolicy` plus binding that limit what a bootstrap token can change.

The per-site Deployment is named `metalman-controller-<site>`. It uses host networking because DHCP, TFTP, and HTTP bind host ports. It is scheduled to nodes carrying the site label through a node affinity that accepts the canonical site label and its deprecated predecessor. The rollout uses `maxSurge: 0` and `maxUnavailable: 1` because a surge pod cannot start while the old pod holds the same host ports on the same node.

The Deployment runs the image entrypoint `metalman` with the arguments `serve-pxe` and `--site=<site>`. It also sets `POD_NAMESPACE` from the Downward API so the leader-election lease and its namespaced RBAC stay in the same namespace as the Deployment. When the operator knows the API server endpoint, it sets `METALMAN_APISERVER_URL` so the kubeconfig served to provisioning hosts matches what machina writes. This matters on managed control planes such as AKS that do not publish `kube-public/cluster-info`; the controller can still source the CA from its in-cluster service-account mount.

### Metalman Arguments

The operator passes very few arguments. The rest are available when you run `metalman serve-pxe` directly in a lab, or when you append them through a workload override. The defaults below come from the command definition.

| Argument | Default | Effect |
| --- | --- | --- |
| `--site` | empty | Selects `Machine` resources by the `unbounded-cloud.io/site` label |
| `--bind-address` | `0.0.0.0` | Address the HTTP and TFTP servers bind, and the address used to resolve the advertised server IP |
| `--http-port` | `8880` | HTTP artifact, callback, and attestation port |
| `--health-port` | `8081` | Health and readiness probe port |
| `--dhcp-interface` | empty | Binds broadcast DHCP to one named interface |
| `--dhcp-auto-interface` | `false` | Detects the DHCP interface from the resolved server IP; mutually exclusive with `--dhcp-interface` |
| `--dhcp-port` | `67` | UDP port for DHCP |
| `--serve-url` | resolved | External URL of this instance, used in boot configuration and media URLs |
| `--cache-dir` | `~/.unbounded/metalman/cache` | Local cache for unpacked OCI images |
| `--media-tftp-server` | server IP | Advertised IPv4 address for rack-manager virtual-media TFTP pulls |
| `--default-netboot-image` | build-stamped | Netboot image used when a Machine omits `netbootImage` |
| `--default-netboot-pull-secret` | empty | Namespaced Secret reference (`namespace/name`) for the default netboot image |
| `--operation-max-concurrent-machines` | `10` | Targets advanced concurrently inside one `MachineOperation` |
| `--operation-max-attempts` | `3` | Redfish action attempts per target machine |
| `--operation-poll-interval` | `5s` | Poll interval for in-progress operations |
| `--leader-elect-lease-duration` | `15s` | How long a candidate waits before trying to acquire leadership |
| `--leader-elect-renew-deadline` | `10s` | How long the leader retries refreshing leadership |
| `--leader-elect-retry-period` | `2s` | Delay between leader-election retries |
| `--experimental-container-isolation` | `false` | Opts every agent configuration into the experimental container isolation mode |

When `--bind-address` is left at `0.0.0.0` and `--serve-url` is empty, the controller resolves the outbound address and advertises `http://<address>:8880`. When `--dhcp-interface` is set, the DHCP server advertises that interface's IPv4 address as the TFTP server and DHCP server identifier, while the HTTP serve URL remains the resolved bind address. On a multi-homed controller, set `--serve-url` explicitly to an address that both hosts and BMCs can reach.

### Container Isolation

The isolation mode decides where the node stack runs on the host: contained in a systemd-nspawn machine, directly on the host, or in a privileged container. `nspawn` is the default for a new host. `container` is experimental, and both the controller and the agent must opt in.

For PXE, the opt-in is `--experimental-container-isolation` on `metalman serve-pxe`. Without it, a `Machine` that declares `spec.isolation: container` gets no agent configuration and no installer boot; the grub template still chainloads the installed disk when no install is pending. Append the flag through a workload override on the `metalman` container. The machine-ops controller has its own flag of the same name, but the PXE path executes `HostReplace` in `metalman`, not in that controller.

### Leader Election

Leader election is always enabled for the controller manager. Each site gets its own lease: `metalman-<site>` for a scoped instance, or `metalman` for an unscoped instance. Replicas can be increased with `spec.components.metalman.replicas`, which defaults to one. All replicas contend for the same site lease, so only one advances `MachineOperation` targets at a time.

The servers behave slightly differently from the reconcilers. The HTTP and TFTP servers do not require leadership, so they start on every replica. The DHCP server requires leadership only when it binds an interface, because broadcast replies must come from one instance. In relay mode it accepts unicast packets from a relay and does not need leadership. With multiple replicas, put them on different nodes; host networking means two pods on the same node cannot bind the same ports.

## Network and DHCP Planning

The provisioning network is the part of the design most likely to cause a failed install. Decide three things before you write a `Machine`: where the controller sits relative to the host, how DHCP requests reach it, and which address the host will use for HTTP and attestation.

### Choosing a DHCP Mode

`metalman` has three DHCP modes, and the mode must match the path between the controller and the host.

| Mode | Selection | When to use it |
| --- | --- | --- |
| Interface | `--dhcp-interface=eth0` | The controller is directly attached to the same Layer 2 segment as the hosts |
| Auto-interface | `--dhcp-auto-interface` | The bind address already identifies the correct interface, and you do not want to name it |
| Relay | neither flag | A DHCP relay forwards requests from a routed segment |

The two interface flags are mutually exclusive. Auto-interface mode resolves the interface that holds the server IP, which is the explicit bind address if you set one, otherwise the outbound address. Interface mode uses the named interface's IPv4 address for DHCP option 54 and the TFTP server address.

Relay mode is the default and the safest choice when you are unsure. It answers only packets that carry a nonzero relay address (`giaddr`), so a direct client packet on the local segment is ignored. That prevents the controller from competing with a site DHCP server on a segment it does not own. If the host and the controller are on the same segment, use interface mode instead; relay mode will not answer a direct request.

In every mode, the lease comes from the `Machine`, not from a dynamic pool. There is no address range to configure. The server returns the configured IPv4 address, subnet mask, gateway, and DNS servers, and it sets a lease time of 86,400 seconds. It answers a discover or request from any MAC that has a lease, so the host can keep the same address across the installer boot and the installed system.

### Lease Semantics

A `dhcpLeases` entry is the identity of one NIC on one host. The fields are `mac`, `ipv4`, `subnetMask`, `gateway`, `dns`, and `holdLease`. The MAC is matched case-insensitively and indexed in lowercase. The IPv4 address is also indexed, and it is the address used to find the `Machine` when the host calls `/attest`, `/pxe/disable`, or the cloud-init log endpoints.

For PXE, the lease also supplies the boot file name. The server reads the netboot image metadata, sets the TFTP server name to the controller address, and sets the boot file name to `dhcpBootImageName`. For UEFI HTTP boot, it sets only a boot URL, and only for clients whose vendor class starts with `HTTPClient`. For virtual media, it sets the next-server field to `0.0.0.0` and no boot file, because the boot trigger is the mounted ISO rather than DHCP.

If a host has multiple NICs, put the NIC that reaches `metalman` first in `dhcpLeases`. The first entry is the provisioning identity for HTTP boot static configuration and for virtual media. The host's requests must arrive from one of its lease addresses, because the controller matches the direct peer address against the lease index. For virtual media, the installer callbacks are pinned to the first lease's address.

`holdLease: true` handles a conflict with a site DHCP server. Without it, the installed OS gets only the static address written by the installer, and the site server may hand that address to another host. With it, the installed host runs a DHCPv4 client on the provisioning NIC, identifies with the MAC, and keeps the reservation. Routes, DNS, NTP, hostname, and domain values from DHCP are ignored; the static address, gateway, and DNS remain authoritative. `metalman` answers only boot firmware for a held lease, so the installed OS talks to the site server. Do not set `holdLease` when no site DHCP server exists, and do not set it on the first lease when `spec.host.netboot.network` is declared.

### Planning HTTP and Virtual Media

For PXE and HTTP boot, the host needs to reach the HTTP server at the serve URL. For virtual media, the BMC needs to reach it, because the BMC downloads the ISO. Those are not always the same network path. A controller that can serve a host on the provisioning segment may not be reachable from a BMC on the management network, and the install will stall at media mounting rather than at the boot loader.

Decide early whether the site uses DHCP at all. With Redfish and UEFI HTTP boot, `metalman` can write a static IPv4 configuration to the host's EthernetInterface, disable DHCPv4 on that interface, and set the HTTP boot override. The host then boots without any DHCP server on the provisioning network. If you want that path, make sure the BMC exposes a writable EthernetInterface matching the first lease MAC; otherwise the controller falls back to BIOS attributes, and some firmware ignores them.

## Images

A repave uses two OCI images. The machine image is the operating system that ends up on disk. The netboot image is the reusable boot environment that runs the installer. They are separate so that one boot environment can install many machine images, and so that updating a machine image does not require rebuilding the bootloader and kernel for every host.

`metalman` pulls both images from a registry and unpacks them into the cache directory under `oci/<digest>/<architecture>/disk/...`. The first boot after an image change is slower because the pull and unpack happen then; later boots stream from the local cache. Private registries are supported through `pullSecretRef` for the machine image and `netbootPullSecretRef` for the netboot image. The netboot pull secret is ignored when no netboot image is named, because the controller uses its own configured secret for the default image.

### Machine Image

The machine image contains exactly one required artifact: `/disk/disk.img.gz`, a gzip-compressed raw disk image. It is built `FROM scratch` and follows the containerDisk convention, so artifacts live under `/disk/` in the image layers. The installer downloads the file over HTTP, decompresses it, and writes it to the target disk with `dd`.

The repository ships definitions for Ubuntu 24.04 and Azure Linux 3 hosts. The Ubuntu definition converts the matching Ubuntu cloud image from qcow2 to raw, compresses it with `pigz`, and places it at `/disk/disk.img.gz`. The Azure Linux definitions follow the same layout. The host image determines which network configuration the installer persists: images with `/etc/netplan` get a netplan file, and images with `/etc/systemd/network` get systemd-networkd `.netdev` and `.network` files.

```bash
podman build -f images/host-ubuntu2404/Containerfile \
  -t ghcr.io/<owner>/host-ubuntu2404:<tag> .
podman push ghcr.io/<owner>/host-ubuntu2404:<tag>
```

Replace `<owner>` and `<tag>` with values your cluster can pull. Build from the repository root, because the Containerfile copies `LICENSE` and `NOTICE` from the build context. The `images/host-azurelinux3` and `images/host-azurelinux3-nvidia` definitions use the same pattern.

### Netboot Image

The netboot image is the boot environment. It contains the bootloader, kernel, initrd, installer overlay, templates, and a `metadata.yaml`. Files with a `.tmpl` suffix are Go templates rendered per machine at serve time; all other files are served verbatim. That is how the GRUB configuration, cloud-init metadata, vendor data, and network configuration pick up the right machine name, addresses, URLs, and agent configuration.

The shipped amd64 netboot image has this layout:

| Path | Purpose |
| --- | --- |
| `/disk/vmlinuz` | Installer kernel |
| `/disk/initrd` | Ubuntu netboot initrd |
| `/disk/init.cpio` | Local installer overlay with storage, network, bond, and VLAN modules |
| `/disk/shimx64.efi` | Signed first-stage bootloader |
| `/disk/grubx64.efi` | GRUB second stage |
| `/disk/bootx64.efi` | Removable-media copy used by UEFI HTTP boot |
| `/disk/metadata.yaml` | Image-level boot settings |
| `/disk/grub/grub.cfg.tmpl` | Rendered GRUB menu and kernel command line |
| `/disk/cloud-init/meta-data.tmpl` | Instance ID and hostname |
| `/disk/cloud-init/vendor-data.tmpl` | Agent configuration, install script, and first-boot setup |
| `/disk/cloud-init/network-config.tmpl` | Single-NIC lease network configuration |

The arm64 build places `shimaa64.efi` and `grubaa64.efi` instead of the amd64 loader files. The `init.cpio` overlay carries the modules for virtio, AHCI, NVMe, SCSI, ATA, XFS, ext4, bonding, VLAN, and the common ethernet drivers, plus the `ip` binary the installer uses when the initrd's own copy lacks bond or VLAN support. It also performs a PCI modalias pass, so a RAID controller that is missing from the fixed module list is still probed.

The `metadata.yaml` file supplies settings that the controller needs before it renders any template.

| Key | Required | Purpose |
| --- | --- | --- |
| `dhcpBootImageName` | For PXE | File name offered as DHCP option 67, resolved under `/disk/` |
| `httpBootPath` | For HTTP boot | Path appended to the serve URL; falls back to `dhcpBootImageName` when empty |
| `mediaFormat` | For virtual media | Must be `unbounded-uefi-v1`; the controller refuses to build media otherwise |
| `localInstaller` | For `network` | Must be `unbounded-local-installer-v1`; declares that the ISO installer can bring up bonds and VLANs |

The repository's amd64 image sets `dhcpBootImageName: shimx64.efi`, `httpBootPath: bootx64.efi`, and both capability keys. The HTTP path differs from the DHCP name because firmware HTTP boot loads the removable-media path directly, while PXE loads the signed shim over TFTP.

### Template Data

Templates are rendered from a fixed map. A template that names a missing key renders it as an empty value rather than failing, so an older netboot image can run against a newer controller and the reverse. The fields in this release are:

| Name | Meaning |
| --- | --- |
| `.Machine` | The `Machine` being provisioned |
| `.BootLease` | The lease selected for this request, or nil |
| `.DHCPHoldLease` | True when the selected lease sets `holdLease` |
| `.ApiserverURL` | API server URL from cluster info or the operator-provided endpoint |
| `.ServeURL` | Base URL of the artifact server |
| `.AgentConfigJSON` | Rendered agent configuration as JSON |
| `.InstallScript` | Agent install script |
| `.InstallEnv` | Environment assignments for the install script |
| `.InstallRequested` | True when an install is pending |
| `.VirtualMedia` | True when serving a bootstrap ISO |
| `.ArtifactRoot` | URL prefix for kernel and initrd artifacts |
| `.MediaCallbackURL` | Callback URL for the virtual media installer |
| `.HostNetplan` | Declared host network as a netplan version 2 document, or empty |

The template functions are `indent`, `ipAddresses`, `join`, `subnetPrefix`, and `yamlQuote`. The Kubernetes version and cluster DNS are not top-level template fields. They reach the host through `.AgentConfigJSON`. A custom template that needs them should read the agent configuration or a value it writes into the `Machine`, not a template variable that is not present.

### Boot Pipeline and Image Pipeline

The boot pipeline uses the netboot image twice. Over PXE and HTTP, the firmware downloads the bootloader and GRUB fetches the rendered `grub.cfg`. Over virtual media, `metalman` builds a small operation-owned ISO that contains the same installer and a live GRUB handoff, then mounts it through Redfish. In both cases the installer fetches the machine image from the HTTP server and writes it to disk.

The image pipeline is independent of any one host. Build the machine image when the OS content changes. Build the netboot image when the installer, kernel, templates, or metadata change. Tag both, push them to a registry the cluster can reach, and reference them from `Machine` resources. For virtual media, pin both by digest, because the controller refuses to build an ISO from a moving tag.

```bash
podman build -f images/netboot/Containerfile \
  -t ghcr.io/<owner>/netboot:<tag> .
podman push ghcr.io/<owner>/netboot:<tag>
```

For an arm64 host whose NIC driver the arm64 Ubuntu kernel lacks, build the Azure Linux installer kernel variant. Secure Boot must be off for that variant because the Ubuntu shim does not trust the Azure Linux kernel.

```bash
podman build --platform linux/arm64 \
  --build-arg INSTALLER_KERNEL=azurelinux \
  -f images/netboot/Containerfile \
  -t ghcr.io/<owner>/netboot-azurelinux:<tag> .
```

![Netboot image layout showing the machine image, netboot artifacts, metadata, templates, and the controller cache](../../img/machina-netboot-layout.svg)

## Machine and MachineClass Resources

A `Machine` is the unit of declaration for one physical host. It is cluster scoped, has the short name `mach`, and prints the SSH host, phase, Kubernetes version, and age as columns. For a PXE host, the phase column is normally empty because `metalman` does not drive it; progress lives on the `MachineOperation`. The agent still writes its own conditions on the `Machine` after the host joins.

### Netboot Fields

The netboot configuration belongs under `spec.host.netboot`. The older top-level `spec.pxe` shape is still served for existing machines, and both resolve through the same compatibility accessor, but new machines should use `spec.host.netboot`. The API rejects a `Machine` that combines host ownership with the legacy `spec.pxe`, `spec.provider`, or `spec.providerID` fields.

| Field | Type | Default | Purpose |
| --- | --- | --- | --- |
| `image` | string | required | Machine image containing `/disk/disk.img.gz` |
| `pullSecretRef` | namespaced Secret reference | unset | Registry credentials for the machine image |
| `architecture` | `amd64` or `arm64` | `amd64` | Architecture of the boot artifacts and machine image |
| `netbootImage` | string | release default | Netboot environment override |
| `netbootPullSecretRef` | namespaced Secret reference | unset | Registry credentials for the netboot image |
| `bootProtocol` | `PXE`, `HTTP`, or `VirtualMedia` | `PXE` | How to trigger the network boot |
| `dhcpLeases` | list | empty | Static leases and, for HTTP, the boot client network settings |
| `targetDisk` | string | automatic | Block device the installer writes |
| `redfish` | object | unset | BMC URL, username, device ID, and password Secret |
| `cloudInit` | object | unset | ConfigMap reference for first-boot user data |
| `cloudInitTimeout` | duration | `20m` | Budget from installer handoff to first-boot completion |
| `network` | object | unset | Ethernets, bonds, VLANs, routes, and DNS for static host networking |

A `dhcpLeases` entry has four required values and two optional ones. The required values are `mac`, `ipv4`, `subnetMask`, and `gateway`; `dns` and `holdLease` are optional. The API validates that the MAC is a lowercase colon-separated unicast address when it indexes the lease, and the host network validator applies the same rule to ethernet declarations.

| Lease field | Meaning |
| --- | --- |
| `mac` | Permanent MAC address of the provisioning NIC |
| `ipv4` | Address the host uses during the installer and first boot |
| `subnetMask` | Contiguous IPv4 netmask for that address |
| `gateway` | Default gateway for the provisioning network |
| `dns` | Optional IPv4 nameservers passed to the installer and written into the OS |
| `holdLease` | Runs a DHCP client in the installed OS to hold the reservation |

The `redfish` block is optional for PXE and HTTP boot, but it is required for virtual media and for any operation that changes power. Its `url` is the Redfish endpoint and may include a per-system path prefix such as `https://manager:8080/7`; it must not include `/redfish/v1`. The `deviceID` defaults to `1`. The password is read from the Secret named by `passwordRef`, whose `key` defaults to `password`.

| Redfish field | Required | Default | Meaning |
| --- | --- | --- | --- |
| `url` | yes | none | BMC or rack-manager endpoint |
| `username` | yes | none | Redfish username |
| `deviceID` | no | `1` | Redfish system identifier |
| `passwordRef.name` | yes | none | Secret name |
| `passwordRef.namespace` | yes | none | Secret namespace |
| `passwordRef.key` | no | `password` | Key inside the Secret |

The `kubernetes` block controls the node identity and kubelet settings. `version` falls back to the cluster's Kubernetes version when omitted and is normalized with a leading `v`. `nodeRef.name` pins the node name; when it is unset, the `Machine` name is used. `nodeLabels` and `registerWithTaints` are passed to kubelet. `bootstrapTokenRef` is used by the SSH path, not by TPM attestation.

The `agent` block controls the host-side agent that runs after first boot. `image` sets the nspawn image, `version` pins the agent release, `url` or `baseURL` overrides the download source, and `downloads` overrides the sources for kubelet, containerd, runc, CNI plugins, crictl, and CoreDNS. For a normal PXE install you can leave the `agent` block empty and let the release defaults apply.

A minimal but complete PXE machine looks like this. The site label must match the `Site` name, because the operator-deployed controller is scoped with `--site=<site>`.

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
      image: ghcr.io/example/host-ubuntu2404:v1
      architecture: amd64
      bootProtocol: PXE
      targetDisk: /dev/disk/by-id/nvme-Example_OS_Disk
      dhcpLeases:
        - mac: "aa:bb:cc:dd:ee:01"
          ipv4: "10.10.0.11"
          subnetMask: "255.255.255.0"
          gateway: "10.10.0.1"
          dns: ["10.10.0.53"]
      redfish:
        url: https://bmc-node-01.example.com
        username: admin
        passwordRef:
          name: bmc-node-01
          namespace: unbounded-system
          key: password
  kubernetes:
    version: v1.34.0
    nodeLabels:
      hardware.example.com/class: batch
```

Create the BMC password Secret before the `Machine` needs it. The key must match `passwordRef.key`:

```bash
kubectl -n unbounded-system create secret generic bmc-node-01 \
  --from-literal=password='<bmc-password>'
```

If you omit `targetDisk`, the installer selects the largest whole block device it can find and logs a warning. On a server with a small boot device and a large data disk, that heuristic picks the wrong disk. Set `targetDisk` whenever the host has more than one candidate, using a stable path such as `/dev/disk/by-id/...` rather than a kernel name that can change.

The BMC password Secret is read at operation time, not at admission. If you rotate the password, update the Secret; the next Redfish call uses the new value. The BMC's TLS certificate is different: it is pinned on first connection and stored in `status.redfish.certFingerprint`. A certificate rotation requires clearing that status field before the controller will trust the new certificate.

### MachineClass

`MachineClass` is a provider-neutral template for allocation-driven capacity. It names a `Site`, a provider adapter, a provider configuration ConfigMap, and a pinned `MachineConfiguration` version. A consumer asks for capacity, the allocation controller selects a matching offering from the class, and the provider launches the host. It is cluster scoped, has the short name `mcls`, and prints the site, provider, readiness, and age.

A `MachineClass` does not have netboot fields and is not read by `metalman`. The only provider registered in this repository's machine-ops controller is `AzureVM`, which creates and replaces Azure virtual machines. There is no bare-metal provider that turns a class offering into a `Machine` with a lease. For a physical host, declare the `Machine` directly, as shown above. Use `MachineClass` for the cloud API path described in [Cloud API provisioning]({{< relref "guides/cloud-api" >}}), not for PXE.

The distinction matters when you read examples that mix the two concepts. A `Machine` created for an allocation carries `spec.allocationRef`, which is immutable, and may be fenced. A PXE `Machine` normally has neither. Its identity comes from the lease and the BMC, not from a provider resource identifier.

### Legacy `spec.pxe`

Existing machines may still carry the top-level `spec.pxe` object. It has the same shape as `spec.host.netboot` and resolves through the same accessor, so `metalman` continues to serve it. Do not add new fields to it. When you edit an old machine, move the block to `spec.host.netboot` in one change. The API rejects a machine that has both a netboot host owner and the legacy fields, so remove `spec.pxe`, `spec.provider`, and `spec.providerID` when you migrate.

## Bonds and VLANs

The single-NIC model is deliberately simple: one lease, one MAC, one static address, and a netplan file keyed by that MAC. Some hosts cannot use that model. A blade may have only an LACP bond as its uplink, or the provisioning address may live on a tagged VLAN. For those hosts, declare the topology in `spec.host.netboot.network`.

The declared network is authoritative. The installer brings it up before it contacts `metalman`, uses it during installation, and persists the same topology into the installed system as netplan or systemd-networkd configuration. It is available only with `bootProtocol: VirtualMedia`, because firmware PXE cannot bring up a bond before it downloads the bootloader. If the link only passes traffic after LACP negotiates, virtual media is the only path that works.

The cross-references are validated when `metalman` builds the bootstrap ISO, not when the `Machine` is admitted. A bond member must be a declared ethernet. A VLAN link must be a declared ethernet or bond, and not a bond member. A bond member must not carry addresses, a gateway, or routes. Only one interface may set a default gateway. A VLAN's MTU must not exceed the MTU of its link, and a bond member that sets an MTU must match the bond's MTU. Addressing is static IPv4 only. Prefix lengths must be between 1 and 30, addresses must be global unicast and not the network or broadcast address, and a next hop must be inside one of the interface's subnets.

The first `dhcpLeases` entry remains the provisioning identity even when `network` is declared. Its IPv4 address must be the first address on exactly one declared interface, and its subnet mask must match that interface's prefix. Its gateway must match the gateway declared on that same interface. Its MAC must be one of the ethernets underlying that interface, including through a bond. This is what lets `metalman` authorize installer callbacks by source IP. `holdLease` is rejected in this mode, because the declared network is authoritative.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: server-01
  labels:
    unbounded-cloud.io/site: rack-a
spec:
  host:
    netboot:
      image: ghcr.io/example/host-ubuntu2404@sha256:0000000000000000000000000000000000000000000000000000000000000000
      netbootImage: ghcr.io/example/netboot@sha256:0000000000000000000000000000000000000000000000000000000000000000
      architecture: amd64
      bootProtocol: VirtualMedia
      targetDisk: /dev/disk/by-id/nvme-Example_OS_Disk
      dhcpLeases:
        - ipv4: "192.0.2.30"
          mac: "02:00:00:00:00:01"
          subnetMask: "255.255.255.0"
          gateway: "192.0.2.1"
          dns: ["192.0.2.53", "192.0.2.54"]
      network:
        ethernets:
          - name: enp49s0f0np0
            macAddress: "02:00:00:00:00:01"
          - name: enp49s0f1np1
            macAddress: "02:00:00:00:00:02"
        bonds:
          - name: bond0
            interfaces: [enp49s0f0np0, enp49s0f1np1]
            mode: 802.3ad
            miiMonitorInterval: 100
            transmitHashPolicy: layer3+4
            mtu: 1500
            addresses: ["192.0.2.30/24"]
            gateway: "192.0.2.1"
        vlans:
          - name: bond0.64
            id: 64
            link: bond0
            addresses: ["198.51.100.30/24"]
            routes:
              - to: "203.0.113.0/24"
                via: "198.51.100.1"
        dns: ["192.0.2.53", "192.0.2.54"]
      redfish:
        url: https://bmc-01.example.com
        username: admin
        passwordRef:
          name: bmc-passwords
          namespace: unbounded-system
          key: bmc-01
```

The digest values above are placeholders. Virtual media requires both `image` and `netbootImage` to be digest references of the form `repository@sha256:<64 hex characters>`. A tag is rejected before any media is built.

The interfaces are matched by permanent MAC, not by kernel name. That is what lets the installer find the right port even when the installer kernel names it differently from the installed OS. The `name` field is the name persisted on the installed host. Bond modes are `802.3ad`, `active-backup`, and `balance-xor`. `transmitHashPolicy` applies only to `802.3ad` and `balance-xor`. `lacpRate` applies only to `802.3ad`. VLAN IDs range from 1 to 4094, and a VLAN can be declared only on an ethernet or bond that is not itself a bond member.

`metalman` refuses to build media for a host with a declared network unless the netboot image's `metadata.yaml` sets `localInstaller: unbounded-local-installer-v1`. That value tells the controller the installer can boot from local media, bring up the declared topology itself, and then fetch its instructions. The media also uses the `unbounded-uefi-local-v1` layout, which includes the installer and the rendered network description.

The repository includes a complete example at `deploy/machina/examples/metalman-bond-vlan.yaml`. It uses documentation address ranges and placeholder MACs and digests, so replace every value before applying it. It is the best starting point for a bonded host because it shows the lease, ethernet, bond, VLAN, route, DNS, and Redfish blocks together.

## Cloud-Init Customization

The installed system runs cloud-init on first boot. Cloud-init consumes two sources. The vendor data is managed by Unbounded: it writes the agent configuration to `/etc/unbounded/agent/config.json`, writes the agent install script, runs the script, disables cloud-init at the end, and installs package-manager protections that keep unattended upgrades from restarting `systemd-machined` and tearing down the running node. The user data is yours: it can install packages, set kernel parameters, add users, or run first-boot commands.

You supply user data through a ConfigMap and reference it from the `Machine`. The key defaults to `user-data` when omitted. Both `data` and `binaryData` are supported, and the value must be a valid cloud-init document, normally starting with `#cloud-config`.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: node-01-user-data
  namespace: unbounded-system
data:
  user-data: |
    #cloud-config
    packages:
      - chrony
    runcmd:
      - [ systemctl, enable, --now, chrony ]
```

```yaml
spec:
  host:
    netboot:
      cloudInit:
        userDataConfigMapRef:
          name: node-01-user-data
          namespace: unbounded-system
          key: user-data
```

If the referenced ConfigMap does not exist, `metalman` serves the default minimal document, which is exactly `#cloud-config`, and the install continues. That fallback is deliberate: a missing customization should not block a first boot. If the ConfigMap exists but the selected key is missing, the request fails instead. That distinction catches a typo in `key` without making the ConfigMap mandatory.

`cloudInitTimeout` bounds the time from the installer's reboot handoff to the completion of first-boot cloud-init and the registration of the `Node`. The default is 20 minutes. The budget covers firmware POST, so a large GPU server can spend most of it before Linux even starts. Raise the timeout when POST is slow rather than treating a slow POST as a failed install. The value must be positive.

```yaml
spec:
  host:
    netboot:
      cloudInitTimeout: 30m
```

Cloud-init reports progress to `POST /cloudinit/log`. `metalman` uses those events to set the `CloudInitDone` condition on the operation. A failed stage sets the condition to `False` with reason `Failed`, and the operation fails rather than waiting for the whole timeout. The final stage is `modules-final`; when it succeeds, the condition becomes `True`.

## Boot Flow

A bare metal `Machine` is a declaration, not a job. Creating one registers the lease, the image, and the BMC settings, but it does not boot anything by itself. The work starts when you create a `HostReplace` operation or when a person powers on a host that is already set to network boot. This split is the most important lifecycle difference from the SSH path: the machine describes a host that should exist, and each operation describes one attempt to make the host match that description.

The operation has a phase and, for each target, a stage. The phase is `Pending`, `InProgress`, `Complete`, or `Failed`. The stage tells you where the physical sequence is. For a PXE or HTTP repave, `metalman` moves through power-off, boot override, installer, cloud-init, and node registration. Progress is recorded on the operation, not on the `Machine`.

| Stage | What it means | What to inspect when it stalls |
| --- | --- | --- |
| `PoweringOff`, `WaitingOff` | The controller asked the BMC to force the host off | BMC reachability, credentials, power state |
| `PoweringOn`, `WaitingOn` | The boot override is set and the host is starting | Redfish boot override support, firmware boot order |
| `WaitingRepave` | The host should be running the installer or booting the media | DHCP or boot URL, TFTP, HTTP port 8880, host console |
| `WaitingCloudInit` | The image was written and the host rebooted | First-boot cloud-init log, network, image contents |
| `WaitingNode` | Cloud-init finished and the node should register | Attestation, CSR approval, kubelet logs, API reachability |

The target also carries conditions that latch the important milestones: `BootLoaderDownloaded`, `BootImageWritten`, and `CloudInitDone`. The operation-level conditions are set for single-machine operations; for selector operations, the same milestones are recorded per target. The `BootImageWritten` transition is also the installer's reboot handoff, which starts the cloud-init budget.

### Repave Requested

The normal way to start an unattended install is the CLI:

```bash
kubectl unbounded machine repave node-01
```

The command creates a `HostReplace` `MachineOperation` named `<machine>-repave-<unix timestamp>` with a default TTL of 300 seconds after completion, then watches it and prints phase changes until it reaches a terminal state. Use `--ttl 0` if you want to keep the operation for inspection.

```bash
kubectl get machineoperation
kubectl get machineoperation node-01-repave-1700000000 -o yaml
```

Creating the operation does not immediately write a disk. `metalman` first checks that it owns the target, that the target is not already involved in a conflicting operation, and that the isolation mode is allowed. It then obtains a power client. If the `Machine` has no `redfish` block, there is no power client, and the operation cannot complete; boot the host from the network manually instead.

### Boot Handoff

The handoff depends on `bootProtocol`. All three paths end at the same installer, but they reach it differently.

For `PXE`, the controller sets the Redfish boot override to PXE with continuous enabled, powers the host on, and waits. The firmware sends a DHCP request. `metalman` matches the MAC, returns the static lease, sets the TFTP server name, and offers the boot file from `metadata.yaml`, normally `shimx64.efi`. The host downloads the shim over TFTP, the shim chainloads GRUB, and GRUB fetches the rendered `grub.cfg` over HTTP. The TFTP download of the initial loader sets `BootLoaderDownloaded`.

For `HTTP`, the controller uses Redfish to find the EthernetInterface whose MAC matches the first lease. It disables DHCPv4 on that interface, writes the lease's IPv4 address, subnet mask, gateway, and DNS servers, and sets a UEFI HTTP boot URL derived from `httpBootPath`. It then powers the host on. The firmware fetches the EFI binary directly over HTTP, with no DHCP or TFTP exchange. If the BMC does not expose a writable EthernetInterface, the controller falls back to BIOS attributes and a one-time UEFI HTTP boot target. Some firmware ignores those attributes, which is a hardware conformance issue rather than a controller setting.

For `VirtualMedia`, the controller does not use DHCP or TFTP at all. It builds an operation-owned ISO, stages it on the BMC or rack manager, mounts it, sets the boot override to the virtual drive, and powers the host on. The ISO's GRUB hands off to the installer, which brings up the declared network or uses the lease and then fetches its instructions over HTTP. The next section covers this path in detail.

### GRUB Decision

The rendered `grub.cfg` is the safety interlock. It has two jobs. When an install is pending for the machine, it boots the installer kernel with the image URL, serve URL, cloud-init seed URL, node name, API server URL, lease address, gateway, disk, and console settings on the command line. When no install is pending, it chainloads the locally installed bootloader instead.

The check is based on active `HostReplace` operations. A target that has not written its image, has not reached a terminal phase, and has no operation-owned boot media counts as an install request. Once the operation is terminal, or once the `BootImageWritten` condition is true, the installer entry is no longer selected. That is what makes network boot safe as a permanent default boot option. A stale boot override, or a host that always PXE boots, still lands on its disk when nobody asked for a repave.

The template passes `unbounded.boot_mac` from the selected lease. The installer uses that MAC to find the provisioning NIC instead of assuming a name such as `eth0`. It passes `unbounded.dhcp_hold=1` when the lease sets `holdLease`, and it passes the lease DNS servers. If `targetDisk` is set, it passes `unbounded.disk`; otherwise the installer chooses a disk. The template also selects a serial console on non-arm64 hosts and omits it on arm64.

### Installer

The installer is an initramfs overlay, not a general-purpose operating system. It starts by loading storage and network drivers, including a PCI modalias pass, so a RAID controller that is not in the fixed module list still appears. It then finds the provisioning NIC by MAC, brings the link up, and applies the static address, gateway, and DNS servers from the kernel command line. On the virtual media path it brings up the declared host network first and then fetches its instructions from `metalman`, so it does not depend on DHCP at all.

Next it resolves the target disk. If `targetDisk` is set, it waits for that device to appear, including a by-id path, which it recreates from sysfs because the installer has no udev. If it is not set, it selects the largest whole block device and logs a warning. It downloads the machine image over HTTP with up to 120 attempts, which covers a slow registry pull behind the cache, decompresses it, and writes it with `dd`.

After writing, the installer mounts the new root filesystem and prepares the first boot. It writes a NoCloud datasource configuration that points at `metalman`, disables cloud-init's own network rendering so a fallback DHCP configuration cannot conflict, and persists the static network configuration. On an image with `/etc/netplan` it writes `/etc/netplan/99-unbounded-metal.yaml`; on an image with `/etc/systemd/network` it writes `.netdev` and `.network` files. It also writes `/etc/unbounded-metal/config` with the serve URL and node identity. On UEFI systems it creates a boot entry for the installed loader when `efibootmgr` is available; otherwise the GRUB chainload path remains the fallback.

Finally, the installer signals `GET /pxe/disable` and reboots. The handler finds the `Machine` by the request's source IP and records `BootImageWritten`. That condition ends the install intent for `grub.cfg`, so the next network boot chainloads the installed disk instead of starting the installer again. On the virtual media path the installer first reports the image-written state and waits for the controller to release the ISO and clear the boot override, so a failed cleanup cannot cause a second install.

### First Boot

The installed system boots and cloud-init runs. Vendor data writes the agent configuration and install script, then runs the script. The agent configuration contains the attestation URL, the cluster endpoint, the cluster CA, the Kubernetes version, the node name, labels, taints, and any download overrides. The script downloads the agent release and starts it.

Vendor data also disables cloud-init at the end, so first-boot work runs once, and installs package-manager protections that keep unattended upgrades from restarting `systemd-machined`. The agent re-asserts the same protection during start, so it exists from first boot even if a package upgrade races the agent install.

If you supplied user data, cloud-init merges it with the vendor data. A package install or a `runcmd` entry runs in the same first-boot pass. If the user data is invalid, cloud-init reports a failed stage and the `CloudInitDone` condition becomes `False` with reason `Failed`; the operation fails instead of waiting for the timeout.

### Node Join

The agent prepares the node stack. It installs host packages, downloads the container runtime and Kubernetes artifacts, creates the isolation environment, starts containerd and kubelet, and requests a bootstrap token through TPM attestation. Kubelet uses that token for TLS bootstrap, submits a client certificate request, and becomes `Ready` after `metalman` approves the request.

The `MachineOperation` completes when the expected `Node` exists, not when the installer reboots. The expected name is `spec.kubernetes.nodeRef.name` when set, otherwise the `Machine` name. That is why a node that joins under a different name leaves the operation in `WaitingNode` even though the host is healthy.

## Virtual Media

Virtual media is the third boot protocol. Instead of relying on DHCP and TFTP, `metalman` builds a small bootstrap ISO for one operation, stages it on the BMC or a rack manager, mounts it as a virtual drive, and tells the firmware to boot from it. The ISO contains the installer and a live GRUB handoff, not the machine image. The installer fetches the rest of its instructions and the disk image over HTTP.

This path exists for hosts that cannot use network boot reliably. A link that only passes traffic after LACP negotiates is the common case: firmware PXE may time out before the bond is up. A host whose NIC driver is missing from the installer kernel is another. Virtual media also helps when a rack manager stages media for many blades and you do not want a TFTP server on the provisioning segment.

Virtual media has stricter requirements than PXE. `bootProtocol: VirtualMedia` requires a `redfish` block. Both `image` and `netbootImage` must be digest references. The netboot image's `metadata.yaml` must set `mediaFormat: unbounded-uefi-v1`, and it must set `localInstaller: unbounded-local-installer-v1` when the machine declares `network`. A missing or mismatched value fails the operation with an explicit error instead of building an incompatible ISO.

The controller records media ownership before it changes the BMC. The target's `status.bootMedia` carries the media ID, the source server and name, the machine UID, a copy of the netboot spec, the certificate fingerprint, and flags for image written, cleaned, staged deleted, released, and abandoned. That state is what lets a retry clean up media it created rather than adopting another operation's ISO.

The media ID is 128 random bits rendered as 32 lowercase hex characters, and the ISO is named `ub-<id>.iso`. Some rack managers attach a readable drive only after a mount under a different name, so the provider also exposes a primer name `up-<id>.iso` for the same image. The ISO is a hybrid: an El Torito EFI boot image plus a GPT EFI System Partition, because some rack managers expose it as a removable USB disk.

The boot media phases are visible in the target message and in `status.bootMedia.phase`.

| Phase | Meaning |
| --- | --- |
| `Preparing` | The controller is building the ISO, which may wait on a first-time image pull |
| `Staging` | The ISO is being placed in the BMC or rack-manager media store |
| `PoweringOff` | The host is being forced off before the media is mounted |
| `Mounting` | The ISO is being mounted as a virtual drive |
| `SettingBoot` | The boot override is being set to the virtual drive |
| `PoweringOn` | The host is being powered on |
| `Installing` | The host should be running the media installer |
| `Replacing` | The controller is replacing staged media after the host did not boot it |
| `Installed` | The installer handed off and the controller is waiting for the installed system |

The installer does not write a disk until it is authorized. It fetches `GET /bootmedia/<id>/install` before installing. `metalman` returns `409 Conflict` when the media is no longer authorized for a pending operation or when the host is in a phase where it should not boot, and the installer powers the host off. Transient lookup failures return `503` so the installer can retry. After writing the image, the installer reports completion to `POST /bootmedia/<id>/complete` and waits for `GET /bootmedia/<id>/ready` before rebooting.

Two flags matter for virtual media deployments. `--serve-url` must be an address the BMC can reach, because the BMC downloads the ISO from it. `--media-tftp-server` advertises an IPv4 address for rack-manager TFTP pulls, which is separate from the host's own boot path and defaults to the server IP. Standard BMCs use the HTTP URL; rack managers use TFTP.

A BMC may require a license for virtual media. If the mount request never completes, check the license before debugging the network. If the host powers on but never fetches its loader configuration, the controller retries the boot up to three times and replaces the staged image with a new name before giving up.

## TPM Attestation

A bootstrap token is a credential. Delivering one to a host that merely claims to be a machine would let an attacker join the cluster by spoofing an address. Bare metal solves that with TPM 2.0: the token is encrypted so that only the host with the matching endorsement key can decrypt it.

The exchange is one HTTP round trip to `POST /attest`. The host creates an endorsement key and a storage root key, reads both public keys in `TPM2B_PUBLIC` format, and sends them to `metalman`. The controller looks up the `Machine` by the direct TCP peer address, takes a short-lived ServiceAccount token for `metalman-bootstrap`, generates a random AES-256 key, wraps that key to the endorsement key with `CreateCredential`, encrypts the token with AES-256-GCM, and returns the credential blob, the encrypted secret, the nonce, the encrypted token, and the cluster CA. The host recovers the AES key with `ActivateCredential`, decrypts the token, and uses it for kubelet TLS bootstrap.

No cluster secret is embedded in the image. The token is minted at attestation time, expires after one hour, and is scoped to one machine. The host-side agent opens `/dev/tpmrm0`, the kernel resource manager, which multiplexes access and cleans up transient objects when the file descriptor closes. If the TPM is disabled in firmware, cleared, or not exposed to Linux, attestation fails before any token is issued.

Trust on first use is the identity anchor. The first endorsement key seen for a machine is written to `status.tpm.ekPublicKey`. Later attestations must present the same key. A different key is rejected with HTTP 403 before a token is minted, and the rejection is logged. That is what you do after replacing a motherboard or a TPM: clear the field deliberately rather than letting the new hardware silently adopt the old identity.

Before returning the token, the controller records the credential ID in `status.bootstrap.credentialID`, the node name in `status.bootstrap.nodeName`, and the issue time in `status.bootstrap.issuedAt`. The credential ID is the value the API server reports in the `authentication.kubernetes.io/credential-id` user extra, in the form `JTI=<uuid>`. Re-attestation overwrites it, so tokens issued by earlier attestations can no longer get kubelet certificates approved for the machine.

This is not measured boot. The exchange proves that the host possesses the endorsement key that was pinned for the machine. It does not quote PCRs, verify a boot measurement, or prove that the firmware or installed OS is unmodified. Treat it as a hardware-bound delivery channel for the token, not as remote attestation of the software stack.

Two preconditions are easy to miss. ServiceAccount tokens must carry a `jti`, which requires Kubernetes 1.30 or later; on an older cluster attestation fails rather than falling back to an unverified token. And the request must arrive directly from the host. Because the controller uses the peer address and ignores forwarding headers, it is not supported behind an HTTP proxy that rewrites the client address.

### Certificate Approval

Approval is part of the controller, not an external webhook. `metalman` watches kubelet client certificate requests from the `metalman-bootstrap` ServiceAccount. It approves a request only when the request's credential ID matches exactly one `Machine`'s recorded credential and the certificate's common name is `system:node:<nodeName>` for that machine. Otherwise it denies the request.

Each instance decides only requests whose credential is recorded on a `Machine` it owns and leaves the rest pending. That is what keeps a machine from being approved by the wrong site's controller. The Kubernetes controller manager does not auto-approve these requests, so a stalled CSR usually means the credential does not match, the common name is wrong, or the owning site's controller is not running.

A `ValidatingAdmissionPolicy` limits what the bootstrap ServiceAccount can do. It may update only its own machine's status, and it may not modify `status.tpm` or `status.bootstrap`. It cannot create machines. That keeps a compromised bootstrap credential from rewriting the trust anchor that authorized it. The policy and its binding are part of the shared metalman support set, planned once per pass, and the bootstrap grant is withheld if the live restriction cannot be verified.

## Secure Boot and Firmware Notes

The default amd64 and arm64 netboot images use the Ubuntu shim and GRUB. The Ubuntu shim verifies GRUB, and GRUB verifies the Ubuntu kernel, so the installer boots with Secure Boot enabled. The Ubuntu host image also ships a signed kernel, so the installed system can keep Secure Boot on.

The Azure Linux installer kernel variant is different. It runs the Azure Linux kernel instead of the Ubuntu kernel, and the Ubuntu shim does not trust that kernel. Build and use `netboot-azurelinux` only with Secure Boot turned off. This variant exists for arm64 hosts with a Microsoft MANA NIC, whose driver the arm64 Ubuntu kernel lacks. Turning Secure Boot off is a property of that boot chain, not a setting in the `Machine`.

Host images with unsigned kernel modules impose their own requirements. The NVIDIA host images build the open kernel module with DKMS, which produces an unsigned module that a Secure Boot host rejects unless you enroll a key or disable Secure Boot. That requirement belongs to the GPU image, not to `metalman`. See the [NVIDIA GPU guide]({{< relref "reference/gpu/nvidia" >}}) before combining Secure Boot with those images.

Firmware behavior is the other variable. UEFI HTTP boot depends on the BMC and firmware honoring the boot URL and the static IPv4 settings the controller writes. Applying those settings and fetching the first EFI binary are hardware responsibilities. A BMC that ignores the URL will not be fixed by a controller change. The same applies to virtual media: the BMC must support mounting a remote ISO, and some hosts require a license for it.

## Site Isolation

A site is the unit of ownership for bare metal. Each `Site` that enables the metalman component gets its own Deployment and its own leader-election lease. The Deployment's `--site` value is matched against the `unbounded-cloud.io/site` label on `Machine` resources. A `Machine` labeled for `rack-a` is owned by the `rack-a` controller and ignored by every other controller, including an unscoped one.

The scoping rule has one subtle case. An instance started without `--site` does not mean "all sites"; it means "machines with no site label". That is useful for a small installation that does not use sites, but it also means you cannot run one unscoped instance and one scoped instance and expect them to divide the cluster cleanly. Pick one model per cluster. If you label a machine for a site, only that site's controller owns it.

Ownership is enforced again when an operation runs. A `MachineOperation` is executed by the instance that owns its target `Machine`, and an operation that targets an unowned machine is left alone. Operations that could interfere with each other are serialized. The controller treats the machine identity and the normalized Redfish endpoint as conflict keys, so it will not run two host operations against the same BMC at the same time, even if two operations were created for different reasons. An older conflicting operation wins, and the newer one waits.

The practical consequence is that a rack with its own provisioning network can have its own failure domain. If the `metalman` Deployment for rack A is down, rack B is unaffected, and rack A's operations stay pending rather than being executed by the wrong controller. The tradeoff is that every machine must carry the correct label. A missing label is not a neutral omission when the controller is scoped; the machine becomes invisible to it.

## Operations

Day-2 host actions are `MachineOperation` resources. The CLI commands create them and watch them to completion. They are the same objects you would create by hand, so you can also apply your own operation manifest or use a selector to target several machines.

| Command | Operation kind | Effect |
| --- | --- | --- |
| `kubectl unbounded machine repave NAME` | `HostReplace` | Reimage the host and reinstall the agent |
| `kubectl unbounded machine reboot NAME` | `HostHardReboot` | Full power cycle through the BMC |
| `kubectl unbounded machine power-off NAME` | `HostPowerOff` | Power the host off through the BMC |
| `kubectl unbounded machine power-on NAME` | `HostPowerOn` | Power the host on through the BMC |
| `kubectl unbounded machine soft-reboot NAME` | `HostSoftReboot` | Reboot from inside the installed OS, handled by the agent |
| `kubectl unbounded machine node-reboot NAME` | `NodeReboot` | Restart the node stack without reprovisioning, handled by the agent |

`machine reboot`, `power-off`, and `power-on` require a `redfish` block. `machine reboot` fails early with an explicit message when there is none. `machine soft-reboot` and `machine node-reboot` are agent operations; they do not need BMC access, but the agent must already be running on the host.

A repave is destructive by definition: it writes the machine image over the target disk. The `Machine` name, lease, and BMC settings survive, but anything the previous OS wrote on that disk does not. If you need to preserve data, that data has to live somewhere else. For configuration changes that do not require a new disk image, prefer a new `MachineConfigurationVersion` and the normal node repave flow rather than a host repave.

## Verification

Verification follows the same split as troubleshooting: the operation answers boot and install questions, and Kubernetes answers node questions.

Start with the operation, not the machine. The `Phase` column is the fastest signal, and the target stage tells you where the physical sequence is.

```bash
kubectl get machineoperation
kubectl get machineoperation <operation-name> -o yaml
```

Read `status.targets[].stage`, `status.targets[].message`, and the target conditions. A completed target has phase `Complete` and a message such as `HostReplace completed`. The conditions should show `BootLoaderDownloaded`, `BootImageWritten`, and `CloudInitDone` all `True`.

Check the machine's hardware-bound state. For a PXE machine, `status.phase` is normally empty, but the TPM and bootstrap fields tell you whether attestation happened.

```bash
kubectl get machine node-01 -o yaml
kubectl get machine node-01 \
  -o jsonpath='{.status.tpm.ekPublicKey}{"\n"}{.status.bootstrap.credentialID}{"\n"}'
```

Check that the node exists and is ready. The expected name is the pinned node name, not necessarily the `Machine` name.

```bash
kubectl get node <node-name> -o wide
kubectl get node <node-name> -o jsonpath='{range .status.conditions[*]}{.type}{"="}{.status}{"\n"}{end}'
```

Check the controller itself. The Deployment, lease, and recent logs are usually enough to tell a crash loop from a boot problem.

```bash
kubectl -n unbounded-system get deploy metalman-controller-rack-a
kubectl -n unbounded-system get lease metalman-rack-a
kubectl -n unbounded-system logs deploy/metalman-controller-rack-a --tail=200
```

The logs report DHCP responses, TFTP file transfers, HTTP artifact requests, Redfish actions, media phases, and attestation outcomes. A missing DHCP response for the host's MAC is a network or lease problem. A TFTP request that never arrives is a firmware or DHCP option problem. An HTTP request for `disk.img.gz` that never completes is an artifact or network problem. An `/attest` request that returns 403 is a TPM identity problem.

## Troubleshooting

### Machine Stuck in Repaving

A machine is not "stuck in repaving" at the `Machine` level, because `metalman` does not drive `Machine.status.phase`. The wait is on the `HostReplace` target. Read its stage and message first. If the stage is `WaitingRepave`, the host has not reported that it wrote the image. That is a boot problem, not a Kubernetes problem: check that the firmware actually booted the network device, that the DHCP offer reached it, and that the HTTP server is reachable from the provisioning address.

If the stage is `WaitingNode`, the disk was written and the host is running the installed OS. The problem is now attestation, certificate approval, or kubelet configuration. If the stage is `PoweringOff` or `PoweringOn`, the problem is the BMC or its credentials. If the operation has failed after repeated attempts, inspect the last target message before retrying; it names the failed action.

Verify that the target can reach TCP 8880 on the controller. A host that can DHCP and TFTP but cannot fetch `grub.cfg` will never reach the installer. Verify that the controller can reach the BMC on HTTPS 443 and that the BMC is powered and responsive. If the boot override was set but the host booted its disk, the firmware may not support the requested target or may have a boot-order policy that overrides it. Check the host console before changing the `Machine`.

### DHCP Not Responding

First confirm the mode. In interface mode, verify that `--dhcp-interface` names the NIC on the provisioning segment and that the NIC has the address you expect. In auto-interface mode, verify that the resolved server address belongs to the intended NIC; a multi-homed controller can resolve the wrong one. In relay mode, verify that the relay forwards to the controller and that the request carries a nonzero relay address. Relay mode intentionally ignores direct client packets.

Check for a competing DHCP server. If another server offers the host a different address, the host may accept that offer and never ask `metalman` for the boot file. The controller stays out of a request that names another server identifier, so the symptom is a lease from the wrong server rather than an error from `metalman`.

Check the Layer 2 path. If the provisioning address is on a tagged VLAN, the switch port must carry that VLAN to the controller. If the host is behind a bond, the bond must be up before the firmware sends DHCP; that is the case where virtual media is the better path.

### BMC Connection Failures

Redfish calls fail for three common reasons: the endpoint is unreachable, the credentials are wrong, or the TLS certificate no longer matches the pinned fingerprint. Confirm HTTPS 443 from the controller to the BMC, then verify the Secret key named by `passwordRef.key`. The default key is `password`.

On first connection, the controller captures the BMC certificate fingerprint and stores it in `status.redfish.certFingerprint`. Later requests must match it. If the BMC certificate was rotated, clear the field so the controller pins the new certificate on the next reconcile:

```bash
kubectl patch machine node-01 --subresource=status --type=merge \
  -p '{"status":{"redfish":{"certFingerprint":""}}}'
```

If the BMC is a rack manager rather than a blade BMC, the Redfish URL may need a slot path such as `/7`, and virtual media may need `--media-tftp-server`. A rack manager that accepts the session but never mounts the ISO is usually a TFTP reachability or media-store problem, not a credential problem.

### TPM Attestation Rejected with 403

A 403 from `/attest` means the endorsement key presented by the host does not match `status.tpm.ekPublicKey`. That is expected after a motherboard or TPM replacement, and it is also what happens when a different host is put behind an existing `Machine` name. Confirm that the change is intentional, then clear the pin:

```bash
kubectl patch machine node-01 --subresource=status --type=merge \
  -p '{"status":{"tpm":null,"bootstrap":null}}'
```

The next attestation stages the new endorsement key and records a new bootstrap credential in the same status update. If the host has no TPM device, check firmware settings and confirm that `/dev/tpmrm0` exists on the installed system. If the cluster is older than Kubernetes 1.30, attestation fails even with a correct key because the issued token has no `jti`; there is no fallback.

### Node Not Joining

A host that boots and runs cloud-init but never registers is usually failing at attestation, certificate approval, or kubelet startup. Verify that the host can reach the Kubernetes API on TCP 6443. Verify that the `metalman-bootstrap` ServiceAccount exists and that its RBAC and the admission restriction are present. Verify that `status.bootstrap.credentialID` is set on the `Machine`; without it, no CSR can match.

Check that the CSR was approved by the owning site's controller. A CSR with the right credential but the wrong common name is denied. The common name must be `system:node:<status.bootstrap.nodeName>`. If the node name is wrong, the credential was issued for a different name, often because `spec.kubernetes.nodeRef` changed after provisioning. Re-attestation is required to record a new name.

Read the kubelet and agent logs on the host. Cloud-init logs are also reported to `POST /cloudinit/log`, and the agent install log is reported to `POST /unbounded-agent/install-log`, so the controller logs may already contain the failure. If the agent never started, inspect the cloud-init vendor-data run and the install script output.

### UEFI HTTP Boot Does Not Start

Confirm that `--serve-url` is reachable from the host, not just from the controller. Confirm that the BMC exposes a writable EthernetInterface for the first lease MAC. If it does not, the controller falls back to BIOS attributes; some firmware ignores them, and the host stays on its previous boot option.

The boot URL comes from `httpBootPath` in the netboot image metadata. If the key is missing, the controller falls back to `dhcpBootImageName`, which is the PXE shim name and may not be the file the HTTP firmware expects. Check the metadata and the HTTP path before changing the `Machine`.

### Virtual Media Mount Never Completes

If `status.bootMedia.phase` stays at `Staging` or `Mounting`, the BMC did not accept the media. Check three things in order. First, confirm that `--serve-url` is reachable from the BMC, because a standard BMC downloads the ISO over HTTP. Second, confirm that a rack manager can reach the TFTP server address advertised by `--media-tftp-server`, because rack managers use TFTP instead. Third, confirm that the BMC has a virtual media license and that no other session holds the virtual drive.

If the phase reaches `Installing` but never records a loader start, the host powered on without a usable virtual drive. The controller powers the host off, replaces the staged image with a new name, and retries up to three times. A host that still does not boot the media is a firmware or media-store problem. Check the host console and the BMC's media log before creating another operation.

If the installer reports image written but the operation stays at `WaitingRepave`, the controller is waiting for the media cleanup handoff. Do not power the host off. The installer is waiting for the controller to detach the ISO and clear the boot override so it cannot install twice. If cleanup cannot complete, the target fails after its retention window and keeps the media ownership record for inspection.

## Limitations

The boot chain is UEFI only. The netboot image ships EFI shims and GRUB binaries, not a legacy BIOS boot sector. A server that can only boot in legacy mode cannot use this path without a different boot environment.

Static host networking is IPv4 only, requires `bootProtocol: VirtualMedia`, requires a first lease for the provisioning identity, and does not support `holdLease`. The installer can persist netplan or systemd-networkd configuration, but the declared model does not include IPv6, policy routing, or multiple default gateways.

TPM attestation requires Kubernetes 1.30 or later because the issued ServiceAccount token must carry a `jti`. There is no fallback to an unverified token on an older cluster.

`metalman` must see the host's real source address. Forwarding headers are ignored, so an HTTP proxy that rewrites the client address breaks attestation. A reverse proxy in front of the HTTP server is not supported.

Inventory is observation only. It does not create `Machine` resources, provide leases, or drive boots. You still declare each host in Kubernetes.

UEFI HTTP boot depends on firmware and BMC conformance. Writing the static IPv4 configuration and fetching the first EFI binary are hardware responsibilities. A BMC that ignores the boot URL or the EthernetInterface settings will not be fixed by the controller.

Virtual media may require a BMC license. If the ISO mount never completes, check the license before debugging the network. The media path also requires the netboot image and controller to agree on `mediaFormat`, and on `localInstaller` when `network` is declared.

The first-boot budget defaults to 20 minutes and starts at the installer reboot handoff. Large GPU servers can spend most of that in firmware POST. Raise `cloudInitTimeout` when that is the case rather than treating a slow POST as a failed install.

`MachineClass` and `MachineAllocation` are not part of the bare metal path. The only machine-ops provider in this repository is `AzureVM`. A physical host is declared directly as a `Machine` with `spec.host.netboot`.

## See Also

- [Bare metal concepts]({{< relref "concepts/bare-metal" >}}) for the model, the lifecycle, and how bare metal differs from cloud virtual machines.
- [SSH provisioning]({{< relref "guides/ssh" >}}) for hosts that already run Linux and are reachable over SSH.
- [Cloud API provisioning]({{< relref "guides/cloud-api" >}}) for allocation-driven capacity through `MachineClass`.
- [Host operations]({{< relref "guides/operations/host-operations" >}}) for repave, reboot, and power procedures after the first install.
- [Machina CRD reference]({{< relref "reference/machina-crd" >}}) for every `Machine`, `MachineOperation`, and `MachineClass` field.
- [Agent reference]({{< relref "reference/agent" >}}) and [agent configuration]({{< relref "reference/agent/configuration" >}}) for what runs on the host after first boot.
- [CLI reference]({{< relref "reference/cli" >}}) for the exact flags of `install`, `site init`, and the `machine` commands.
- [Architecture reference]({{< relref "reference/architecture" >}}) for how `metalman` fits with the operator, machina, and the network controller.
- [Workload overrides]({{< relref "reference/workload-overrides" >}}) for appending flags such as `--dhcp-interface` or `--experimental-container-isolation` to the per-site Deployment.

