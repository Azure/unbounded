#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# Run the racer-block ublk device tests inside a disposable QEMU guest.
#
# The guest boots the Ubuntu 24.04 GA generic kernel (6.8, the minimum kernel
# racer-block supports) with an initramfs built from busybox-static, the
# race-enabled test binaries, and their shared libraries. The host needs no
# ublk support, no root, and no kernel module changes, and a kernel crash in
# the guest cannot wedge the host.
#
# Usage:
#   hack/scripts/racer-block-vm-test.sh run
#   hack/scripts/racer-block-vm-test.sh self-test
#
# "run" expects the race-enabled test binaries in $TEST_BIN_DIR (default
# bin/racer-block-tests): ublk.test and racerblock.test. The guest runs the
# full ublk package and TestEndToEndDevice as root. The job fails unless the
# guest reports a result for every suite, every suite exits zero, and no test
# skipped because root, /dev/ublk-control, or io_uring was missing.
#
# Environment:
#   TEST_BIN_DIR      test binaries (default bin/racer-block-tests)
#   WORK_DIR          scratch output (default bin/racer-block-vm)
#   KERNEL_PACKAGE    kernel meta package (default linux-image-generic)
#   VM_TIMEOUT        seconds before QEMU is terminated (default 1500)
#   VM_MEMORY         guest memory (default 4G)
#   VM_CPUS           guest CPUs (default 2)
#   VM_ACCEL          kvm, tcg, or auto (default auto)
#   SUITE_TIMEOUT     guest watchdog seconds per suite (default 600)
#   KERNEL_IMAGE      prebuilt kernel image; skips the apt download
#   KERNEL_MODULES    directory searched for ublk_drv.ko* (with KERNEL_IMAGE)
#   KERNEL_VERSION    expected guest uname -r (with KERNEL_IMAGE)
#
# KERNEL_IMAGE, KERNEL_MODULES, and KERNEL_VERSION let a developer try another
# kernel locally. CI uses the default Ubuntu 6.8 download.

set -euo pipefail

TEST_BIN_DIR="${TEST_BIN_DIR:-bin/racer-block-tests}"
WORK_DIR="${WORK_DIR:-bin/racer-block-vm}"
KERNEL_PACKAGE="${KERNEL_PACKAGE:-linux-image-generic}"
VM_TIMEOUT="${VM_TIMEOUT:-1500}"
VM_MEMORY="${VM_MEMORY:-4G}"
VM_CPUS="${VM_CPUS:-2}"
VM_ACCEL="${VM_ACCEL:-auto}"
SUITE_TIMEOUT="${SUITE_TIMEOUT:-600}"
KERNEL_IMAGE="${KERNEL_IMAGE:-}"
KERNEL_MODULES="${KERNEL_MODULES:-}"
KERNEL_VERSION="${KERNEL_VERSION:-}"

# Each suite is "name|binary|arguments". Test timeouts stay below VM_TIMEOUT
# so a hung test reports through the guest before the host kills QEMU.
SUITES=(
	"ublk|ublk.test|-test.timeout=8m -test.v"
	"racerblock|racerblock.test|-test.timeout=8m -test.v -test.run=^TestEndToEndDevice\$"
)

# Skip messages that mean the environment, not the test, decided the outcome.
FATAL_SKIP_PATTERN='RACER_BLOCK_TEST_VM|requires root|/dev/ublk-control unavailable|unavailable: .*(no such|not permitted|not supported|function not implemented)|io_uring unavailable'

log() { echo "racer-block-vm: $*" >&2; }
die() {
	log "error: $*"
	exit 1
}

require_tools() {
	local missing=() tool
	for tool in "$@"; do
		command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
	done
	if ((${#missing[@]} > 0)); then
		die "missing required tools: ${missing[*]}"
	fi
}

# check_results LOG prints a verdict and returns nonzero unless every suite
# reported exit status 0 and no suite skipped for an environmental reason.
check_results() {
	local console="$1" spec name status rc=0
	[[ -f "$console" ]] || {
		log "error: console log $console missing"
		return 1
	}
	if grep -qE 'Kernel panic|BUG: |Oops: |RACER_BLOCK_GUEST_FATAL' "$console"; then
		log "error: guest reported a fatal error:"
		grep -E 'Kernel panic|BUG: |Oops: |RACER_BLOCK_GUEST_FATAL' "$console" | head -n 20 >&2
		rc=1
	fi
	if grep -E -A2 -e '--- SKIP' "$console" | grep -qE "$FATAL_SKIP_PATTERN"; then
		log "error: tests skipped because the guest lacked root, ublk, or io_uring"
		rc=1
	fi
	for spec in "${SUITES[@]}"; do
		name="${spec%%|*}"
		status="$(sed -n "s/^RACER_BLOCK_SUITE_RESULT ${name} \([0-9][0-9]*\).*/\1/p" "$console" | tr -d '\r' | tail -n 1)"
		if [[ -z "$status" ]]; then
			log "error: suite $name reported no result (guest crash or timeout)"
			rc=1
		elif [[ "$status" != 0 ]]; then
			log "error: suite $name exited $status"
			rc=1
		else
			log "suite $name passed"
		fi
	done
	if ! grep -qE -- '^--- PASS: TestEndToEndDevice ' "$console"; then
		log "error: TestEndToEndDevice did not report PASS"
		rc=1
	fi
	if ! grep -q '^RACER_BLOCK_GUEST_DONE' "$console"; then
		log "error: guest did not finish"
		rc=1
	fi
	return "$rc"
}

# fetch_kernel downloads and unpacks the kernel image and modules without
# installing them, so the runner's own kernel and /boot are untouched.
fetch_kernel() {
	local pkgdir="$WORK_DIR/pkgs" root="$WORK_DIR/kernel-root" image version
	require_tools apt-cache apt-get dpkg-deb
	image="$(apt-cache depends "$KERNEL_PACKAGE" | sed -n 's/^ *Depends: \(linux-image-[0-9][^ ]*-generic\)$/\1/p' | head -n 1)"
	[[ -n "$image" ]] || die "cannot resolve kernel image from $KERNEL_PACKAGE"
	version="${image#linux-image-}"
	case "$version" in
	6.8.*) ;;
	*) die "expected an Ubuntu 6.8 generic kernel, $KERNEL_PACKAGE resolved to $version" ;;
	esac
	log "using kernel $version"
	rm -rf "$pkgdir" "$root"
	mkdir -p "$pkgdir" "$root"
	(cd "$pkgdir" && timeout --signal=TERM --kill-after=10s 300s apt-get download \
		"linux-image-$version" "linux-modules-$version" "linux-modules-extra-$version")
	local deb
	for deb in "$pkgdir"/*.deb; do
		dpkg-deb -x "$deb" "$root"
	done
	[[ -f "$root/boot/vmlinuz-$version" ]] || die "vmlinuz-$version missing from packages"
	echo "$version"
}

# add_module ROOT MODDIR NAME copies a module and its dependencies into the
# initramfs, decompressed because busybox insmod cannot read .zst files.
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

copy_libs() {
	local root="$1" bin="$2" lib
	for lib in $(ldd "$bin" | grep -oE '/[^ ]+'); do
		mkdir -p "$root$(dirname "$lib")"
		cp -L "$lib" "$root$lib"
	done
}

build_initramfs() {
	local version="$1" root="$WORK_DIR/initramfs" spec name bin args
	local busybox
	busybox="$(command -v busybox)"
	file -L "$busybox" | grep -q 'statically linked' || die "$busybox is not static (install busybox-static)"
	rm -rf "$root"
	mkdir -p "$root"/{bin,dev,proc,sys,tmp,run,modules,tests}
	cp "$busybox" "$root/bin/busybox"
	ln -s busybox "$root/bin/sh"
	: >"$root/modules/order"
	add_module "$root" "$MODULES_DIR" ublk_drv
	for spec in "${SUITES[@]}"; do
		bin="$(cut -d'|' -f2 <<<"$spec")"
		[[ -x "$TEST_BIN_DIR/$bin" ]] || die "test binary $TEST_BIN_DIR/$bin missing"
		cp "$TEST_BIN_DIR/$bin" "$root/tests/$bin"
		copy_libs "$root" "$TEST_BIN_DIR/$bin"
	done
	{
		echo '#!/bin/sh'
		echo "EXPECTED_KERNEL='$version'"
		echo "SUITE_TIMEOUT='$SUITE_TIMEOUT'"
		cat <<'EOF'
/bin/busybox --install -s /bin
export PATH=/bin HOME=/tmp TMPDIR=/tmp
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mount -t tmpfs tmpfs /tmp
fatal() { echo "RACER_BLOCK_GUEST_FATAL $*"; echo RACER_BLOCK_GUEST_DONE; poweroff -n -f; }
echo "RACER_BLOCK_GUEST_KERNEL $(uname -r)"
while read -r m; do
	insmod "/modules/$m.ko" || fatal "insmod $m failed"
done </modules/order
i=0
while [ ! -c /dev/ublk-control ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
[ -c /dev/ublk-control ] || fatal "/dev/ublk-control missing"
[ "$(id -u)" = 0 ] || fatal "guest init is not root"
if [ -f /proc/sys/kernel/io_uring_disabled ]; then echo 0 >/proc/sys/kernel/io_uring_disabled; fi
[ "$(uname -r)" = "$EXPECTED_KERNEL" ] || fatal "booted $(uname -r), expected $EXPECTED_KERNEL"
# Startup fault regressions run only in a disposable VM. Never set on a host.
export RACER_BLOCK_TEST_VM=1
cd /tmp
# A test stuck in the kernel can outlive its own -test.timeout panic, so a
# watchdog reports the hang and powers off instead of waiting for the host.
run_suite() {
	name="$1"
	shift
	echo "RACER_BLOCK_SUITE_START $name"
	(
		sleep "$SUITE_TIMEOUT"
		echo "RACER_BLOCK_GUEST_FATAL suite $name hung for ${SUITE_TIMEOUT}s"
		echo RACER_BLOCK_GUEST_DONE
		poweroff -n -f
	) &
	dog=$!
	"$@" 2>&1
	rc=$?
	kill "$dog" 2>/dev/null
	echo "RACER_BLOCK_SUITE_RESULT $name $rc"
}
EOF
		for spec in "${SUITES[@]}"; do
			name="${spec%%|*}"
			bin="$(cut -d'|' -f2 <<<"$spec")"
			args="$(cut -d'|' -f3 <<<"$spec")"
			echo "run_suite $name /tests/$bin $args"
		done
		echo 'echo RACER_BLOCK_GUEST_DONE'
		echo 'poweroff -n -f'
	} >"$root/init"
	chmod +x "$root/init"
	(cd "$root" && find . -print0 | cpio --null -o -H newc --quiet) | gzip -1 >"$WORK_DIR/initramfs.cpio.gz"
}

run_vm() {
	local version="$1" console="$WORK_DIR/console.log" accel="$VM_ACCEL" status=0
	if [[ "$accel" == auto ]]; then
		if [[ -r /dev/kvm && -w /dev/kvm ]]; then accel=kvm; else accel=tcg; fi
	fi
	log "booting guest (accel=$accel, timeout=${VM_TIMEOUT}s)"
	: >"$console"
	timeout --signal=TERM --kill-after=10s "$VM_TIMEOUT" qemu-system-x86_64 \
		-machine "q35,accel=$accel" -cpu max -smp "$VM_CPUS" -m "$VM_MEMORY" \
		-kernel "$KERNEL_PATH" \
		-initrd "$WORK_DIR/initramfs.cpio.gz" \
		-append "console=ttyS0 rdinit=/init panic=-1 oops=panic quiet" \
		-nographic -no-reboot -nodefaults -serial stdio -monitor none \
		</dev/null | tee "$console" || status=$?
	if ((status == 124)); then
		log "error: guest exceeded ${VM_TIMEOUT}s"
	fi
	# QEMU's exit status alone does not prove the tests ran, so judge the log.
	check_results "$console" && ((status != 124))
}

self_test() {
	local rc=0
	dir="$(mktemp -d "${TMPDIR:-/tmp}/racer-block-vm-selftest.XXXXXX")"
	trap 'rm -rf "$dir"' EXIT
	expect() {
		local want="$1" case="$2" got=0
		check_results "$dir/$case.log" 2>/dev/null || got=1
		if [[ "$got" != "$want" ]]; then
			log "self-test $case: want $want got $got"
			rc=1
		fi
	}
	local pass='RACER_BLOCK_SUITE_RESULT ublk 0
--- PASS: TestEndToEndDevice (0.10s)
RACER_BLOCK_SUITE_RESULT racerblock 0
RACER_BLOCK_GUEST_DONE'
	printf '%s\n' "$pass" >"$dir/pass.log"
	expect 0 pass
	printf '%s\n' "--- SKIP: TestIntegrationDeleteStale (0.00s)" "    x_test.go:450: UBLK_TEST_DELETE_IDS not set" "$pass" >"$dir/benign-skip.log"
	expect 0 benign-skip
	printf '%s\n' "--- SKIP: TestIntegration (0.00s)" "    x_test.go:45: /dev/ublk-control unavailable: no such file" "$pass" >"$dir/ublk-skip.log"
	expect 1 ublk-skip
	printf '%s\n' "--- SKIP: TestX (0.00s)" "    x_test.go:37: requires root" "$pass" >"$dir/root-skip.log"
	expect 1 root-skip
	printf '%s\n' "--- SKIP: TestStartupFault (0.00s)" "    x_test.go:9: set RACER_BLOCK_TEST_VM=1 to run" "$pass" >"$dir/vm-skip.log"
	expect 1 vm-skip
	printf '%s\n' 'RACER_BLOCK_SUITE_RESULT ublk 0' 'Kernel panic - not syncing: Fatal exception' >"$dir/panic.log"
	expect 1 panic
	printf '%s\n' 'RACER_BLOCK_SUITE_RESULT ublk 0' >"$dir/timeout.log"
	expect 1 timeout
	sed 's/racerblock 0/racerblock 1/' "$dir/pass.log" >"$dir/fail.log"
	expect 1 fail
	printf '%s\n' 'RACER_BLOCK_GUEST_FATAL /dev/ublk-control missing' 'RACER_BLOCK_GUEST_DONE' >"$dir/fatal.log"
	expect 1 fatal
	expect 1 missing-log
	if (PATH=/nonexistent require_tools qemu-system-x86_64) 2>/dev/null; then
		log "self-test missing-tools: want failure"
		rc=1
	fi
	((rc == 0)) && log "self-test passed"
	return "$rc"
}

main() {
	case "${1:-}" in
	run)
		require_tools qemu-system-x86_64 busybox cpio gzip zstd ldd modinfo file timeout tee
		mkdir -p "$WORK_DIR"
		local version
		if [[ -n "$KERNEL_IMAGE" ]]; then
			[[ -f "$KERNEL_IMAGE" && -d "$KERNEL_MODULES" && -n "$KERNEL_VERSION" ]] ||
				die "KERNEL_IMAGE needs a KERNEL_MODULES directory and KERNEL_VERSION"
			version="$KERNEL_VERSION"
			KERNEL_PATH="$KERNEL_IMAGE"
			MODULES_DIR="$KERNEL_MODULES"
		else
			version="$(fetch_kernel | tail -n 1)"
			KERNEL_PATH="$WORK_DIR/kernel-root/boot/vmlinuz-$version"
			MODULES_DIR="$WORK_DIR/kernel-root/lib/modules/$version"
		fi
		build_initramfs "$version"
		run_vm "$version"
		;;
	self-test) self_test ;;
	*) die "usage: $0 run|self-test" ;;
	esac
}

main "$@"
