#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Run the racer root-only ublk tests inside a disposable QEMU guest.
#
# The guest boots an Ubuntu generic kernel chosen by GUEST_KERNEL: 6.8 from
# Ubuntu 24.04 (noble) or 7.0 from Ubuntu 26.04 (resolute). Its initramfs holds
# busybox-static, the Rust test binary, its shared libraries, and ublk_drv.ko.
# The host needs no ublk support and no root, and nothing is installed: the
# kernel packages are downloaded with a private, signature-checked APT
# configuration and unpacked under WORK_DIR.
#
# Usage:
#   TEST_BIN=<path> hack/scripts/racer-ublk-vm-test.sh
#
# TEST_BIN is the racer unit test binary, for example the path
# printed by `cargo test -p racer --no-run`. The guest runs
# `TEST_BIN $TEST_FILTER --test-threads=1` as root with RACER_UBLK_TEST=1. The
# script fails unless QEMU exits zero, the binary exits zero, libtest prints
# "test result: ok." with at least one passed test, and no test reports that it
# skipped the kernel checks.
#
# Environment:
#   TEST_BIN          test binary (required)
#   TEST_FILTER       libtest filter (default ublk)
#   GUEST_KERNEL      6.8 or 7.0 (default 6.8)
#   WORK_DIR          scratch output (default bin/racer-vm/$GUEST_KERNEL)
#   KERNEL_PACKAGE    kernel meta package (default linux-image-generic)
#   ARCHIVE_URL       Ubuntu archive (default http://archive.ubuntu.com/ubuntu)
#   ARCHIVE_KEYRING   archive keys (default /usr/share/keyrings/ubuntu-archive-keyring.gpg)
#   VM_TIMEOUT        seconds before QEMU is terminated (default 600)
#   GUEST_TIMEOUT     guest watchdog seconds for the test binary (default 540)
#   VM_MEMORY         guest memory (default 4G)
#   VM_CPUS           guest CPUs (default 2)
#   VM_ACCEL          kvm, tcg, or auto (default auto)
#   QEMU              QEMU binary (default qemu-system-x86_64)

set -euo pipefail

TEST_BIN="${TEST_BIN:-}"
TEST_FILTER="${TEST_FILTER:-ublk}"
GUEST_KERNEL="${GUEST_KERNEL:-6.8}"
WORK_DIR="${WORK_DIR:-bin/racer-vm/$GUEST_KERNEL}"
KERNEL_PACKAGE="${KERNEL_PACKAGE:-linux-image-generic}"
ARCHIVE_URL="${ARCHIVE_URL:-http://archive.ubuntu.com/ubuntu}"
ARCHIVE_KEYRING="${ARCHIVE_KEYRING:-/usr/share/keyrings/ubuntu-archive-keyring.gpg}"
VM_TIMEOUT="${VM_TIMEOUT:-600}"
GUEST_TIMEOUT="${GUEST_TIMEOUT:-540}"
VM_MEMORY="${VM_MEMORY:-4G}"
VM_CPUS="${VM_CPUS:-2}"
VM_ACCEL="${VM_ACCEL:-auto}"
QEMU="${QEMU:-qemu-system-x86_64}"

log() { echo "racer-vm: $*" >&2; }
die() {
	log "error: $*"
	exit 1
}

for tool in "$QEMU" apt-get apt-cache dpkg-deb busybox cpio gzip zstd ldd modinfo file timeout tee; do
	command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
done
[[ -n "$TEST_BIN" && -x "$TEST_BIN" ]] || die "TEST_BIN must name an executable test binary"
case "$GUEST_KERNEL" in
6.8) RELEASE=noble ;;
7.0) RELEASE=resolute ;;
*) die "GUEST_KERNEL must be 6.8 or 7.0, not '$GUEST_KERNEL'" ;;
esac
case "$VM_ACCEL" in
auto) ACCEL=kvm:tcg ;;
kvm | tcg) ACCEL="$VM_ACCEL" ;;
*) die "VM_ACCEL must be auto, kvm, or tcg" ;;
esac
[[ -r "$ARCHIVE_KEYRING" ]] || die "archive keyring $ARCHIVE_KEYRING not readable"
mkdir -p "$WORK_DIR"
WORK_DIR="$(cd "$WORK_DIR" && pwd)"

# fetch_kernel downloads and unpacks the kernel image and modules for RELEASE
# with a private APT configuration that reads no host APT state. The unpacked
# tree is reused only while its version file names a GUEST_KERNEL kernel whose
# vmlinuz is present; any other cache is discarded and refetched.
fetch_kernel() {
	local apt="$WORK_DIR/apt" root="$WORK_DIR/kernel-root" pkgs="$WORK_DIR/pkgs"
	local suite deps image version deb
	if [[ -f "$WORK_DIR/version" && -d "$root" ]]; then
		version="$(<"$WORK_DIR/version")"
		if [[ ("$version" == "$GUEST_KERNEL".* || "$version" == "$GUEST_KERNEL"-*) &&
			-f "$root/boot/vmlinuz-$version" ]]; then
			echo "$version"
			return
		fi
		log "discarding cached kernel ${version:-<empty>}: not a usable Linux $GUEST_KERNEL kernel"
		rm -f "$WORK_DIR/version"
	fi
	rm -rf "$apt" "$root" "$pkgs"
	mkdir -p "$apt"/etc/{apt.conf.d,preferences.d,trusted.gpg.d,sources.list.d} \
		"$apt/state/lists/partial" "$apt/cache/archives/partial" "$root" "$pkgs"
	: >"$apt/state/status"
	for suite in "$RELEASE" "$RELEASE-updates" "$RELEASE-security"; do
		echo "deb [arch=amd64 signed-by=$ARCHIVE_KEYRING] $ARCHIVE_URL $suite main"
	done >"$apt/etc/sources.list"
	cat >"$apt/apt.conf" <<EOF
Dir "/";
Dir::Etc "$apt/etc";
Dir::Etc::main "/dev/null";
Dir::Etc::parts "$apt/etc/apt.conf.d";
Dir::Etc::sourcelist "$apt/etc/sources.list";
Dir::Etc::sourceparts "$apt/etc/sources.list.d";
Dir::Etc::preferences "/dev/null";
Dir::Etc::preferencesparts "$apt/etc/preferences.d";
Dir::Etc::trusted "/dev/null";
Dir::Etc::trustedparts "$apt/etc/trusted.gpg.d";
Dir::State "$apt/state";
Dir::State::status "$apt/state/status";
Dir::Cache "$apt/cache";
APT::Architecture "amd64";
APT::Architectures { "amd64"; };
APT::Get::AllowUnauthenticated "false";
Acquire::AllowInsecureRepositories "false";
Acquire::Languages "none";
Acquire::Retries "3";
Debug::NoLocking "true";
EOF
	export APT_CONFIG="$apt/apt.conf"
	log "updating private APT lists for $RELEASE"
	timeout -s TERM -k 10s 300s apt-get -q update >&2 || die "apt-get update failed"
	deps="$(apt-cache depends "$KERNEL_PACKAGE" | sed -n 's/^ *Depends: \(linux-[^ ]*\)$/\1/p')"
	image="$(grep -m 1 -E '^linux-image-[0-9][^ ]*-generic$' <<<"$deps" || true)"
	[[ -n "$image" ]] || die "cannot resolve kernel image from $KERNEL_PACKAGE"
	version="${image#linux-image-}"
	[[ "$version" == "$GUEST_KERNEL".* || "$version" == "$GUEST_KERNEL"-* ]] ||
		die "expected a Linux $GUEST_KERNEL kernel, got $version"
	# 6.8 ships ublk_drv in linux-modules-extra; 7.0 ships it in linux-modules.
	deps+=$'\n'"$(apt-cache depends "$image" | sed -n 's/^ *Depends: \(linux-[^ ]*\)$/\1/p')"
	mapfile -t deps < <(grep -E "^linux-(image|modules|modules-extra)-${version//./\\.}\$" <<<"$deps" | sort -u)
	log "downloading kernel $version: ${deps[*]}"
	(cd "$pkgs" && timeout -s TERM -k 10s 300s apt-get -q download "${deps[@]}" >&2) ||
		die "downloading kernel $version failed"
	for deb in "$pkgs"/*.deb; do
		dpkg-deb -x "$deb" "$root"
	done
	[[ -f "$root/boot/vmlinuz-$version" ]] || die "vmlinuz-$version missing from packages"
	echo "$version" >"$WORK_DIR/version"
	echo "$version"
}

# add_module copies a module and its dependencies into the initramfs,
# decompressed because busybox insmod cannot read compressed modules.
add_module() {
	local root="$1" moddir="$2" name="$3" path dep
	path="$(find "$moddir" -name "${name}.ko*" -print -quit)"
	[[ -n "$path" ]] || die "kernel module $name not found"
	for dep in $(modinfo -F depends "$path" | tr ',' ' '); do
		add_module "$root" "$moddir" "$dep"
	done
	[[ -f "$root/modules/${name}.ko" ]] && return 0
	case "$path" in
	*.zst) zstd -q -d -c "$path" >"$root/modules/${name}.ko" ;;
	*.xz) xz -d -c "$path" >"$root/modules/${name}.ko" ;;
	*) cp "$path" "$root/modules/${name}.ko" ;;
	esac
	echo "$name" >>"$root/modules/order"
}

build_initramfs() {
	local version="$1" root="$WORK_DIR/initramfs" moddir lib busybox
	busybox="$(command -v busybox)"
	file -L "$busybox" | grep -q 'statically linked' || die "$busybox is not static (install busybox-static)"
	moddir="$WORK_DIR/kernel-root/lib/modules/$version"
	[[ -d "$moddir" ]] || moddir="$WORK_DIR/kernel-root/usr/lib/modules/$version"
	rm -rf "$root"
	mkdir -p "$root"/{bin,dev,proc,sys,tmp,modules,tests}
	cp "$busybox" "$root/bin/busybox"
	ln -s busybox "$root/bin/sh"
	: >"$root/modules/order"
	add_module "$root" "$moddir" ublk_drv
	cp "$TEST_BIN" "$root/tests/test-bin"
	if ldd "$TEST_BIN" >/dev/null 2>&1; then
		for lib in $(ldd "$TEST_BIN" | grep -oE '/[^ ]+'); do
			mkdir -p "$root$(dirname "$lib")"
			cp -L "$lib" "$root$lib"
		done
	fi
	cat >"$root/init" <<EOF
#!/bin/sh
/bin/busybox --install -s /bin
export PATH=/bin HOME=/tmp TMPDIR=/tmp RACER_UBLK_TEST=1 RUST_BACKTRACE=1
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mount -t tmpfs tmpfs /tmp
fatal() { echo "RACER_VM_FATAL \$*"; poweroff -n -f; }
echo "RACER_VM_KERNEL \$(uname -r)"
[ "\$(uname -r)" = '$version' ] || fatal "booted \$(uname -r), expected $version"
while read -r m; do insmod "/modules/\$m.ko" || fatal "insmod \$m failed"; done </modules/order
i=0
while [ ! -c /dev/ublk-control ] && [ \$i -lt 50 ]; do sleep 0.1; i=\$((i + 1)); done
[ -c /dev/ublk-control ] || fatal "/dev/ublk-control missing"
if [ -f /proc/sys/kernel/io_uring_disabled ]; then echo 0 >/proc/sys/kernel/io_uring_disabled; fi
( sleep $GUEST_TIMEOUT; echo "RACER_VM_FATAL tests hung for ${GUEST_TIMEOUT}s"; poweroff -n -f ) &
cd /tmp
/tests/test-bin '$TEST_FILTER' --test-threads=1 --nocapture 2>&1
echo "RACER_VM_RESULT \$?"
poweroff -n -f
EOF
	chmod +x "$root/init"
	(cd "$root" && find . -print0 | cpio --null -o -H newc --quiet) | gzip -1 >"$WORK_DIR/initramfs.cpio.gz"
	rm -rf "$root"
}

version="$(fetch_kernel)"
build_initramfs "$version"
console="$WORK_DIR/console.log"
log "booting $version (accel=$ACCEL, timeout=${VM_TIMEOUT}s)"
status=0
timeout -s TERM -k 10s "$VM_TIMEOUT" "$QEMU" \
	-machine "q35,accel=$ACCEL" -cpu max -smp "$VM_CPUS" -m "$VM_MEMORY" \
	-kernel "$WORK_DIR/kernel-root/boot/vmlinuz-$version" \
	-initrd "$WORK_DIR/initramfs.cpio.gz" \
	-append "console=ttyS0 rdinit=/init panic=-1 oops=panic quiet" \
	-nographic -no-reboot -nodefaults -serial stdio -monitor none \
	</dev/null | tee "$console" || status=$?

text="$(tr -d '\r' <"$console")"
rc=0
((status == 0)) || {
	log "error: QEMU exited $status"
	rc=1
}
if grep -qE 'Kernel panic|BUG: |Oops: |RACER_VM_FATAL' <<<"$text"; then
	grep -m 10 -E 'Kernel panic|BUG: |Oops: |RACER_VM_FATAL' <<<"$text" >&2
	log "error: guest reported a fatal error"
	rc=1
fi
[[ "$(sed -n 's/^RACER_VM_RESULT \([0-9]*\)$/\1/p' <<<"$text" | tail -n 1)" == 0 ]] || {
	log "error: test binary did not exit zero"
	rc=1
}
grep -qE '^test result: ok\. [1-9][0-9]* passed' <<<"$text" || {
	log "error: no passing libtest summary"
	rc=1
}
if grep -q 'skipping ublk kernel test' <<<"$text"; then
	log "error: a ublk kernel test skipped inside the VM"
	rc=1
fi
((rc == 0)) && log "ublk tests passed on $version"
exit "$rc"
