---
title: "Bare Metal Provisioning"
description: "How Unbounded discovers, matches, boots, and repaves physical hosts with the metalman controller."
weight: 3
---

Bare metal provisioning is the part of Unbounded that turns a physical server into a
Kubernetes worker. The host starts with no operating system you control, a network
interface, and usually a baseboard management controller (BMC). By the end, the host
has a disk image written to it, the Unbounded agent installed, a bootstrap credential
proven to belong to that exact hardware, and a `Node` object in your cluster.

The mental model is simple. A `Machine` resource declares how to recognize one physical
host and what to install on it. The `metalman` controller runs inside the cluster, on
the site's provisioning network, and does the work: it answers the host's network boot,
serves the installer and images, controls power through Redfish, and hands out bootstrap
tokens. Everything else, including the Kubernetes join, is the same join sequence used
by every other provisioning path.

This page explains the components and the model. It covers how you learn what hardware
you have, how a booting host is matched to a `Machine`, what the PXE, API, and SSH paths
each assume, what host preparation actually writes to disk, how the `Machine` lifecycle
is reported, and where bare metal differs from provisioning cloud virtual machines.

## When to Use Bare Metal Provisioning

Use bare metal provisioning when the host is hardware you own or lease directly and you
need Unbounded to install the operating system. The typical cases are new servers that
arrived without an OS, cluster expansion with identical hardware, and reimaging after a
damaged disk, a failed upgrade, or a hardware swap. In each case there is no useful
operating system on the machine to log in to, so the install has to start below the OS.

Bare metal is also the path that gives you out-of-band control. A `Machine` with a
`redfish` block can be powered off, powered on, hard rebooted, and repaved without
anyone touching the rack. That is what makes the path unattended. Without Redfish you
can still provision a host, but someone has to put it into network boot and power it on.

Choose a different path when the precondition is different:

- If the host already runs Linux and you can reach SSH, use
  [SSH provisioning]({{< relref "guides/ssh" >}}). It is the simplest path and does not
  need a provisioning network.
- If the capacity comes from a cloud provider that creates instances through an API,
  use [Cloud API provisioning]({{< relref "guides/cloud-api" >}}). There is no physical
  host to boot and no BMC to talk to.
- If the hardware has no OS, no SSH, and a BMC, use this path and the
  [PXE guide]({{< relref "guides/pxe" >}}).

The three paths are not exclusive. A single cluster can contain bare metal in one site,
cloud instances in another, and SSH-registered machines in a lab, because all of them
produce the same `Machine` and `Node` contract.

## Choosing a Provisioning Path

The paths differ in what they assume about the host, which controller acts on it, and
where the bootstrap credential comes from. The table summarizes the distinction. The
sections after it explain why each difference matters.

| Question | PXE (bare metal) | SSH | Cloud API |
|---|---|---|---|
| What must exist first | Hardware, NIC, network boot, optional BMC | Running Linux with SSH access | Provider adapter and a ready `MachineClass` |
| Who performs the install | `metalman`, from the provisioning network | `machina`, over SSH from the cluster | The provider, through cloud-init on first boot |
| How the OS arrives | Raw disk image over HTTP, written by an installer | The host keeps its OS; the agent is copied over SSH | Provider image plus cloud-init user data |
| How power is controlled | Redfish BMC, or manually | The host is already on | Provider API |
| Where the token comes from | TPM 2.0 attestation against `metalman` | `spec.kubernetes.bootstrapTokenRef` secret | The allocation's bootstrap token |
| Best fit | New or broken hardware | Existing servers and labs | Elastic, API-driven capacity |

The PXE path assumes the least about the host and the most about the network. The host
needs a NIC whose MAC address you know, a DHCP path to `metalman` (directly or through a
relay), and reachability to the HTTP artifact server. If you also want unattended
repaves, it needs a BMC that speaks Redfish and credentials that `metalman` can read.

The SSH path assumes an OS and a credential. `machina` probes the address, copies an
install script and a generated agent configuration to the host, runs the script with
`sudo`, and then tracks the node join. It works well for machines that were provisioned
by some other system and for hosts behind a bastion.

The cloud API path assumes a provider integration. Karpenter reacts to unschedulable
pods, a `MachineAllocation` is created, and a provider adapter launches an instance with
cloud-init user data. The cluster does not connect to the host to install anything;
cloud-init does the work on first boot. This is the path to use when capacity should
appear automatically rather than when a human racks a server.

## The Bare Metal Model

Three ideas carry the whole design: inventory tells you what hardware exists, matching
binds one booting host to one `Machine`, and `metalman` performs the boot and the
credential exchange. Keeping those separate is what lets the same controller serve many
racks and many hardware generations without per-host agents in the control plane.

### Inventory: knowing what hardware you have

Unbounded ships a separate inventory subsystem for hardware facts. It is not part of the
boot path, but it is how you collect the serial numbers, MAC addresses, disks, and switch
topology you need before you write a `Machine`.

The subsystem has four binaries. `inventory-agent` runs as root on each node, reads
sysfs, procfs, and a set of optional utilities, stores what it collects in a local
SQLite database, and publishes it over gRPC to `inventory-aggregator`. The aggregator
persists devices and neighbors to PostgreSQL. `inventory-viewer` serves a read-only web
UI plus JSON endpoints at `/api/devices` and `/api/neighbors`. `inventory-inspector` runs
periodically and records conflicts, currently duplicate device serial numbers and
duplicate LLDP upstream ports.

The agent collects chassis identity from DMI, BMC data over IPMI, CPU and memory
details, disks, physical NICs, and GPUs. It also records LLDP neighbors for switch
topology and, on NVIDIA systems, IMEX and NVLink peers. Devices are upserted by serial
number, so repeated runs update a record rather than creating a new one. Neighbors use
the pair of host identifier and local interface.

That data is useful before provisioning because a `Machine` for the PXE path needs exact
MAC addresses and static addresses. It is also useful after provisioning, when you need
to know which physical port a host is cabled to. Inventory is a record of what is there,
not a controller: `metalman` does not read the inventory database, and adding a device to
inventory does not create a `Machine`. You still declare the host in Kubernetes.

The repository ships manifests for the aggregator, the viewer, and the inspector. The
agent runs on the host rather than as a cluster workload, so it is installed and started
outside the manifests in this repository.

### Matching a booting host to a Machine

A bare metal `Machine` declares one or more static DHCP leases. Each lease carries a MAC
address, an IPv4 address, a subnet mask, a gateway, and optional DNS servers. Those
leases are the join key between a physical host and a Kubernetes object. `metalman`
builds field indexes over the lease MAC addresses, normalized to lowercase, and over the
lease IPv4 addresses. When a DHCP request arrives, the MAC in the request selects the
`Machine`; when an attestation request arrives, the direct TCP peer address selects it.

This is deliberately narrow. The host is not identified by a hostname the firmware
reports, by a serial number in an HTTP header, or by an agent running in the installer.
It is identified by an address that the network stack can observe. That makes the match
hard to spoof from another subnet, and it is why the provisioning network should be
mostly trusted. It is also why a proxy that rewrites the client address breaks
attestation: `metalman` ignores `X-Forwarded-For`, `X-Real-IP`, and `Forwarded`, and uses
only the direct peer.

The same lease data is reused later. Kubelet client certificate requests are approved
only when the credential ID in the request matches the credential recorded on a
`Machine` and the certificate's common name is `system:node:<nodeName>` for that machine.
The credential ID is indexed as well, so the approver can find the owning `Machine`
without scanning the cluster.

Site membership is the outer scope for matching. `metalman` instances select `Machine`
resources by the `unbounded-cloud.io/site` label. An instance started with `--site=rack-a`
owns machines labeled for `rack-a`; an instance started with no `--site` owns machines
that do not carry the label at all. A `MachineOperation` is only executed by the instance
that owns its target, so two sites can share a cluster without fighting over hosts.

### The metalman controller

`metalman` is a single binary with the boot servers built in. It runs as
`metalman serve-pxe` and provides four services from one process:

| Service | Default port | Protocol | Purpose |
|---|---|---|---|
| DHCP | 67/udp | DHCPv4 | Static leases from `Machine` NIC specs, boot file or HTTP boot URL |
| TFTP | 69/udp | TFTP | Initial bootloader delivery |
| HTTP | 8880/tcp | HTTP | Installer artifacts, templated configuration, images, attestation, callbacks |
| Health | 8081/tcp | HTTP | Liveness and readiness probes |

The same process runs reconcilers for OCI image pulling and caching, for Redfish
certificate pinning, and for `MachineOperation` host actions such as power changes and
repaves. It also approves kubelet certificate requests for the machines it bootstrapped.

In a cluster you normally do not run `metalman` by hand. Enable the component on a
`Site`, and the Unbounded operator creates the per-site Deployment named
`metalman-controller-<site>`. The Deployment uses host networking, because DHCP and TFTP
bind host ports, and it is scheduled to nodes carrying the site label. The component
block looks like this:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: rack-a
spec:
  components:
    metalman:
      enabled: true
      dhcpAutoInterface: true
```

The operator passes `serve-pxe` and `--site=<site>`. It enables `--dhcp-auto-interface`
when the Site sets `dhcpAutoInterface`, and it sets no other arguments unless you
override the workload. The remaining flags are available when you run the binary
directly, for example in a lab or when testing a boot path:

| Flag | Default | Effect |
|---|---|---|
| `--site` | empty | Selects `Machine` resources by the site label |
| `--dhcp-interface` | empty | Binds broadcast DHCP to one named interface |
| `--dhcp-auto-interface` | `false` | Detects the DHCP interface from the server bind address |
| `--http-port` | `8880` | HTTP artifact and callback port |
| `--health-port` | `8081` | Health and readiness port |
| `--cache-dir` | `~/.unbounded/metalman/cache` | Local cache for unpacked OCI images |
| `--serve-url` | empty | External URL of this instance, used in boot configuration |
| `--default-netboot-image` | release-matched | Netboot image used when a machine omits one |
| `--operation-max-concurrent-machines` | `10` | Targets advanced concurrently within one operation |
| `--operation-poll-interval` | `5s` | Poll interval for in-progress operations |

Leader election is always enabled. Each site gets its own lease named `metalman-<site>`;
an unscoped instance uses the lease name `metalman`. Replicate a site's Deployment if you
want failover, and expect one leader per site lease.

The security model is worth stating plainly: `metalman` assumes a mostly trusted network
between itself and the hosts. Bootstrap tokens are issued to the source address that
matches a `Machine` NIC, so anyone who can source traffic from that address at the right
moment can ask for a token. TPM attestation limits where the token can be used, because
the token is encrypted to the host's endorsement key, but it does not authenticate the
network path. Keep the provisioning network separate from untrusted networks.

## How PXE Boot Works

PXE is the firmware's network boot path. The host firmware sends a DHCP request, receives
an address and a boot file name or boot URL, downloads a bootloader, and then boots the
kernel and initramfs that the bootloader names. Unbounded uses that chain to run a small
installer in memory, write a disk image, and then reboot into the installed system.

![Bare metal PXE boot flow showing firmware, DHCP, TFTP, HTTP, installer, and first boot](../../img/bare-metal-pxe-boot.svg)

The sequence for a repave through `metalman` looks like this:

1. The firmware starts a network boot. It sends a DHCP request tagged as a PXE client, or
   as an HTTP client when UEFI HTTP boot is selected.
1. `metalman` looks up the lease by MAC, offers the configured static address, and
   returns either a boot file name for TFTP or a boot URL for HTTP boot.
1. TFTP serves the initial bootloader, typically `shimx64.efi`, which chainloads GRUB.
   `metalman` records that the loader was downloaded on the operation target.
1. The host requests the rendered `grub.cfg` over HTTP. The template checks whether a
   `HostReplace` operation is pending for this machine.
1. If a repave is pending, GRUB boots the installer kernel with the image URL, serve URL,
   cloud-init seed URL, node name, API server URL, lease address, gateway, disk, and
   console settings on the kernel command line. If no repave is pending, GRUB chainloads
   the locally installed bootloader instead, so network boot remains safe as a default
   boot option.
1. The installer runs from the initramfs. It loads storage and network drivers, selects
   the provisioning NIC by MAC, and applies the static address from the kernel command
   line. On the virtual media path it brings up the declared host network and fetches its
   instructions from `metalman` instead.
1. The installer downloads the machine image and writes it to the target disk, then
   mounts the new root filesystem and injects the cloud-init datasource, static network
   configuration, and a small file describing the serve URL and node identity.
1. The installer creates a UEFI boot entry for the installed disk where possible, signals
   `/pxe/disable`, and reboots.
1. The installed system boots and cloud-init runs. The vendor data writes the agent
   configuration and install script, then runs the script.
1. The agent installs into the host, prepares the node stack, and requests a bootstrap
   token from `metalman` using TPM attestation. Kubelet starts, the node joins, and the
   operation completes.

Two parts of that sequence surprise people. First, there are two distinct installs: the
installer writes an OS image, and the first boot installs and starts the Unbounded agent.
Second, the same GRUB configuration serves both a repave and a normal boot. Network boot
is not a one-way door. If no operation is pending, the host chainloads its disk and the
provisioning server stays out of the way.

`metalman` protects a healthy node in two places. For PXE and HTTP boot, the rendered GRUB
configuration chainloads the installed disk when no `HostReplace` operation is pending.
For virtual media, the installer asks for install instructions before writing anything;
`metalman` returns 409 when the media is no longer authorized for a pending operation or
the host is in a phase where it should not be booting, and the installer powers the host
off instead of writing a disk. Either way, a stale boot override cannot reimage a node
that nobody asked to repave.

## Key Concepts

The rest of the model is in the details: how DHCP is scoped, what the two OCI images
contain, which fields describe a machine, how virtual media differs from network boot,
how sites stay isolated, how the token exchange is protected, and which operations can be
requested.

### DHCP Modes

`metalman` can answer DHCP in three modes, and the mode has to match the network between
the controller and the host. The difference is where the DHCP request arrives from, not
which leases are configured.

| Mode | Selection | Behavior |
|---|---|---|
| Interface | `--dhcp-interface=eth0` | Binds one interface and answers broadcast DHCP on the local segment |
| Auto-interface | `--dhcp-auto-interface` | Chooses the interface from the server bind address |
| Relay | neither flag | Listens for unicast only, expecting a DHCP relay to forward requests |

Interface mode is the simplest when `metalman` is attached to the same Layer 2 segment as
the hosts. Auto-interface mode is for deployments where the bind address already implies
the correct interface, and it is mutually exclusive with the named interface. Relay mode
is the default and is the one to use when the provisioning network is behind a router or
when a relay agent already forwards DHCP between subnets.

In every mode the lease comes from the `Machine`, not from a dynamic pool. The server
returns the configured IPv4 address, subnet mask, gateway, and DNS servers, and it sets a
one-day lease time. It answers any discover or request from a MAC that has a lease, so a
machine can use the same static address during the installer boot and after the OS is
installed. A lease with `holdLease: true` narrows that to boot firmware only: the
installed OS runs its own DHCP client to keep the address reserved on the site DHCP
server, and `metalman` ignores those non-firmware requests so the client binds to the
site server instead. That detail is covered under host preparation.

### OCI Images

A repave needs two OCI images. The machine image is the operating system that ends up on
disk. The netboot image is the reusable boot environment that runs the installer. They
are separate so that one boot environment can install many machine images, and so that
image updates do not require rebuilding the bootloader and kernel for every host.

The machine image contains `/disk/disk.img.gz`, a gzip-compressed raw disk image. It is
built `FROM scratch` and follows the containerDisk convention: artifacts live under
`/disk/` in the image layers. `metalman` streams the file to the installer, which
decompresses it and writes it to the target disk.

The netboot image contains the bootloader, kernel, initramfs, the installer overlay,
templates, and a `metadata.yaml`. Files with a `.tmpl` suffix are rendered per machine at
serve time, which is how the GRUB configuration, meta-data, vendor data, and network
configuration pick up the right machine name, addresses, and URLs. Files without that
suffix are served verbatim. The metadata file supplies image-level settings such as
`dhcpBootImageName` for the TFTP boot file name, `httpBootPath` for UEFI HTTP boot,
`mediaFormat` for scoped installer callbacks, and `localInstaller` when the image boots
from local media and configures host networking itself.

Images are pulled and unpacked by `metalman` into the cache directory, under
`oci/<digest>/<architecture>/disk/...`. That cache is why the first boot after an image
change is slower than later boots, and why the HTTP service can serve artifacts without
contacting the registry on every request. Private registries are supported through
`pullSecretRef` for the machine image and `netbootPullSecretRef` for the netboot image.
The netboot pull secret is ignored when no netboot image is specified, because the
controller uses its own configured secret for the default image.

### Machine CRD (PXE Fields)

A `Machine` is cluster scoped, has the short name `mach`, and prints the SSH host, phase,
Kubernetes version, and age as columns. The netboot configuration belongs under
`spec.host.netboot`. The older top-level `spec.pxe` shape is still served for existing
machines, and `metalman` resolves both through the same accessor, but new machines should
use `spec.host.netboot`.

| Field | Type | Default | Purpose |
|---|---|---|---|
| `image` | string | required | Machine image containing `/disk/disk.img.gz` |
| `architecture` | `amd64` or `arm64` | `amd64` | Architecture of boot artifacts and image |
| `netbootImage` | string | release default | Netboot environment override |
| `bootProtocol` | `PXE`, `HTTP`, `VirtualMedia` | `PXE` | How to trigger the network boot |
| `dhcpLeases` | list | empty | Static leases: `mac`, `ipv4`, `subnetMask`, `gateway`, `dns`, `holdLease` |
| `targetDisk` | string | automatic | Block device the installer writes |
| `redfish` | object | unset | BMC URL, username, device ID, password secret |
| `cloudInit` | object | unset | ConfigMap reference for first-boot user data |
| `cloudInitTimeout` | duration | `20m` | Budget from installer handoff to first-boot completion |
| `network` | object | unset | Ethernets, bonds, VLANs, and DNS for static host networking |
| `pullSecretRef` | secret reference | unset | Registry credentials for the machine image |
| `netbootPullSecretRef` | secret reference | unset | Registry credentials for the netboot image |

A minimal bare metal machine needs a name, an image, and at least one lease. The
following example also configures Redfish, which is what allows an unattended repave:

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
      targetDisk: /dev/disk/by-id/nvme-Model_Serial_1
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
          namespace: unbounded-system
          key: password
  kubernetes:
    version: v1.34.0
    nodeLabels:
      hardware.example.com/class: gpu
```

The BMC password is read from the named Secret. The key defaults to `password`, and the
Redfish device ID defaults to `1`. The URL is the Redfish endpoint and may include a
system path prefix; it must not include `/redfish/v1`. On first connection, `metalman`
captures the BMC's TLS certificate fingerprint and pins it in
`status.redfish.certFingerprint`; later requests must match it.

`targetDisk` is optional, and omitting it is a real choice rather than an error. The
installer then selects the largest whole block device it can find and logs a warning. On a
server with a small boot device and a large data disk, that heuristic picks the wrong
disk, so set `targetDisk` whenever the host has more than one candidate.

`spec.kubernetes` is how you pass the Kubernetes version, node labels, and registration
taints to kubelet. The agent pins the node name to `spec.kubernetes.nodeRef.name` when it
is set, otherwise to the `Machine` name. `metalman` writes that name into the agent
configuration it serves, so editing `nodeRef` after provisioning does not change which
node the issued credential is allowed to claim.

### Virtual Media

Virtual media is the third boot protocol, alongside PXE and UEFI HTTP boot. Instead of
relying on DHCP and TFTP, `metalman` mounts a small bootstrap ISO through Redfish and
tells the BMC to boot from it. The ISO belongs to the `MachineOperation`, not to a shared
server directory, and the installer it contains fetches the rest of its instructions from
`metalman` over HTTP.

This path exists because some hosts cannot use the normal network boot reliably. A link
that only passes traffic after LACP negotiates is the common case: firmware PXE may time
out before the bond is up. The virtual media installer boots without network access,
brings up the declared host network itself, and only then contacts `metalman`. It is also
useful when a rack manager stages media for many machines and you do not want a TFTP
server on the provisioning segment.

Virtual media has stricter requirements than PXE. `bootProtocol: VirtualMedia` requires
a `redfish` block, the machine image must be pinned by digest, and the netboot image must
also be pinned by digest. `spec.host.netboot.network`, the static ethernet, bond, and
VLAN description, requires virtual media as well, because the installer has to bring up
that topology before it can reach the controller. When `network` is set, the first
`dhcpLeases` entry remains the provisioning identity: its address and gateway must be
configured on exactly one declared interface, and its MAC must appear in the ethernet
list. `holdLease` is not supported in that mode.

The controller records media ownership before it changes the BMC. The operation target's
`status.bootMedia` carries the media ID, the source server and name, the machine UID, a
copy of the netboot spec, the certificate fingerprint, and flags for image written,
cleaned, released, and abandoned. That state is what lets a retry clean up media it
created rather than adopting a stranger's ISO. The installer reports completion and then
waits for the controller to detach the media and clear the boot override before it
reboots, so a failed cleanup cannot cause a second install.

Two flags matter for virtual media deployments. `--serve-url` must be an address the BMC
can reach, because the BMC downloads the ISO from it. `--media-tftp-server` advertises an
IPv4 address for rack-manager TFTP pulls, which is separate from the host's own boot
path. Media serving also rechecks ownership on downloads; range requests and `HEAD` are
supported so a BMC that probes before downloading works correctly. Finally, account for
BMC virtual media licenses: some hosts require one before media mounting works, and the
failure looks like a mount request that never completes.

### Site Isolation

A site is the unit of ownership for bare metal. Each `Site` that enables the metalman
component gets its own Deployment and its own leader-election lease, and it manages the
machines labeled for that site. The `--site` value is matched against the
`unbounded-cloud.io/site` label on `Machine` resources, which is also the canonical site
label that the network controller applies to `Node` objects.

The scoping rule has one subtle case. An instance started without `--site` does not mean
"all sites"; it means "machines with no site label". That is useful for a small
installation that does not use sites, but it also means you cannot run one unscoped
instance and one scoped instance and expect them to divide the cluster cleanly. Pick one
model per cluster. If you label a machine for a site, only that site's controller owns
it.

Ownership is enforced again when an operation runs. A `MachineOperation` is executed by
the instance that owns its target `Machine`, and operations that target an unowned machine
are left alone. Operations that could interfere with each other are serialized: the
controller treats a machine identity and a Redfish endpoint as conflict keys, so it will
not run two host operations against the same BMC at the same time, even if two
`MachineOperation` objects were created for different reasons. An older conflicting
operation wins, and the newer one waits in `Pending`.

The practical consequence is that a rack with its own provisioning network can have its
own failure domain. If the `metalman` Deployment for rack A is down, rack B is unaffected,
and the `MachineOperation` objects for rack A stay pending rather than being executed by
the wrong controller.

### TPM 2.0 Attestation

Bootstrap tokens are credentials. Delivering one to a host that merely claims to be a
machine would let an attacker join the cluster by spoofing an address. Bare metal solves
that with the Trusted Platform Module (TPM) 2.0: the token is encrypted so that only the
host with the matching endorsement key can decrypt it.

The exchange is one HTTP round trip to `POST /attest` on the HTTP server. The host sends
its endorsement key and storage root key public keys. `metalman` looks up the `Machine`
by the direct TCP peer address, takes a short-lived ServiceAccount token for the
`metalman-bootstrap` service account, encrypts a random AES-256 key to the endorsement
key with `MakeCredential`, encrypts the token with AES-256-GCM, and returns both along
with the cluster CA. The host recovers the AES key with `ActivateCredential`, decrypts
the token, and uses it for kubelet TLS bootstrap. The token lifetime is one hour.

Trust on first use is the identity anchor. The first endorsement key seen for a machine
is written to `status.tpm.ekPublicKey`. Later attestations must present the same key; a
different key is rejected with 403 until an operator clears the status field. That is what
you do after replacing a motherboard or a TPM, and it is also why replacing a whole host
under an existing `Machine` name needs a deliberate status reset rather than a silent
adoption.

The token is scoped to its `Machine` in two ways. Before returning it, `metalman` records
the credential ID in `status.bootstrap.credentialID`, the node name in
`status.bootstrap.nodeName`, and the issue time in `status.bootstrap.issuedAt`. The
recorded credential ID is what the certificate approver compares against. The node name
recorded at issue time is also the name written into the agent configuration, so later
edits to `spec.kubernetes.nodeRef` do not widen the credential.

Approval is part of the controller, not an external admission webhook. `metalman`
approves kubelet client certificate requests from `metalman-bootstrap` only when the
request's `authentication.kubernetes.io/credential-id` extra matches exactly one
`Machine`'s recorded credential and the common name is `system:node:<nodeName>` for that
machine. Otherwise it denies the request. Each instance decides only requests whose
credential is recorded on a machine it owns and leaves the rest pending, so a machine
does not get approved by the wrong site. The Kubernetes controller manager does not
auto-approve these requests; the CSRs depend on the approver.

A ValidatingAdmissionPolicy limits what the bootstrap service account can do. It may
update only its own machine's status, and it may not modify `status.tpm` or
`status.bootstrap`. It cannot create machines. That keeps a compromised bootstrap
credential from rewriting the trust anchor that authorized it.

Two preconditions are easy to miss. ServiceAccount tokens must carry a `jti`, which
requires Kubernetes 1.30 or later; on an older cluster attestation fails rather than
falling back to an unverified token. And the request must arrive directly from the host.
Because `metalman` uses the peer address and ignores forwarding headers, it is not
supported behind an HTTP proxy that rewrites the client address.

### MachineOperation-Based Operations

Day-2 actions are `MachineOperation` resources. They are cluster scoped, have the short
name `mop`, and behave like a Job: they carry a phase, a message, timestamps, and an
optional time to live after completion. The spec names one machine with `machineRef` or
selects several with `machineSelector`. For the bare metal controller, the matched
machines become targets in `status.targets`, snapshotted when execution begins so later
selector changes do not alter the work in flight.

For bare metal, the kinds that matter are the out-of-band host operations:

| Kind | Handled by | Effect |
|---|---|---|
| `HostHardReboot` | `metalman` or a cloud controller | Full power cycle through the BMC |
| `HostPowerOff` | `metalman` or a cloud controller | Power the host off |
| `HostPowerOn` | `metalman` or a cloud controller | Power the host on |
| `HostReplace` | `metalman` or a cloud controller | Repave the host and reinstall the agent |

`NodeReboot`, `HostSoftReboot`, `AgentUpgrade`, and `AgentReset` are handled by the agent
on the host instead. `HostAllocate` and `HostRelease` belong to `MachineAllocation` and
are explicitly ignored by `metalman`. The split matters when you debug: a soft reboot
that never completes is an agent problem, while a hard reboot that never completes is a
BMC or `metalman` problem.

An operation moves through the phases `Pending`, `InProgress`, `Complete`, and `Failed`.
Inside an operation, each target has its own phase, stage, message, attempts, and
conditions. For a repave, the stages follow the physical sequence: waiting for power off,
powering on with the boot override set, waiting for the installer to write the image,
waiting for first-boot cloud-init, and waiting for the node. The target conditions record
the milestones that matter: `BootLoaderDownloaded`, `BootImageWritten`, and
`CloudInitDone`. Progress is reported on the operation, not on the `Machine`, which is why
`kubectl get machineoperation` is the place to look during a PXE boot.

## Host Preparation

Host preparation is the work between a network boot and a joined node. It happens in two
stages, and the split is deliberate: the installer runs in memory and writes an image,
then the installed OS runs cloud-init and installs the agent. Nothing in the installer
contains cluster credentials, so a disk image is reusable across clusters.

### What the installer does

The installer is an initramfs overlay, not a general-purpose OS. It starts by loading
storage and network drivers, including a PCI modalias pass so that a RAID controller
missing from the fixed module list is still probed. It then finds the provisioning NIC by
the MAC in the kernel command line, brings it up, and applies the static address, gateway,
and DNS servers. The address comes from the kernel command line on the PXE path, and from
the install instructions on the virtual media path.

Next it resolves the target disk. If `targetDisk` is set, the installer waits for that
device to appear, including by-id paths, which it recreates from sysfs because the
installer has no udev. If it is not set, the installer selects the largest whole block
device it can find and logs a warning. It then downloads the machine image over HTTP,
decompresses it, and writes it with `dd`. The download is retried up to 120 times with a
delay, which covers a slow registry pull behind the cache.

After writing, the installer mounts the new root filesystem and prepares the first boot.
It writes a NoCloud datasource configuration that points at `metalman`, disables
cloud-init's own network rendering so a fallback DHCP configuration cannot conflict, and
persists the static network configuration as netplan or systemd-networkd depending on
what the image uses. It also writes `/etc/unbounded-metal/config` with the serve URL and
node identity. On UEFI systems it creates a boot entry for the installed loader when
`efibootmgr` is available; otherwise the GRUB chainload path remains the fallback.

Finally, the installer signals `/pxe/disable` to clear the network boot override and
reboots. For virtual media it first reports the image-written state and waits for the
controller to release the ISO and its boot override, so the host cannot reboot into the
installer twice.

### What the first boot does

The installed image's cloud-init runs vendor data supplied by `metalman`. The vendor data
creates a locked local user, writes `/etc/unbounded/agent/config.json` and the agent
install script, and runs the script. The agent configuration contains the attestation URL
and the cluster settings; the script downloads the agent release and starts it. Vendor
data also installs package-manager protections that keep unattended upgrades from
restarting `systemd-machined` and tearing down the running node, and it disables cloud-init
at the end so first-boot work runs once.

The agent then prepares the node stack. It installs host packages, downloads the
container runtime and Kubernetes artifacts, creates the isolation environment, starts
containerd and kubelet, and bootstraps TLS with the attested token. `spec.isolation`
selects where that stack runs: `nspawn` is the default for a new host, `none` runs it
directly on the host, and `container` is experimental and must be enabled explicitly.
The mode is applied at provisioning time; changing it does not move a running node until
its next repave. See [agent isolation]({{< relref "reference/agent/isolation" >}}) for the
tradeoffs.

### Static networking and held leases

The static network configuration is written before the installed system boots, and it is
keyed by MAC rather than by interface name. That avoids the usual `eth0` versus `eno1`
mismatch: the image and the installer do not have to agree on kernel naming, because the
match is on the hardware address.

`holdLease` handles a conflict that appears when a site also runs DHCP for installed
hosts. Without it, the installed OS gets no DHCP configuration from the installer and
uses only the static address. If your site DHCP server hands that address to another
host, however, the reservation is not protected. Setting `holdLease: true` makes the
installed host run a DHCPv4 client on the provisioning NIC, identify with the MAC, and
keep the lease while ignoring routes, DNS, NTP, hostname, and domain values from DHCP.
The static address, gateway, and DNS remain authoritative. `metalman` answers only boot
firmware for that lease, so the installed OS talks to the site DHCP server instead.

For bonds and VLANs, use `spec.host.netboot.network` with `bootProtocol: VirtualMedia`.
The installer brings up the declared topology before it contacts the controller, persists
it as netplan or systemd-networkd configuration, and validates cross-references: bond
members must be declared ethernets, VLAN links must reference a declared ethernet or
bond, and at most one interface may carry the default gateway. Addressing is IPv4 only.
If the topology depends on LACP negotiation, virtual media is the only path that works,
because firmware PXE would time out before the link passes traffic.

## The Machine Lifecycle

A bare metal `Machine` does not have a single controller advancing a phase field the way
an SSH machine does. Instead, the declaration is static and the work is expressed as
operations. That is the most important lifecycle difference to internalize: the machine
describes a host that should exist, and each `HostReplace` describes one attempt to make
the host match that description.

### Creating a Machine

Creating a `Machine` does not by itself boot anything. It registers the lease, the image,
and the optional BMC credentials. If the host already has network boot enabled, the next
power-on can use the provisioning server. If not, you have two choices: enable network
boot once in the firmware or boot menu, or create an operation that asks `metalman` to do
it through Redfish.

The minimal first install therefore looks like this: apply a `Machine` with a lease and an
image, then either boot the host from the network manually or give the machine Redfish
access and repave it. In both cases the installer writes the image, the first boot installs
the agent, and the node joins. A machine without Redfish is still fully usable; it just
cannot be reimaged without physical access.

`metalman` does not write `status.phase` on the `Machine`. That field is driven by
`machina` for SSH-provisioned machines, and a PXE machine normally leaves it empty.
Progress for a bootstrap or repave is on the `MachineOperation` that requested it. The
agent still writes its own status on the machine, including the `AgentBootstrapped`
condition and the applied isolation mode, so `Machine.status.conditions` remains useful
after the node is up.

### Repaving and rebooting

Three commands cover the common day-2 actions. They all create `MachineOperation`
resources, and all of them can be watched to completion:

```bash
# Reimage the host through a HostReplace operation.
kubectl unbounded machine repave node-01

# Hard power cycle through the BMC.
kubectl unbounded machine reboot node-01

# Reboot the host OS from inside the host, no BMC required.
kubectl unbounded machine soft-reboot node-01
```

`machine repave` creates a `HostReplace` operation. With Redfish, `metalman` forces the
host off, configures the boot override for the selected protocol, powers it on, waits for
the installer, waits for first-boot cloud-init, and completes when the expected `Node`
exists. Without Redfish, the controller cannot obtain a power client, so the operation
cannot complete; boot the host from the network manually instead.

`machine reboot` is a `HostHardReboot` and requires a Redfish configuration; the command
fails early with an explicit message when there is none. `machine soft-reboot` creates a
`HostSoftReboot`, which the agent executes with `systemctl reboot` and reports complete
only after it starts again on the new boot. The agent will not start a soft reboot while
an out-of-band host operation targeting the same machine is unfinished, which prevents a
power race during a repave.

A repave is destructive by definition: it writes the machine image over the target disk.
The `Machine` name, lease, and BMC settings survive, but anything the previous OS wrote
on that disk does not. If you need to preserve data, that data has to live somewhere else.
For configuration changes that do not require a new disk image, prefer a new
`MachineConfigurationVersion` and the normal node repave flow rather than a host repave.

### Watching progress

Operators usually want one of three answers: is the host powered, has the install started,
and has the node joined. The operation answers all three if you read the target stage and
its conditions.

```bash
kubectl get machineoperation node-01-repave-1700000000 -o yaml
```

The name is generated as `<machine>-repave-<unix timestamp>`, so tab completion or
`kubectl get machineoperation` is easier than typing it. The command that created the
operation also watches it and prints phase changes until it reaches a terminal state,
which is usually enough for an interactive repave.

| Stage | What it means | What to check when it stalls |
|---|---|---|
| `PoweringOff`, `WaitingOff` | The controller asked the BMC to force the host off | BMC reachability, credentials, power state |
| `PoweringOn`, `WaitingOn` | The boot override is set and the host is starting | Redfish boot override support, firmware boot order |
| `RepaveRequested`, `WaitingRepave` | The host should be running the installer | DHCP or boot URL, TFTP, HTTP port 8880, console output |
| `WaitingCloudInit` | The image is written and the host rebooted | First-boot cloud-init log, image contents, network |
| `WaitingNode` | Cloud-init finished and the node should register | Attestation, CSR approval, kubelet logs, API reachability |

When the stage is `WaitingRepave`, the host has not reported that it wrote the image. That
is a boot problem, not a Kubernetes problem. Check that the firmware actually booted the
network device, that the DHCP offer reached it, and that the HTTP server is reachable from
the provisioning address. When the stage is `WaitingNode`, the disk was written and the
host is running the installed OS; the problem is now attestation, certificate approval, or
kubelet configuration.

### Replacing hardware

Replacing a failed host under the same `Machine` name is possible, but it requires
clearing hardware-bound state. A new TPM presents a different endorsement key, so
attestation returns 403 until `status.tpm.ekPublicKey` is cleared. A new BMC presents a
different TLS certificate, so Redfish calls fail until `status.redfish.certFingerprint` is
cleared or the new fingerprint is pinned. If the replacement has different NICs, update
`dhcpLeases`; otherwise the DHCP match will not find the machine.

If the replacement is different hardware with a different role, creating a new `Machine`
and deleting the old one is usually cleaner than editing identity fields in place. The
`Machine` name is also the default node name, so reusing a name keeps the node identity
stable, which is what you want when you are replacing a failed unit rather than
reassigning capacity.

## How Bare Metal Differs from Cloud Virtual Machines

Both paths end with a `Machine` and a `Node`, and both install the same agent. The
differences are in where capacity comes from, what identity means, and what can fail.

A cloud virtual machine is created by an API call. The provider owns the hardware, the
firmware, the network, and the disk, and it exposes an instance identifier that the
cluster can store. Boot is usually a provider image plus cloud-init from a metadata
service. Power operations are provider API calls, and destroying the instance is the
cleanup path.

A bare metal host already exists. Nobody creates it in response to a pending pod, and
there is no provider-side identifier to store. Identity comes from the physical NIC and
the TPM, the OS arrives as a disk image over the provisioning network, and power is
controlled through a BMC that may be on a separate, less reliable network. Cleanup means
reimaging or decommissioning hardware, not deleting an API object.

| Concern | Bare metal | Cloud virtual machine |
|---|---|---|
| Capacity source | Hardware you rack or lease | Provider API, often through Karpenter and a `MachineClass` |
| API field | `spec.host.netboot` | `spec.host.azure` or `spec.host.external` |
| Identity | Lease MAC, TPM endorsement key, BMC fingerprint | Provider instance or resource ID |
| Boot media | Netboot image plus a machine disk image | Provider image plus cloud-init user data |
| Power control | Redfish BMC | Provider API |
| Provisioning transport | DHCP, TFTP, HTTP on the site network | Provider metadata and cloud-init |
| Token source | TPM attestation against `metalman` | Bootstrap token from the allocation |
| Typical failure | Firmware, cabling, BMC, disk selection | API quota, image, IAM, provider outage |
| Scaling model | One `Machine` per host, declared by an operator | Allocation-driven, elastic |

The failure modes follow from those differences. On bare metal, the most common failures
are below Kubernetes: a NIC that does not PXE boot, a BMC license that blocks virtual
media, a disk that is not the one you meant, a link that needs LACP before it passes
traffic, or firmware POST that takes longer than the first-boot budget. In the cloud, the
common failures are above the host: a quota, a missing image, a provider API error, or a
credential that cannot create instances. That is why the two paths have different
observability: bare metal leans on the `MachineOperation` stage and the host console,
while the cloud path leans on the allocation and the provider adapter.

One consequence is that bare metal favors a known, curated image. The machine image is a
complete disk, so the OS, drivers, and filesystem layout are under your control and are
identical across hosts. The cloud path favors a provider image plus first-boot
configuration, because the provider controls the disk format. Neither is better in the
abstract; they match different ownership boundaries.

## Operating Limits and Troubleshooting

A few limits are worth knowing before you depend on the path:

- `metalman` must see the host's real source address. A proxy that rewrites the client
  address breaks attestation, and forwarding headers are ignored by design.
- TPM attestation requires Kubernetes 1.30 or later, because the token must carry a `jti`
  that the controller records and the approver checks.
- The first-boot budget defaults to 20 minutes and starts at the installer reboot
  handoff. Large GPU servers can spend most of that in firmware POST. Raise
  `cloudInitTimeout` when that is the case, rather than treating a slow POST as a failed
  install.
- The static host network is IPv4 only, requires `bootProtocol: VirtualMedia`, requires a
  first lease for the provisioning identity, and does not support `holdLease` in that
  mode.
- UEFI HTTP boot depends on firmware and BMC conformance. Applying the Redfish settings
  and fetching the first EFI binary are hardware responsibilities; a BMC that ignores the
  boot URL will not be fixed by the controller.
- A BMC may require a license for virtual media. If the ISO mount never completes, check
  the license before debugging the network.
- Site ownership is strict. A machine labeled for one site is ignored by every other
  site's controller, including the unscoped instance.

Start troubleshooting with the resource that owns the failing step. For a boot or power
problem that is the `MachineOperation` target and the `metalman` logs. For a node that
booted but did not join, it is attestation, the CSR approver, and kubelet on the host. For
networking after the join, it is the site and the network controller. The
[host operations guide]({{< relref "guides/operations/host-operations" >}}) covers the
day-2 commands in more detail, and the [PXE guide]({{< relref "guides/pxe" >}}) walks
through a full install with example manifests.

## Next Steps

If you are provisioning your first bare metal host, start with the
[PXE guide]({{< relref "guides/pxe" >}}), which covers image preparation, the `Machine`
manifest, and the boot sequence end to end. If you already have a host with an OS, the
[SSH guide]({{< relref "guides/ssh" >}}) is faster and does not need a provisioning
network.

- For every `Machine` and `MachineOperation` field, see the
  [CRD reference]({{< relref "reference/machina-crd" >}}).
- For the agent that runs after the first boot, see the
  [agent reference]({{< relref "reference/agent" >}}) and its
  [configuration]({{< relref "reference/agent/configuration" >}}).
- For power, repave, and reboot procedures, see
  [host operations]({{< relref "guides/operations/host-operations" >}}).
- For how bare metal fits with networking and other capacity sources, see the
  [overview]({{< relref "concepts/overview" >}}) and the
  [architecture reference]({{< relref "reference/architecture" >}}).
- For automatic capacity from a provider API, see
  [cloud API provisioning]({{< relref "guides/cloud-api" >}}).
