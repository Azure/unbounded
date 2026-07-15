#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# build-cloudhv-firmware.sh -- build the custom Cloud Hypervisor OVMF firmware
# blobs used by the metalman Redfish test fixture (internal/metalman/redfish/qemusvr).
#
# Cloud Hypervisor consumes a single flat firmware image via `--firmware` (there
# is no separate pflash VARS file like qemu). The prebuilt CLOUDHV.fd published
# by the cloud-hypervisor/edk2 project is compiled with secure boot and UEFI
# HTTP boot DISABLED, which the metalman smoke tests require. This script builds
# the OvmfPkg/CloudHv target from the cloud-hypervisor/edk2 fork with those
# features enabled and produces two blobs:
#
#   CLOUDHV.fd             - plain firmware (no secure boot). Used by the PXE smoke.
#   CLOUDHV_SECUREBOOT.fd  - secure boot firmware (fixture --firmware-secureboot).
#
# Secure boot under Cloud Hypervisor is special. The CloudHv firmware descriptor
# has NO on-flash UEFI variable store: variables live in volatile guest RAM that
# is initialized empty on every cold boot, and Cloud Hypervisor loads the
# firmware read-only. Keys therefore cannot be pre-enrolled into the image with a
# tool like virt-fw-vars (there is no varstore FV to edit) and would not persist.
#
# Instead we make the firmware ENROLL the default secure boot keys on every boot.
# edk2 already ships OvmfPkg/EnrollDefaultKeys, which reads a Platform Key from an
# SMBIOS type 11 "OEM String" that the hypervisor supplies (Cloud Hypervisor:
# `--platform oem_strings=[...]`) and enrolls it together with the compiled-in
# Microsoft KEK/db certificates. Upstream ships it as a UEFI application; this
# script converts it into an auto-dispatched DXE driver and adds it to the secure
# boot firmware volume. Because the RAM varstore starts empty (setup mode) on
# every cold boot, the driver re-enrolls the keys before BDS/the OS runs, so the
# guest observes SecureBoot=1 / SetupMode=0 on every boot. The metalman fixture
# passes the matching PK certificate via the SMBIOS OEM string at launch.
#
# The build runs inside a pinned Ubuntu container so it is reproducible on any
# host with docker and in CI. Output artifacts are written to the directory given
# by the first argument (default: $repo_root/bin/cloudhv-firmware).

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
out_dir="${1:-$repo_root/bin/cloudhv-firmware}"

# Pin the cloud-hypervisor/edk2 fork revision. Branch `ch` carries the CloudHv
# platform plus the submodule pins Cloud Hypervisor expects. Pinning the exact
# commit keeps the firmware (and the source transformation below) reproducible.
: "${EDK2_REPO:=https://github.com/cloud-hypervisor/edk2.git}"
: "${EDK2_COMMIT:=1e1b96f1264a9c9532cbeb053c8c05885a7d2c78}"
: "${BUILD_IMAGE:=ubuntu:24.04}"

mkdir -p "$out_dir"

echo "==> building Cloud Hypervisor OVMF firmware in $BUILD_IMAGE"
echo "    edk2: $EDK2_REPO @ $EDK2_COMMIT"
echo "    out:  $out_dir"

docker run --rm \
	-e EDK2_REPO="$EDK2_REPO" \
	-e EDK2_COMMIT="$EDK2_COMMIT" \
	-v "$out_dir:/out" \
	"$BUILD_IMAGE" bash -euo pipefail -c '
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq
	apt-get install -y -qq --no-install-recommends \
		build-essential uuid-dev iasl nasm git python3 ca-certificates >/dev/null

	git clone "$EDK2_REPO" /edk2
	cd /edk2
	git checkout --quiet "$EDK2_COMMIT"
	git submodule update --init --depth 1

	# Convert OvmfPkg/EnrollDefaultKeys from a UEFI application into an
	# auto-dispatched DXE driver, and add it to the secure boot firmware
	# volume. See the header comment for why this is how secure boot works
	# under Cloud Hypervisor.
	python3 - <<"PYEOF"
import io, os

def patch(path, edits):
	with io.open(path, "r", encoding="utf-8") as f:
		data = f.read()
	for old, new in edits:
		if old not in data:
			raise SystemExit("anchor not found in %s: %r" % (path, old))
		data = data.replace(old, new, 1)
	with io.open(path, "w", encoding="utf-8") as f:
		f.write(data)

enroll_c = "OvmfPkg/EnrollDefaultKeys/EnrollDefaultKeys.c"
with io.open(enroll_c, "r", encoding="utf-8") as f:
	lines = f.readlines()
# Drop the shell entry-point header include; this is no longer a shell app.
kept = [ln for ln in lines if "Library/ShellCEntryLib.h" not in ln]
if len(kept) == len(lines):
	raise SystemExit("ShellCEntryLib.h include not found in %s" % enroll_c)
with io.open(enroll_c, "w", encoding="utf-8") as f:
	f.writelines(kept)
patch(enroll_c, [
	# Rename the core routine so we can wrap it with a DXE entry point.
	("ShellAppMain (", "EnrollDefaultKeysMain ("),
])
with io.open(enroll_c, "a", encoding="utf-8") as f:
	f.write("""
/**
  DXE driver entry point.

  Cloud Hypervisor keeps UEFI variables in volatile guest RAM that starts empty
  (setup mode) on every cold boot, so this driver re-enrolls the default secure
  boot keys on each boot. The Platform Key is supplied by the hypervisor through
  an SMBIOS type 11 OEM String (see EnrollDefaultKeysMain -> GetPkKek1).

  Failure is intentionally non-fatal: if no PK OEM String is present the platform
  simply stays in setup mode instead of blocking boot.
**/
EFI_STATUS
EFIAPI
EnrollDefaultKeysEntryPoint (
  IN EFI_HANDLE        ImageHandle,
  IN EFI_SYSTEM_TABLE  *SystemTable
  )
{
  EnrollDefaultKeysMain (0, NULL);
  return EFI_SUCCESS;
}
""")

enroll_inf = "OvmfPkg/EnrollDefaultKeys/EnrollDefaultKeys.inf"
patch(enroll_inf, [
	("MODULE_TYPE                    = UEFI_APPLICATION",
	 "MODULE_TYPE                    = DXE_DRIVER"),
	("ENTRY_POINT                    = ShellCEntryLib",
	 "ENTRY_POINT                    = EnrollDefaultKeysEntryPoint"),
	("  ShellPkg/ShellPkg.dec\n", ""),
	("  ShellCEntryLib\n", "  UefiDriverEntryPoint\n"),
])
with io.open(enroll_inf, "a", encoding="utf-8") as f:
	f.write("""
[Depex]
  gEfiVariableArchProtocolGuid      AND
  gEfiVariableWriteArchProtocolGuid AND
  gEfiSmbiosProtocolGuid
""")

fdf = "OvmfPkg/CloudHv/CloudHvX64.fdf"
patch(fdf, [
	("  INF  SecurityPkg/VariableAuthenticated/SecureBootConfigDxe/SecureBootConfigDxe.inf\n",
	 "  INF  SecurityPkg/VariableAuthenticated/SecureBootConfigDxe/SecureBootConfigDxe.inf\n"
	 "  INF  OvmfPkg/EnrollDefaultKeys/EnrollDefaultKeys.inf\n"),
])
print("edk2 secure boot auto-enroll transformation applied")
PYEOF

	export PYTHON_COMMAND=python3
	# edksetup.sh and the edk2 build env are not written for `set -u`/`set -e`;
	# relax nounset and errexit while sourcing it (edksetup.sh returns a
	# non-zero status that would otherwise abort the whole build), then restore
	# errexit for the actual build steps.
	set +eu
	# shellcheck disable=SC1091
	source ./edksetup.sh
	set -e
	# ubuntu:24.04 ships gcc-13; the pinned edk2 BaseTools VfrCompile (antlr
	# generated C++) does not compile cleanly with it. -fpermissive downgrades
	# the offending warnings. BaseTools MUST be built serially (no -j): with a
	# parallel make the antlr-generated EfiVfrParser.cpp can be compiled while
	# it is still being regenerated, producing spurious hard errors about
	# SetWordType not being a static data member of class EfiVfrParser.
	make -C BaseTools EXTRA_OPTFLAGS="-fpermissive -Wno-error"

	build_fd() {
		# $1: extra -D flags; $2: output name
		local extra="$1" name="$2"
		build \
			-a X64 \
			-t GCC5 \
			-p OvmfPkg/CloudHv/CloudHvX64.dsc \
			-b RELEASE \
			$extra
		cp Build/CloudHvX64/RELEASE_GCC5/FV/CLOUDHV.fd "/out/$name"
		# The build directory is reused between variants; wipe it so the
		# next variant is compiled from scratch with its own PCDs.
		rm -rf Build/CloudHvX64
	}

	# Plain firmware: no secure boot, but with UEFI HTTP boot + TLS + IPv6 so
	# firmware-native HTTP boot works for both smoke variants.
	build_fd "-D NETWORK_HTTP_BOOT_ENABLE=TRUE -D NETWORK_TLS_ENABLE=TRUE -D NETWORK_IP6_ENABLE=TRUE" CLOUDHV.fd

	# Secure boot firmware: same networking plus secure boot support with the
	# auto-enroll DXE driver added above. SMM is intentionally NOT required
	# (Cloud Hypervisor has no SMM).
	build_fd "-D SECURE_BOOT_ENABLE=TRUE -D NETWORK_HTTP_BOOT_ENABLE=TRUE -D NETWORK_TLS_ENABLE=TRUE -D NETWORK_IP6_ENABLE=TRUE" CLOUDHV_SECUREBOOT.fd

	chmod 0644 /out/CLOUDHV.fd /out/CLOUDHV_SECUREBOOT.fd
'

echo
echo "==> built firmware:"
ls -la "$out_dir"/CLOUDHV.fd "$out_dir"/CLOUDHV_SECUREBOOT.fd
sha256sum "$out_dir"/CLOUDHV.fd "$out_dir"/CLOUDHV_SECUREBOOT.fd
