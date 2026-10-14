# Metalman: Rack Manager Support and Virtual Media Install

This doc explains how Metalman manages servers behind an OpenBMC-style rack
manager, and how it reimages them by booting a small ISO through the rack manager's
virtual drive. It covers the Redfish client, the operation state machine, the
ISO, the installer, the machine images, and what we learned on real hardware.

For setup steps and the user-facing contract, see the
[PXE guide](../docs/content/guides/pxe.md), sections "Virtual media
(experimental)" and "Machine images". This doc explains the internals.

Line numbers are a guide and may drift as the code changes.

## Background

A rack manager fronts many servers ("slots") and talks to each server's BMC.
Its Redfish API is not standard:

- Each server lives under a path prefix: `https://manager/7/redfish/v1/System`
  is slot 7. Note `System`, not `Systems/{id}`.
- Rack-wide actions live at the root: `/redfish/v1/Managers/RackManager/...`.
- Most responses carry a completion code in an OEM extension. A request can
  return HTTP 200 and still fail.
- There is no session login. Only HTTP basic auth works.

The code avoids vendor names. It finds OEM data and actions by shape, not by
vendor key (see "Detection"). Where documented behavior and tested firmware
differ, the code follows the hardware.

The servers we target get DHCP from a network we do not control, so PXE is not
an option. Instead Metalman builds a per-machine ISO, stages it on
the rack manager, mounts it as a virtual drive, and boots the host from it.

## Design rules

- **No vendor terms in the public API.** The slot is part of `redfish.url`
  (for example `https://manager/28`). Metalman detects a rack manager by itself.
  The only new public value is the generic `bootProtocol: VirtualMedia`.
- **PXE and HTTP boot are unchanged.**
- **Fail closed.** A host must never boot the installer twice by accident, and
  must not reboot before its boot override is restored.
- **Durable state.** All progress is saved on the `MachineOperation`, so a
  controller restart resumes or cleans up correctly.

## Components

| Part | Code | Role |
|---|---|---|
| Rack manager client | `internal/metalman/redfish/rackmanager.go` | Detection, slot paths, completion codes, power, boot override |
| Virtual media client | `internal/metalman/redfish/virtualmedia.go` | Stage, mount, boot, cleanup, delete |
| Shared interfaces | `internal/metalman/bootmedia/types.go` | `Provider` (builds ISOs) and `Client` (talks to the BMC) |
| Operation controller | `internal/metalman/machineops/virtualmedia.go` | Phase state machine, retries, cleanup, finalizer |
| ISO builder | `internal/metalman/netboot/media.go` | Builds and caches the ISO, serves it over TFTP |
| Media HTTP endpoints | `internal/metalman/netboot/media_http.go` | GRUB config, `complete` and `ready` callbacks |
| Installer | `images/netboot/assets/init`, `grub.cfg.tmpl` | Writes the disk, reports back, reboots |
| Netboot image | `images/netboot/Containerfile` | Kernel, initrd, shim, GRUB |
| Machine images | `images/host-azurelinux3*`, `images/host-ubuntu2404` | Disk images to install |

## Rack manager client

### Detection

`Dial` calls `detectRackManager` (`rackmanager.go:50`) only when the URL has a
path. It sends `GET /redfish/v1/Managers/RackManager` to the origin root and
checks:

- `Id` is `RackManager` and `@odata.id` matches.
- `Actions.Oem` has an action whose name ends in `#SystemPower.On` and whose
  target is under the rack manager (`slotPowerOnTarget`, `:122`). The client
  saves that target and uses it for slot power-on. No vendor key is hardcoded.
- The path is a positive slot number, such as `/7`.

If the probe fails for any reason other than the caller's context or a TLS pin
mismatch, Metalman falls back to standard Redfish. So a normal BMC behind a
proxy path still works.

Once confirmed, the client uses a copy of the pinned HTTP client with a
three-minute timeout (`rackmanager.go:112`), because some rack actions block for a
long time (see "Hardware findings").

### Requests and errors

`rackRequest` (`rackmanager.go:144`) checks both the HTTP status and the
completion code. The code can be at the top level or inside an `error` object.
The client reads `CompletionCode` from any key under `Oem`
(`completion.go:21`). A `Success` inside `error` counts as success.

`rackUnavailable` (`rackmanager.go:375`) decides when the slot's BMC is not answering
(for example "failed to query" or "failed to discover"). These errors may turn
into `ErrNotReady` (`rackmanager.go:24`), which tells the controller to wait instead
of using up an attempt.

### Power

- Power state comes from the blade (`GET /{slot}/redfish/v1/System`). If that
  fails, the rack power map (`SystemPower`) is used (`rackmanager.go:206`).
- Power off is `ComputerSystem.Reset` with `ForceOff`. Power on uses `On`.
- If the BMC does not answer and the rack shows the slot off, Metalman sends
  the discovered slot power-on action once and waits up to ten minutes
  (`rackmanager.go:283-370`). It does this only for `Reset(On)` and for clearing the
  boot override, because on tested firmware this action also turns the host on.
  Cleanup and media steps never change power.

### Boot override

The rack only accepts the target: `None`, `Pxe`, `RemoteDrive`, `Hdd`,
`BiosSetup`. The firmware ignores `Once` and always keeps `Continuous`. So
Metalman always sets `Hdd` again after an install.

## Virtual media client

All methods are in `redfish/virtualmedia.go`. Names must match
`ub-<1-32 hex>.iso` and the server must be a literal IPv4 address
(`virtualmedia.go:38`).

| Method | Requests | Notes |
|---|---|---|
| `StageVirtualMedia` (`:53`) | `POST /redfish/v1/Managers/RackManager/Tftp/Actions/Tftp.Get` with `{Server, File}`, for the primer name and then the real name | The rack pulls the file from Metalman over TFTP (UDP 69). Blocks until done. |
| `MountVirtualMedia` (`:82`) | `POST /{slot}/redfish/v1/System/Actions/ComputerSystem.RemoteDriveMount` with `{ImageName}`, primer then real | About 20 seconds each. |
| `SetVirtualMediaBoot` (`:102`) | `PATCH System` with mode `UEFI`, then target `RemoteDrive` | |
| `CleanupVirtualMedia` (`:122`) | `PATCH` target `Hdd`, then `RemoteDriveUnmount` | An unmount error saying support "is not enabled" means nothing is mounted, so it counts as success. |
| `DeleteVirtualMedia` (`:155`) | `Tftp.Delete` for primer and real name | "No such file or directory" counts as success. |

The primer name is `up-<id>.iso` (`bootmedia.PrimerName`, `types.go:23`). It is
the same ISO. See "Why two mounts" below.

## The operation state machine

A `HostReplace` operation on a Machine with `bootProtocol: VirtualMedia` runs
`advanceVirtualMedia` (`machineops/virtualmedia.go:268-452`). State is kept in
`target.bootMedia` on the operation status.

### Start

On the first pass (`:340-358`) the controller:

1. Adds the finalizer `metalman.unbounded-cloud.io/boot-media`.
2. Creates a random ID (16 bytes, 32 hex characters, `newMediaID`, `:456`).
   The ID is a capability: it is the only secret in the callback URLs.
3. Saves a frozen copy of the Machine's netboot settings, the Machine UID, the
   site, the node name, and the BMC certificate fingerprint.

From here on, the operation uses the frozen copy. Later Machine edits do not
change a running install.

### Phases

| Phase | Action | Next |
|---|---|---|
| `Preparing` | Build the ISO, record its name and TFTP server | `Staging` |
| `Staging` | Rack pulls both names over TFTP | `PoweringOff` |
| `PoweringOff` | `ForceOff` if on; wait for off | `Mounting` |
| `Mounting` | Mount primer, then real name | `SettingBoot` |
| `SettingBoot` | Mode `UEFI`, target `RemoteDrive` | `PoweringOn` |
| `PoweringOn` | `Reset(On)`; wait for on | `Installing` |
| `Installing` | Wait for GRUB and the installer | cleanup |
| `Replacing` | Retry path: clean up old media, pick a new ID | `Preparing` |
| `Installed` | Wait for cloud-init and the Node | complete |

Each phase has the normal five-minute power-action timeout. `Installing` has
its own one-hour timeout (`mediaInstallTimeout`, `:591`). BMC "not ready" errors
get a fixed ten-minute grace per phase that does not reset on retries.

### Did the host boot the media?

When GRUB on the ISO first fetches its config, the HTTP handler saves
`loaderStartedAt` before it answers (`media_http.go:199-214`). This is the
proof that the host booted the media.

If `loaderStartedAt` is still empty ten minutes after power on
(`mediaBootTimeout`, `:583`), the controller moves to `Replacing`
(`:297-315`): it restores `Hdd`, unmounts, deletes the staged files, releases
the old ISO, creates a new ID, and starts again from `Preparing`. A new name is
needed because the firmware can keep a broken drive for a given name. After
three boots in total (`mediaMaxBootAttempts`, `:35`) the target fails.
`bootAttempts` counts the retries.

If the HTTP handler sets `loaderStartedAt` at the same time the controller
starts a retry, `mergeMediaCallbacks` (`:543-577`) keeps the loader start and
drops the retry.

### Cleanup and the reboot barrier

`reconcileMediaCleanup` (`:122-197`) runs on every reconcile, before conflict
checks and TTL. It starts when the installer reports the disk is written, or
when the target or operation ends, or when the operation is being deleted.
Steps, each saved before the next:

1. **Cleaned**: restore `Hdd` and unmount. Sets `cleaned` and `cleanedAt`.
   The host is now safe to reboot.
2. **StagedDeleted**: delete both files from the rack.
3. **Handoff**: if the installer started, wait for it to collect `ready`.
4. **Released**: delete the local ISO cache.

The finalizer is removed only when no media work is left (`:188`).

Rack file deletion is separate from step 1. A rack that will not delete a file
keeps ownership but does not block a safe reboot.

### Ownership and conflicts

While media work is pending, the operation still blocks other operations on
the same Machine or BMC, even if it is terminal. This prevents a new operation
from fighting over the boot override or virtual drive.

For a started install, ownership lasts until cloud-init reports success or
failure (`cloudInitReported`, `:69`). Either result proves the host booted from
disk. This keeps `ready` available if its HTTP response was lost.

Ownership also ends in two more cases, so a machine is never locked forever:

- The host never booted the media: ownership ends after cleanup.
- The installer started but cloud-init never reported: ownership ends one
  install timeout after cleanup or handoff, and `abandoned` is set (`:218`).

## The ISO

`MediaProvider` in `netboot/media.go` builds one ISO per operation ID.

### Inputs

- Both `image` and `netbootImage` must be pinned by digest (`media.go:270`).
- The netboot image metadata must declare `mediaFormat: unbounded-uefi-v1` and
  name its shim and GRUB files (`media.go:294`).
- The first DHCP lease must have a lowercase MAC, IPv4 address, mask, and a
  gateway in the subnet (`validateMediaLease`, `:234`).
- `--serve-url` must be plain HTTP with an IPv4 host and no path.

The cache key includes the ISO format version (`unbounded-uefi-hybrid-v2`,
`:36`), so old ISOs are not reused after a format change.

### Layout

`buildISO` (`:389`):

1. Makes an 8 MiB FAT16 image (`mkfs.vfat --invariant -F 16 -s 1`, `:416`).
2. Copies shim as `EFI/BOOT/BOOTAA64.EFI` (or `BOOTX64.EFI`), GRUB next to it,
   and the bootstrap `grub.cfg` into every place GRUB might look.
3. Runs `xorriso` (`:461`) to make a hybrid ISO. The FAT image is both the El
   Torito boot image and GPT partition 2 (type EFI system).

The hybrid layout matters: the rack presents the image as a USB disk, not a
CD. UEFI boots a USB disk from its GPT EFI partition.

The build is deterministic. The same inputs give the same bytes.

### Bootstrap GRUB config

`bootstrapConfig` (`:356-387`) runs inside GRUB on the ISO and has no DHCP:

1. Load `efinet`, `net`, `http`.
2. Find the NIC whose MAC matches the lease. GRUB's interface names do not
   follow the lease, so it probes `efinet0` to `efinet63` by adding a temporary
   address and comparing `$net_ubprobe_mac`. If none match, it halts.
3. Set the static address, replace GRUB's guessed local route with the real
   subnet, and add the default gateway.
4. Fetch `(http,HOST:PORT)/bootmedia/<id>/grub.cfg`. Retry ten times, six
   seconds apart, then halt.

The ISO holds no kernel, initrd, or install intent. Everything else comes over
HTTP, so the server decides at fetch time whether an install is still allowed.

### Serving

- **TFTP** (`tftp.go:68-81`): requests for `ub-<id>.iso` or `up-<id>.iso` are
  served before the normal source-IP lookup, because the requester is the rack
  manager, not the host. The name is the capability.
- **HTTP** (`http.go:70-71`, `media_http.go:135-267`):

| Path | Method | Behavior |
|---|---|---|
| `/bootmedia/<id>/grub.cfg` | GET | Sends `halt` unless the install is still allowed and the phase is `PoweringOn` or `Installing`. Otherwise saves `loaderStartedAt`, then sends the install menu. |
| `/bootmedia/<id>/complete` | POST | Saves `imageWritten`. |
| `/bootmedia/<id>/ready` | GET | Returns 503 until `cleaned`. Then saves `rebootRequestedAt` and returns 200. |

All three require the request to come from the first lease's IP. Kernel,
initrd, `init.cpio`, and disk image downloads use the normal artifact handler,
which serves the frozen images for the IP (`http.go:162-183`). It uses
`http.ServeFile`, so range requests work. GRUB needs them to load a gzip arm64
kernel.

The install menu (`grub.cfg.tmpl`) loads the kernel and initrd from
`(http,HOST:PORT)` and adds `virtual_media=1` and the callback URL. It does not
set `console=ttyS0` on arm64, so the kernel uses the firmware's console (SPCR).

## The installer

The installer is the Ubuntu netboot initrd plus our `init.cpio` overlay. It
runs on the Azure Linux kernel (see "Netboot image"). Main steps in
`images/netboot/assets/init`:

1. Load storage and network drivers, and create `/dev/disk/by-id` links itself,
   since there is no udev (`link_disk_ids`, `:100`).
2. Find the NIC by MAC, with retries (`:218`), and set the static address.
3. Wait up to 30 seconds for the target disk to appear (`:321`).
4. Download `disk.img.gz` and write it with `dd`.
5. Mount the new root and write network config:
   - If the image has `/etc/netplan`, write a netplan file.
   - Else if it has `/etc/systemd/network`, write
     `10-unbounded-metal.network` and remove the image's default DHCP file
     (`:371-394`).
6. Write the cloud-init NoCloud seed URL, and add a UEFI boot entry.
7. With virtual media: POST `complete`, then poll `ready` until it returns 200
   (`:494-512`). On errors it keeps retrying in RAM. It never drops to a shell,
   reboots early, or writes the disk again.
8. `reboot -f`.

## Netboot image

`images/netboot/Containerfile`:

- The installer kernel comes from Azure Linux 3.0 (`:6-10`). The Ubuntu generic
  kernel has no driver for the MANA NIC (upstream `mana` driver) on the tested
  hosts.
- Azure Linux ships compressed modules. The build decompresses them and runs
  `depmod` (`:35-36`), so the Ubuntu initrd's tools can load them.
- `init.cpio` copies network, storage, file system, crypto, and lib modules,
  plus every module they depend on from `modules.dep` (`:66-109`). Before this,
  `mlx5_core` and `xfs` failed with unknown symbols.
- Shim, GRUB, and the base initrd still come from Ubuntu. Secure Boot does not
  work for the installer, because Ubuntu's shim will not trust the Azure Linux
  kernel. The tested hosts have Secure Boot off.

## Machine images

| Image | Contents |
|---|---|
| `host-ubuntu2404` | Ubuntu 24.04 cloud image. No MANA driver, so it has no network on hosts that use a MANA NIC. |
| `host-azurelinux3` | Azure Linux 3.0, generic for amd64 and arm64. |
| `host-azurelinux3-nvidia` | Same, with the HWE kernel and NVIDIA open driver. |

The Azure Linux images are built in two stages:

- `assets/rootfs.sh` runs in an Azure Linux container. It installs the kernel,
  shim, GRUB, systemd-networkd, resolved, cloud-init, SSH, and tools. It builds
  a generic initramfs with dracut, enables services, masks the iptables
  services (they ship with DROP rules), adds a default DHCP network file, and
  tars the root file system.
- `assets/disk.sh` runs on the build machine in Ubuntu. It makes a 256 MiB
  EFI partition and an ext4 root, writes a fixed GRUB entry for the one
  installed kernel, and compresses the disk.

The GRUB entry is fixed, so kernel updates are done by repaving with a new
image, not by package updates.

The NVIDIA variant uses `kernel-hwe` because the arm64 NVIDIA driver in the
Azure Linux repo (`cuda-open-hwe`) is built only for it. It loads the NVIDIA
modules at boot and runs a persistence-mode unit only when a GPU is bound to the
driver. Fabric Manager and IMEX are not included.

## Agent fixes found during testing

| Problem | Fix |
|---|---|
| GPU preflight could not find `libcuda.so.1` on arm64, because `ldconfig` prints `AArch64`, not `aarch64` | Case-insensitive match in `pkg/agent/phases/host/preflight_nvidia.go` |
| Starting the node container failed: the Azure Linux container has no `/usr/lib/aarch64-linux-gnu` | `pkg/agent/phases/nodestart/nvidia.go` creates the directory and adds it to the linker config |
| Install failures showed too little detail | `vendor-data.tmpl` reports the journal of failed and restarting units, without nspawn console lines |

These fixes are not yet in a released agent. GPU nodes will fail to join with
the released agent until they are.

## Hardware findings

Tested on one arm64 server behind one rack manager.

### Firmware behavior to know

- `RemoteDriveMount` image names must be shorter than 40 characters. That is
  why the ID is 32 hex characters.
- `ImageName` finds files staged with `Tftp.Get`.
- `Tftp.Get` and `RemoteDriveMount` block until done. An 8.8 MB ISO stages in
  2 to 5 seconds; a mount takes about 20 seconds. The default 30-second client
  timeout was too short.
- The rack's TFTP client sends no options (512-byte blocks), about 1 to 3 MB/s.
  Keep the ISO small.
- `ForceOff` works, though the system does not list it.
- The rack power map shows host power, not slot AC. The slot power-on action turns the
  host on.
- Unmounting when nothing is mounted fails with an error saying remote media
  support "is not enabled". Deleting a missing file fails with "No such file
  or directory".
- `GET .../Tftp` (the file list) hangs for minutes. Metalman never calls it.
- An absent slot returns HTTP 500 with a "failed to discover" message.

### Why two mounts

After a host power cycles following a media boot, the BMC's virtual USB devices
can end up in a bad state. Then:

- The first mount of a new name does nothing useful. Linux sees a disk with the
  right size but reads fail, or the USB devices are missing. UEFI logs "USB boot
  desired, but no USB devices found" and boots the next device.
- Mounting the same name again does not help.
- A second mount of a different name always gives a working drive.

So Metalman stages the same ISO under `up-<id>.iso` and `ub-<id>.iso` and
mounts them in that order. With this change every test install booted the
media on the first try. The boot retry remains as a backstop.

### Timing of a successful install

| Step | Time |
|---|---|
| Stage both names | a few seconds |
| Power off, two mounts, set boot, power on | about 1 minute |
| GRUB fetches its config | about 85 seconds after power on |
| Disk written, cleanup done, reboot | about 1 minute later |
| Node `Ready` | about 3 to 4 minutes after reboot |

## Known limits and open work

- Tested on one arm64 server and one firmware version. amd64, other firmware,
  and a slot with AC power truly off are untested.
- No Secure Boot for the installer.
- The rack's virtual drive behavior is worked around, not understood. Other
  firmware may need a different approach.
- Stage and mount are not serialized per rack manager. Many slots at once may
  be slow or hit firmware limits.
- The installer picks the largest disk if `targetDisk` is not set. Set it.
- The ISO's GRUB networking assumes the firmware exposes the NIC to UEFI.
