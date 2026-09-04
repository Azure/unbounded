#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# Assembles a systemd system extension delivering systemd-container. Run inside
# the builder image, where the systemd-container package is already installed so
# that `rpm -ql` reports the exact file list to extract.
#
# BUNDLE_SHARED=1 additionally ships systemd's private libsystemd-shared library
# inside the extension. systemd-nspawn's DT_NEEDED names that library by full
# NVR (libsystemd-shared-255-33.azl3.so), so without bundling the extension only
# loads on a host running exactly the systemd build it was made from. Because the
# soname is NVR-qualified, the bundled copy cannot collide with the host's own
# copy: they are different filenames in the same merged directory, and each
# binary resolves the one it was linked against.
set -euo pipefail

SYSEXT_NAME="${SYSEXT_NAME:-unbounded-nspawn}"
SYSEXT_LEVEL="${SYSEXT_LEVEL:-1.0}"
HOST_ID="${HOST_ID:-azurelinux}"
BUNDLE_SHARED="${BUNDLE_SHARED:-0}"
OUT="${OUT:-/out}"
ROOT="$(mktemp -d)"

copy_package_files() {
    # Copy the files owned by a package. Documentation is dropped: it would be
    # merged into the host's read-only /usr for no benefit.
    local pkg="$1"
    shift

    rpm -ql "${pkg}" | while read -r f; do
        case "$f" in
        /usr/share/man/* | /usr/share/doc/* | /usr/share/locale/*) continue ;;
        esac

        if [ "$#" -gt 0 ]; then
            local keep=0
            for pattern in "$@"; do
                # shellcheck disable=SC2254
                case "$f" in
                $pattern) keep=1 ;;
                esac
            done
            [ "${keep}" = 1 ] || continue
        fi

        if [ -d "$f" ] && [ ! -L "$f" ]; then
            mkdir -p "${ROOT}${f}"
        elif [ -e "$f" ] || [ -L "$f" ]; then
            mkdir -p "${ROOT}$(dirname "$f")"
            cp -a "$f" "${ROOT}${f}"
        fi
    done
}

container_nvr="$(rpm -q systemd-container)"
systemd_nvr="$(rpm -q systemd)"
echo "building sysext ${SYSEXT_NAME} from ${container_nvr} (systemd ${systemd_nvr})"

copy_package_files systemd-container

shared_lib=""

if [ "${BUNDLE_SHARED}" = "1" ]; then
    # Take only the private shared library from the systemd package, never the
    # rest of it: replacing the host's PID 1 or its units through a sysext would
    # be both wrong and unsupported.
    copy_package_files systemd '/usr/lib/systemd/libsystemd-shared-*.so'

    shared_lib="$(cd "${ROOT}" && ls usr/lib/systemd/libsystemd-shared-*.so 2>/dev/null | head -1 || true)"
    if [ -z "${shared_lib}" ]; then
        echo "error: BUNDLE_SHARED=1 but no libsystemd-shared library was found" >&2
        exit 1
    fi

    echo "bundled /${shared_lib}"
fi

# systemd-sysext refuses to merge an extension whose extension-release does not
# match the host. ID must equal the host's ID, and SYSEXT_LEVEL must equal the
# host's SYSEXT_LEVEL.
mkdir -p "${ROOT}/usr/lib/extension-release.d"
cat >"${ROOT}/usr/lib/extension-release.d/extension-release.${SYSEXT_NAME}" <<EOF
ID=${HOST_ID}
SYSEXT_LEVEL=${SYSEXT_LEVEL}
SYSEXT_SCOPE=system
EOF

# Provenance lets a host-side installer decide whether this extension can run on
# the host before merging it, turning a runtime "cannot open shared object file"
# into an actionable preflight failure. BUNDLED_SHARED_LIB tells the installer
# whether the extension is self-contained (any host systemd of the same major
# version) or pinned (host systemd must equal SYSTEMD_NVR exactly).
cat >"${ROOT}/usr/lib/extension-release.d/${SYSEXT_NAME}.provenance" <<EOF
SYSEXT_NAME=${SYSEXT_NAME}
SYSTEMD_CONTAINER_NVR=${container_nvr}
SYSTEMD_NVR=${systemd_nvr}
BUNDLED_SHARED_LIB=${shared_lib}
EOF

for required in \
    /usr/bin/systemd-nspawn \
    /usr/bin/machinectl \
    /usr/lib/systemd/systemd-machined \
    /usr/lib/systemd/system/systemd-nspawn@.service \
    /usr/lib/systemd/system/systemd-machined.service \
    /usr/share/dbus-1/system.d/org.freedesktop.machine1.conf; do
    if [ ! -e "${ROOT}${required}" ]; then
        echo "error: expected payload missing from extension: ${required}" >&2
        exit 1
    fi
done

# The extension must never carry systemd's own units or PID 1.
for forbidden in \
    /usr/lib/systemd/systemd \
    /usr/lib/systemd/system/basic.target; do
    if [ -e "${ROOT}${forbidden}" ]; then
        echo "error: extension must not replace host systemd: ${forbidden}" >&2
        exit 1
    fi
done

mkdir -p "${OUT}"

# The extension MUST ship as a squashfs .raw image, not a directory tree.
#
# systemd-sysext merges a directory extension by bind-mounting it out of
# /var/lib/extensions, which lives on the writable root and carries real SELinux
# xattrs. The payload therefore keeps a data label such as container_var_lib_t,
# and under SELinux Enforcing a binary with that label cannot acquire a D-Bus
# name: systemd-machined times out and systemd-nspawn fails with
# "Failed to register machine: Access denied".
#
# A squashfs image carries no SELinux xattrs, so the merged files take the
# filesystem default, unlabeled_t, which is exactly what Azure Container Linux's
# own read-only /usr uses. This is why the platform ships its own extensions as
# containerd.raw and oem-azure-*.raw rather than directories.
mksquashfs "${ROOT}" "${OUT}/${SYSEXT_NAME}.raw" \
    -all-root -noappend -quiet -no-progress -no-xattrs

cp "${ROOT}/usr/lib/extension-release.d/${SYSEXT_NAME}.provenance" \
    "${OUT}/${SYSEXT_NAME}.provenance"

echo "wrote ${OUT}/${SYSEXT_NAME}.raw"
