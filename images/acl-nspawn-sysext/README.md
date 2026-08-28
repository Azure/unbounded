# acl-nspawn-sysext

Builds a [systemd system extension](https://www.freedesktop.org/software/systemd/man/systemd-sysext.html)
that delivers `systemd-container` (`systemd-nspawn`, `machinectl`,
`systemd-machined`) to Azure Container Linux.

## Why

Azure Container Linux reports `ID=azurelinux` but is a Flatcar-derived immutable
image, not Azure Linux 3. On a live `azure-linux-3-acl` instance:

- `/usr` is a read-only btrfs volume merged with sysext overlays
- there is no package manager at all: no `tdnf`, `dnf`, `rpm` or `rpm-ostree`,
  and `rpm -qa` returns zero packages
- `systemd-nspawn` and `machinectl` are absent

The agent requires `systemd-container` (`pkg/agent/phases/host/os.go`), and its
package-manager path cannot install it. sysext is the mechanism the OS itself
uses to extend `/usr`: it already merges `containerd` and `oem-azure` this way.

## Build

```sh
podman build -t acl-nspawn-sysext images/acl-nspawn-sysext
mkdir -p bin
podman run --rm -v "$PWD/bin:/export:z" acl-nspawn-sysext
```

Produces `bin/unbounded-nspawn.raw` and `bin/unbounded-nspawn.provenance`.

## Install on a host

```sh
cp unbounded-nspawn.raw /var/lib/extensions/
systemd-sysext refresh
```

The merge persists across reboot: `systemd-sysext.service` re-merges everything
under `/var/lib/extensions` on each boot. Refreshing does not disturb the
already-running `containerd.service`.

## The extension must ship as a .raw image, not a directory

`systemd-sysext` accepts either form. Only `.raw` works on Azure Container Linux,
and the reason is SELinux, which runs `Enforcing` there.

A directory extension is merged by bind-mounting it out of `/var/lib/extensions`,
which lives on the writable root and carries real SELinux xattrs. The payload
keeps a data label, observed as `container_var_lib_t`. A binary with that label
cannot acquire a D-Bus name, so:

- `systemd-machined` starts but never acquires `org.freedesktop.machine1`, and
  systemd fails the unit with `start operation timed out`
- `systemd-nspawn` fails with `Failed to register machine: Access denied`
- `machinectl list` reports `Could not get machines: Access denied`
- no AVC denials are logged, because the relevant rules are `dontaudit`ed, which
  makes this failure look like anything but SELinux

A squashfs image carries no SELinux xattrs, so the merged files take the
filesystem default, `unlabeled_t`, which is exactly the label Azure Container
Linux's own read-only `/usr` content has. With the `.raw` form, `machinectl`,
`systemd-machined` and a full `systemd-nspawn --boot` container all work under
`Enforcing` with zero AVC denials.

This is why the platform ships its own extensions as `containerd.raw` and
`oem-azure-*.raw` rather than directories. `build-sysext.sh` therefore emits only
`.raw`; relabelling a directory extension with `chcon` was tried and does not
fix it.

## Version coupling and `BUNDLE_SHARED`

`systemd-nspawn` links systemd's private `libsystemd-shared` library, and on
Azure Linux the soname carries the full package release:

```
$ readelf -d usr/bin/systemd-nspawn
 (NEEDED)  Shared library: [libsystemd-shared-255-33.azl3.so]
 (RPATH)   Library rpath: [/usr/lib/systemd]
```

RPM encodes the same constraint: `systemd-container` requires
`systemd(x86-64) = 255-33.azl3`, an exact equality.

Without bundling, an extension only loads on a host running exactly that systemd
build; on any other it dies at exec with `cannot open shared object file`.
`systemd-sysext` does not catch this, because it matches only on `ID` and
`SYSEXT_LEVEL` and will merge an extension whose binaries cannot load.

`BUNDLE_SHARED=1`, the default, ships `libsystemd-shared` inside the extension.
The release-qualified soname that causes the problem also makes the fix safe: two
builds are two different filenames, so the bundled copy and the host's copy
coexist in the merged `/usr` and each binary resolves the one it was linked
against. This is verified, not assumed: an extension built from
`systemd-255-27.azl3` runs on a `255-33.azl3` host, with `systemd-nspawn
--version` reporting `255-27.azl3` while PID 1 continues to report `255-33.azl3`
and the host reports no failed units.

A bundled extension therefore needs only the host's systemd **major** version to
match; it keeps talking to PID 1 over a stable D-Bus interface and unit-file
syntax. Azure Linux 3 has stayed on systemd 255 across at least fourteen releases
between July 2024 and July 2026, so in practice rebuilds are driven by major
version bumps rather than the roughly six-to-eight-week release cadence.

`unbounded-nspawn.provenance` records what the extension was built against.
`pkg/agent/phases/host/sysext.go` parses it and refuses to merge an incompatible
extension, turning a late runtime loader failure into an actionable error.

To target a specific systemd build, set `SYSTEMD_RELEASE`:

```sh
podman build --build-arg SYSTEMD_RELEASE=255-27.azl3 -t acl-nspawn-sysext \
  images/acl-nspawn-sysext
```
