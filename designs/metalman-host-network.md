# Metalman declarative host networking (bonds and VLANs)

## Problem

Metalman provisions bare-metal hosts over a single NIC: DHCP/TFTP for PXE,
Redfish-configured UEFI HTTP boot, or a VirtualMedia ISO whose GRUB script
brings up one NIC by MAC (`efinet`) and fetches `grub.cfg` over HTTP before
Linux starts. Hosts whose only uplinks are LACP (802.3ad) bond members cannot
use any of these paths unless the switch falls back to an individual port when
no LACPDUs arrive, and many switch configurations do not. The installed host
also only persists a single-NIC netplan or networkd file, so bonds and VLANs
have to be configured out of band.

## API

`PXESpec.network` (`spec.host.netboot.network`, also frozen into
`MachineOperation` snapshots) declares the host topology:

```yaml
network:
  ethernets:
  - name: enp1s0f0          # persisted interface name
    macAddress: 02:00:00:00:00:01   # permanent MAC, the identity used for matching
  - name: enp1s0f1
    macAddress: 02:00:00:00:00:02
  bonds:
  - name: bond0
    interfaces: [enp1s0f0, enp1s0f1]
    mode: 802.3ad           # default; also active-backup, balance-xor
    miiMonitorInterval: 100 # ms, default 100
    transmitHashPolicy: layer3+4
    mtu: 1500
    addresses: [192.0.2.10/24]
    gateway: 192.0.2.1
  vlans:
  - name: bond0.64
    id: 64
    link: bond0
    addresses: [198.51.100.10/24]
    routes:
    - to: 203.0.113.0/24
      via: 198.51.100.1
  dns: [192.0.2.53]
```

Rules enforced by CRD CEL:

- `network` requires `bootProtocol: VirtualMedia` and at least one `dhcpLeases`
  entry.
- `network` rejects `dhcpLeases[0].holdLease: true`. Lease holding only
  applies to the single-NIC config; the declared network is authoritative and
  does not run DHCP. Go validation enforces the same rule.
- Bond members must name declared ethernets; VLAN links must name a declared
  ethernet or bond; `lacpRate` requires 802.3ad.

The bond-member and VLAN-link rules are declared on `MachineSpec` (for
`spec.pxe`) and `HostSpec` (for `spec.host.netboot`), not on
`HostNetworkSpec`. `PXESpec` is also embedded in the MachineOperation
`status.targets[].bootMedia.netboot` snapshot; `targets` has no `maxItems`, so
nested list rules there exceed the API server's CEL cost budget and the CRD
would be rejected. The snapshot is copied from a validated Machine and keeps
the per-field schema and the `PXESpec` rules.

Rules enforced by metalman before it builds media (`hostnetwork.Validate`); a
violation fails the operation with a message naming the field:

- Interface names are unique across ethernets, bonds, and VLANs; MACs are
  unique, lowercase, colon-separated.
- Interface names may not be `lo`, `all`, `default`, or `bonding_masters`,
  which collide with the loopback device, the per-protocol `conf/all` and
  `conf/default` sysctl entries, and the bonding driver control file.
- An ethernet belongs to at most one bond; bond members carry no addresses,
  gateway, or routes, and are not VLAN links.
- VLAN links are ethernets or bonds (no VLAN-on-VLAN); `(link, id)` is unique.
- Addresses are IPv4 CIDRs with a global unicast host address, prefix 1-30,
  not the network or broadcast address, unique across the host.
- At most one interface sets `gateway`; it lies inside one of that
  interface's subnets. Routes need a canonical non-default destination prefix
  and a next hop inside one of the interface's subnets.
- `transmitHashPolicy` is only valid for 802.3ad and balance-xor.
- A VLAN MTU does not exceed its link's effective MTU; an unset ethernet or
  bond MTU counts as the kernel default of 1500.
- A bond member that sets an MTU requires the bond to set the same MTU;
  enslaving resets the member MTU to the bond MTU.
- `dhcpLeases[0]` stays the provisioning identity. Its MAC is one of the
  ethernets underlying the interface that carries its IPv4 address; that
  address (with its subnet mask) is declared on exactly one interface, as the
  first entry in that interface's `addresses`, so the kernel uses it as the
  source for installer requests to Metalman; its
  gateway equals the declared gateway, which sits on the same interface.
- Next hops are not host addresses or a subnet's network/broadcast address;
  route destinations are unique per interface; `dns` holds at most three
  unique canonical unicast IPv4 addresses.

`dhcpLeases[0]` remains the key for every existing IP-based authorization
check (bootmedia callbacks, first-boot attestation, artifact ownership, the
machine IP index). The operator must choose the address the host uses to
reach metalman.

## Boot flow with `network`

The legacy VirtualMedia flow (no `network`) is unchanged.

1. Metalman builds an operation-owned ISO (`mediaISOFormat`
   `unbounded-uefi-local-v1`). The netboot image must advertise
   `localInstaller: unbounded-local-installer-v1` in `metadata.yaml`; older
   images fail fast. The ISO9660 tree holds:
   - `/unbounded/<mediaID>`: empty marker used by GRUB `search`.
   - `/unbounded/vmlinuz`, `/unbounded/initrd`, `/unbounded/init.cpio`: copied
     from the netboot image (`vmlinuz`, `initrd`, `init.cpio`).
   - `/unbounded/media.cpio`: per-operation newc archive (below).

   The media identity hash uses `unbounded-uefi-local-v1` in place of the
   legacy ISO format, so local and legacy media never share cached bytes.
   Metalman also runs `hostnetwork.Validate` and checks that the serve URL is
   plain `http://` before it builds the ISO.
2. The ESP GRUB config boots locally, with no network access:

   ```
   search --no-floppy --file --set=root /unbounded/<mediaID>
   linux /unbounded/vmlinuz unbounded.install_url=<serveURL>/bootmedia/<mediaID> [console args]
   initrd /unbounded/initrd /unbounded/init.cpio /unbounded/media.cpio
   boot
   ```
3. The installer (`images/netboot/assets/init`) sees `unbounded.install_url`,
   brings up the runtime network from `/unbounded/media/network`, waits for a
   carrier on the provisioning interface, and fetches
   `GET <install_url>/install`.
4. Metalman authorizes the request exactly like the legacy `grub.cfg`
   callback: the media ID resolves to a frozen operation snapshot, the request
   source IP equals frozen `dhcpLeases[0].ipv4`, the live operation still owns
   the media and is in `PoweringOn` or `Installing`, and `LoaderStartedAt` is
   recorded with an optimistic lock before instructions are returned. Media
   built without `network` answer 404. As with every bootmedia callback, an
   unknown media ID or a wrong source IP also answers 404, and a non-GET
   request answers 405. A transient failure to look up the media owner
   (API server errors other than not found) answers 503 on every callback,
   and callers retry. Media built with `network` answer 404 to `grub.cfg`,
   so a network installer config is never served for them.
5. The installer writes the image, persists networking, and uses the existing
   `complete` and `ready` callbacks.

### `media.cpio`

Deterministic newc archive (uid/gid 0, mtime 0, directories 0755, files
0644):

- `unbounded/media/network`: runtime description (below).
- `unbounded/media/netplan.yaml`: persisted as
  `/etc/netplan/99-unbounded-metal.yaml` (mode 0600).
- `unbounded/media/networkd/*`: `.link`, `.netdev`, `.network` files persisted
  into `/etc/systemd/network/`.

The installer keeps the legacy persistence choice: if the image has no
`/etc/netplan` but has `/etc/systemd/network`, it installs the networkd files
and removes `90-unbounded-dhcp.network`; otherwise it installs the netplan file
and removes `50-cloud-init.yaml`.

### Runtime description

One directive per line, space-separated tokens, `#` starts a comment. Every
token matches `^[a-zA-Z0-9_.:/,+-]+$`. Directives appear in this order:

```
ethernet <name> <mac> <mtu|->
bond <name> <mode> <miimon> <xmit_hash_policy|-> <lacp_rate|-> <mtu|-> <member>[,<member>...]
vlan <name> <link> <id> <mtu|->
address <iface> <ipv4>/<prefix>
route <iface> default|<dest>/<prefix> <via>
dns <ipv4>
source <iface> <ipv4>
```

`source` names the interface and address that carry `dhcpLeases[0]`. Names
refer to declared interface names; the installer maps ethernets to kernel
devices by permanent MAC and does not depend on kernel naming.

### Install instructions

`GET /bootmedia/<id>/install` returns `200 text/plain` with `KEY=VALUE`
lines. Keys: `IMAGE_URL`, `SERVE_URL`, `DS_URL`, `NODE_NAME`,
`NODE_NAMESPACE`, `APISERVER_URL`, `MEDIA_CALLBACK`, and optional `DISK`.
Values match `^[a-zA-Z0-9_.:/@+-]*$`; metalman refuses to serve anything else
and the installer re-validates and ignores unknown keys. A `409 Conflict`
means metalman refuses this boot (stale media, wrong phase); the installer
powers off. Transient ownership lookup errors (API server failures other
than not found) return `503 Service Unavailable`. Other failures, including
503, are retried.

## Persistent rendering

- Netplan: ethernets matched by `macaddress` with `set-name`, bonds with
  `parameters` (`mode`, `mii-monitor-interval`, `transmit-hash-policy`,
  `lacp-rate`), VLANs with `id`/`link`, `routes` including `to: default` for
  the gateway, and `nameservers` on the interface that carries the gateway.
- systemd-networkd: `.link` files rename ethernets by `PermanentMACAddress`;
  ethernet `.network` files match `PermanentMACAddress` (bond members share
  the bond's MAC, so `MACAddress` matching is ambiguous) and set `Bond=` or
  `VLAN=`; bonds and VLANs get `.netdev` and `.network` files.

- Interfaces without addresses (bond members, VLAN-only links) disable
  link-local addressing (`link-local: []`, `LinkLocalAddressing=no`).
  Addressed interfaces ignore IPv6 router advertisements (`accept-ra: false`,
  `IPv6AcceptRA=no`), matching the legacy single-NIC networkd file.
  Nameservers go on the gateway interface, or on the first addressed
  interface when no gateway is declared. Bonds get no explicit MAC; networkd
  assigns its stable generated MAC.
- Netplan `match.macaddress` matches the permanent MAC since netplan 0.106
  (Ubuntu 24.04 ships 1.0), so it is unambiguous for bond members there too.
- `hostnetwork.RenderNetplanConfig` emits the same document without the
  `network:` wrapper, matching the NoCloud template's shape.

The NoCloud `network-config` template renders the same netplan document when
`network` is set, for images that let cloud-init own networking.

## Out of scope

- PXE and HTTP boot with `network`: both need a single NIC that passes traffic
  before Linux runs.
- IPv6 and DHCP on declared interfaces.
