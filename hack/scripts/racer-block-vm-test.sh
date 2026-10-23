#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Run the racer-block ublk device tests inside a disposable QEMU guest.
#
# The guest boots an Ubuntu generic kernel chosen by GUEST_KERNEL: 6.8 from
# Ubuntu 24.04 (noble), the minimum racer-block supports, or 7.0 from Ubuntu
# 26.04 (resolute). It uses an initramfs built from busybox-static, the
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
# full ublk package and TestEndToEndDevice as root. The job fails unless QEMU
# exits zero, every suite exits zero and prints PASS, every required test
# prints PASS, and every skipped test is a manual-only test.
#
# Environment:
#   TEST_BIN_DIR      test binaries (default bin/racer-block-tests)
#   GUEST_KERNEL      guest kernel family: 6.8 or 7.0 (default 6.8)
#   WORK_DIR          scratch output (default bin/racer-block-vm/$GUEST_KERNEL)
#   KERNEL_PACKAGE    kernel meta package (default linux-image-generic)
#   ARCHIVE_URL       Ubuntu archive (default http://archive.ubuntu.com/ubuntu)
#   ARCHIVE_KEYRING   archive signing keys
#                     (default /usr/share/keyrings/ubuntu-archive-keyring.gpg)
#   VM_TIMEOUT        seconds before QEMU is terminated (default 600)
#   VM_MEMORY         guest memory (default 4G)
#   VM_CPUS           guest CPUs (default 2)
#   VM_ACCEL          kvm, tcg, or auto (default auto: KVM, else TCG)
#   SUITE_TIMEOUT     guest watchdog seconds per suite (default 270)
#   QEMU              QEMU binary (default qemu-system-x86_64)
#   KERNEL_IMAGE      prebuilt kernel image; skips the apt download
#   KERNEL_MODULES    directory searched for ublk_drv.ko* (with KERNEL_IMAGE)
#   KERNEL_VERSION    expected guest uname -r (with KERNEL_IMAGE)
#
# The kernel packages are downloaded with a private APT configuration under
# WORK_DIR that reads only the chosen release from ARCHIVE_URL and checks its
# signature against ARCHIVE_KEYRING. Host APT settings and package state are
# not read or changed, and nothing is installed.
#
# KERNEL_IMAGE, KERNEL_MODULES, and KERNEL_VERSION use a local kernel instead.
# KERNEL_VERSION must still belong to GUEST_KERNEL.

set -euo pipefail

TEST_BIN_DIR="${TEST_BIN_DIR:-bin/racer-block-tests}"
GUEST_KERNEL="${GUEST_KERNEL:-6.8}"
WORK_DIR="${WORK_DIR:-bin/racer-block-vm/$GUEST_KERNEL}"
ARCHIVE_URL="${ARCHIVE_URL:-http://archive.ubuntu.com/ubuntu}"
ARCHIVE_KEYRING="${ARCHIVE_KEYRING:-/usr/share/keyrings/ubuntu-archive-keyring.gpg}"
KERNEL_PACKAGE="${KERNEL_PACKAGE:-linux-image-generic}"
VM_TIMEOUT="${VM_TIMEOUT:-600}"
VM_MEMORY="${VM_MEMORY:-4G}"
VM_CPUS="${VM_CPUS:-2}"
VM_ACCEL="${VM_ACCEL:-auto}"
SUITE_TIMEOUT="${SUITE_TIMEOUT:-270}"
QEMU="${QEMU:-qemu-system-x86_64}"
KERNEL_IMAGE="${KERNEL_IMAGE:-}"
KERNEL_MODULES="${KERNEL_MODULES:-}"
KERNEL_VERSION="${KERNEL_VERSION:-}"

# Each suite is "name|binary|arguments". The 4m test timeout fires before the
# 270s guest watchdog, which fires before the 600s host limit, so a hang is
# reported by the innermost layer that can still report it.
SUITES=(
	"ublk|ublk.test|-test.timeout=4m -test.v"
	"racerblock|racerblock.test|-test.timeout=4m -test.v -test.run=^TestEndToEndDevice\$"
)

# Tests that must print "--- PASS" in each suite. A suite where these skip
# fails even though the binary exits zero.
# shellcheck disable=SC2034 # read through ${!required}
REQUIRED_ublk=(
	TestIntegrationDeleteFallbackClose
	TestIntegrationDeleteFallbackExternalStop
	TestIntegrationDeleteFallbackCancelFirst
	TestIntegrationDeleteFallbackExternalDelete
	TestIntegrationExternalDelete
	TestIntegrationExternalStop
	TestIntegrationReadOnlyDevice
	TestIntegrationContextCancel
	TestIntegrationFixedIDAndDelete
	TestIntegrationStartValidation
	TestIntegrationDeleteStale
	TestIntegrationStartCancelDuringStartup
	TestIntegrationStartCancelBeforeStartDev
	TestIntegrationStartupFetchRejected
	TestIntegrationStartupQueueFailsBeforeStartDev
	TestIntegrationStartupQueueFailsAtStartDev
	TestIntegrationStartupFailedQueueAnswersAfterStartup
	TestIntegrationStartupBrokenRingAtStartDev
	TestIntegrationFatalRingWithDeliveredRequest
	TestIntegrationFatalRingDuringStopDev
	TestIntegrationFatalRingDuringTeardown
	TestIntegrationStartFailsWithoutSpareRing
	TestDrainKeepsCompletionsAfterPrepFailure
	TestRescueUsesSpareRingWithoutNewRing
	TestNewQueueFailsWithoutSpareRing
	TestReleaseClosesUnusedSpareRingOnce
	TestQueueRingHoldsEveryCompletion
	TestServeEIOServesAfterStartup
	TestServeEIOSubmitsAnswerBeforeStopping
	TestServeEIOSeesStopConsumedByDrain
	TestCloseWinsTeardownKeepsFailure
	TestCancelWinsTeardownKeepsFailure
	TestLateFailureDuringTeardownKept
	TestServeErrorRecordedWhenCloseWinsAfterServe
	TestServeErrorRecordedWhenStopWinsAfterServe
	TestServeErrorHoldDecidedBeforeTeardown
	TestCtrlPayloadUsesStablePage
	TestURingNop
)
# shellcheck disable=SC2034 # read through ${!required}
REQUIRED_racerblock=(
	TestEndToEndDevice
)

# The only tests allowed to skip. They need manual setup (UBLK_TEST_FIO=1 or
# UBLK_TEST_DELETE_IDS) and are not run by CI. Any other skip fails, which
# covers a missing root, ublk, io_uring, or RACER_BLOCK_TEST_VM.
ALLOWED_SKIPS='TestIntegrationDeleteIDs|TestIntegrationFio'

# Go prints a skip reason before "--- SKIP", so these are matched anywhere.
FATAL_SKIP_PATTERN='RACER_BLOCK_TEST_VM=1|requires root|/dev/ublk-control unavailable|io_uring unavailable'

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

# suite_section TEXT NAME prints the output of one suite.
suite_section() {
	awk -v n="$2" '
		$0 == "RACER_BLOCK_SUITE_START " n { on = 1; next }
		on && index($0, "RACER_BLOCK_SUITE_RESULT " n " ") == 1 { exit }
		on { print }' <<<"$1"
}

# check_results LOG QEMU_STATUS prints a verdict and returns nonzero unless
# QEMU exited zero and every suite fully passed.
check_results() {
	local console="$1" qemu_status="${2:-0}" spec name status section test required text bad rc=0
	[[ -f "$console" ]] || {
		log "error: console log $console missing"
		return 1
	}
	if [[ "$qemu_status" != 0 ]]; then
		log "error: QEMU exited $qemu_status"
		rc=1
	fi
	# Read the log once. Searching a variable avoids pipelines where an early
	# exit (grep -q, head) makes pipefail report SIGPIPE instead of the match.
	text="$(tr -d '\r' <"$console")"
	if grep -qE 'Kernel panic|BUG: |Oops: |RACER_BLOCK_GUEST_FATAL' <<<"$text"; then
		log "error: guest reported a fatal error:"
		grep -m 20 -E 'Kernel panic|BUG: |Oops: |RACER_BLOCK_GUEST_FATAL' <<<"$text" >&2
		rc=1
	fi
	if grep -qE "$FATAL_SKIP_PATTERN" <<<"$text"; then
		log "error: a test reported missing root, ublk, io_uring, or RACER_BLOCK_TEST_VM:"
		grep -m 20 -E "$FATAL_SKIP_PATTERN" <<<"$text" >&2
		rc=1
	fi
	bad="$(grep -E '^ *--- SKIP: ' <<<"$text" | grep -vE "^ *--- SKIP: ($ALLOWED_SKIPS)( |/)" || true)"
	if [[ -n "$bad" ]]; then
		grep -m 20 . <<<"$bad" >&2
		log "error: tests above skipped and are not manual-only"
		rc=1
	fi
	for spec in "${SUITES[@]}"; do
		name="${spec%%|*}"
		status="$(sed -n "s/^RACER_BLOCK_SUITE_RESULT ${name} \([0-9][0-9]*\)$/\1/p" <<<"$text" | tail -n 1)"
		if [[ -z "$status" ]]; then
			log "error: suite $name reported no result (guest crash or timeout)"
			rc=1
			continue
		elif [[ "$status" != 0 ]]; then
			log "error: suite $name exited $status"
			rc=1
		fi
		section="$(suite_section "$text" "$name")"
		if ! grep -qx 'PASS' <<<"$section"; then
			log "error: suite $name did not print PASS"
			rc=1
		fi
		required="REQUIRED_${name}[@]"
		for test in "${!required}"; do
			if ! grep -qE "^--- PASS: $test \(" <<<"$section"; then
				log "error: suite $name: $test did not PASS"
				rc=1
			fi
		done
		((rc == 0)) && log "suite $name passed"
	done
	if ! grep -qx 'RACER_BLOCK_GUEST_DONE' <<<"$text"; then
		log "error: guest did not finish"
		rc=1
	fi
	return "$rc"
}

# kernel_release FAMILY prints the Ubuntu release whose generic kernel is in
# FAMILY.
kernel_release() {
	case "$1" in
	6.8) echo noble ;;
	7.0) echo resolute ;;
	*)
		log "error: GUEST_KERNEL must be 6.8 or 7.0, not '$1'"
		return 1
		;;
	esac
}

# check_family VERSION fails unless VERSION is in the GUEST_KERNEL family:
# exactly GUEST_KERNEL, or GUEST_KERNEL followed by ".", "-", or "+". Any
# flavor suffix is allowed so local overrides such as 6.8.0-1017-azure work.
# Downloads separately require an Ubuntu "-generic" image.
check_family() {
	case "$1" in
	"$GUEST_KERNEL" | "$GUEST_KERNEL".* | "$GUEST_KERNEL"-* | "$GUEST_KERNEL"+*) ;;
	*)
		log "error: expected a Linux $GUEST_KERNEL kernel, got '$1'"
		return 1
		;;
	esac
}

# apt_setup DIR RELEASE writes a private APT configuration under DIR that
# reads only RELEASE from ARCHIVE_URL, verified with ARCHIVE_KEYRING. Every
# step is checked so a failure returns nonzero even where errexit is off, such
# as inside a command substitution or an "||" list.
apt_setup() {
	local dir="$1" release="$2" suite abs
	rm -rf "$dir" || return 1
	mkdir -p "$dir/etc/apt.conf.d" "$dir/etc/preferences.d" "$dir/etc/trusted.gpg.d" \
		"$dir/etc/sources.list.d" "$dir/state/lists/partial" "$dir/cache/archives/partial" || return 1
	: >"$dir/state/status" || return 1
	abs="$(cd "$dir" && pwd)" || return 1
	for suite in "$release" "$release-updates" "$release-security"; do
		echo "deb [arch=amd64 signed-by=$ARCHIVE_KEYRING] $ARCHIVE_URL $suite main"
	done >"$dir/etc/sources.list" || return 1
	cat >"$dir/apt.conf" <<EOF || return 1
Dir "/";
Dir::Etc "$abs/etc";
Dir::Etc::main "/dev/null";
Dir::Etc::parts "$abs/etc/apt.conf.d";
Dir::Etc::sourcelist "$abs/etc/sources.list";
Dir::Etc::sourceparts "$abs/etc/sources.list.d";
Dir::Etc::preferences "/dev/null";
Dir::Etc::preferencesparts "$abs/etc/preferences.d";
Dir::Etc::trusted "/dev/null";
Dir::Etc::trustedparts "$abs/etc/trusted.gpg.d";
Dir::State "$abs/state";
Dir::State::status "$abs/state/status";
Dir::Cache "$abs/cache";
APT::Architecture "amd64";
APT::Architectures { "amd64"; };
APT::Get::AllowUnauthenticated "false";
Acquire::AllowInsecureRepositories "false";
Acquire::Languages "none";
Acquire::Retries "3";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
Debug::NoLocking "true";
EOF
}

# kernel_packages VERSION reads meta package dependencies on stdin and prints
# the image and module packages for VERSION. It fails unless both the image
# and the base modules package are present.
kernel_packages() {
	local deps
	deps="$(grep -E "^linux-(image|modules|modules-extra)-${1//./\\.}\$" || true)"
	if ! grep -qx "linux-image-$1" <<<"$deps" || ! grep -qx "linux-modules-$1" <<<"$deps"; then
		log "error: kernel $1 meta package lacks linux-image or linux-modules"
		return 1
	fi
	echo "$deps"
}

# fetch_kernel downloads and unpacks the kernel image and modules without
# installing them, so the runner's own kernel, /boot, and APT are untouched.
fetch_kernel() {
	local pkgdir="$WORK_DIR/pkgs" root="$WORK_DIR/kernel-root" aptdir="$WORK_DIR/apt"
	local release deps image version deb pkgs
	require_tools apt-cache apt-get dpkg-deb
	release="$(kernel_release "$GUEST_KERNEL")" || exit 1
	[[ -r "$ARCHIVE_KEYRING" ]] || die "archive keyring $ARCHIVE_KEYRING not readable"
	# main calls this inside $(...), where errexit is off, so check each step
	# explicitly. Without a private config, apt-get would read the host's.
	apt_setup "$aptdir" "$release" || die "cannot create private APT configuration in $aptdir"
	APT_CONFIG="$(cd "$aptdir" && pwd)" || die "cannot resolve $aptdir"
	APT_CONFIG+=/apt.conf
	[[ -f "$APT_CONFIG" && -f "$aptdir/etc/sources.list" ]] || die "private APT configuration missing in $aptdir"
	export APT_CONFIG
	log "updating private APT lists for $release"
	timeout --signal=TERM --kill-after=10s 300s apt-get -q update >&2 ||
		die "apt-get update for $release failed"
	deps="$(apt-cache depends "$KERNEL_PACKAGE" | sed -n 's/^ *Depends: \(linux-[^ ]*\)$/\1/p')"
	image="$(grep -m 1 -E '^linux-image-[0-9][^ ]*-generic$' <<<"$deps" || true)"
	[[ -n "$image" ]] || die "cannot resolve kernel image from $KERNEL_PACKAGE in $release"
	version="${image#linux-image-}"
	check_family "$version" || exit 1
	# 6.8 ships ublk_drv in linux-modules-extra. 7.0 has no extra package and
	# ships it in linux-modules. Take whichever the meta package depends on.
	# noble's meta package reaches linux-modules through the image package.
	deps+=$'\n'"$(apt-cache depends "$image" | sed -n 's/^ *Depends: \(linux-[^ ]*\)$/\1/p')"
	pkgs="$(kernel_packages "$version" <<<"$deps")" || exit 1
	mapfile -t pkgs <<<"$pkgs"
	log "using kernel $version from $release"
	rm -rf "$pkgdir" "$root"
	mkdir -p "$pkgdir" "$root"
	(cd "$pkgdir" && timeout --signal=TERM --kill-after=10s 300s apt-get -q download \
		"${pkgs[@]}" >&2) ||
		die "downloading kernel $version failed"
	for deb in "$pkgdir"/*.deb; do
		dpkg-deb -x "$deb" "$root"
	done
	[[ -f "$root/boot/vmlinuz-$version" ]] || die "vmlinuz-$version missing from packages"
	[[ -d "$root/lib/modules/$version" || -d "$root/usr/lib/modules/$version" ]] ||
		die "modules for $version missing from packages"
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

# accel_arg prints the QEMU accelerator for VM_ACCEL. "auto" asks QEMU to try
# KVM and fall back to TCG when KVM cannot start. kvm and tcg are strict.
accel_arg() {
	case "$1" in
	auto) echo kvm:tcg ;;
	kvm | tcg) echo "$1" ;;
	*)
		log "error: VM_ACCEL must be auto, kvm, or tcg, not '$1'"
		return 1
		;;
	esac
}

run_vm() {
	local console="$WORK_DIR/console.log" accel status=0
	accel="$(accel_arg "$VM_ACCEL")" || return 1
	log "booting guest (accel=$accel, timeout=${VM_TIMEOUT}s)"
	: >"$console"
	timeout --signal=TERM --kill-after=10s "$VM_TIMEOUT" "$QEMU" \
		-machine "q35,accel=$accel" -cpu max -smp "$VM_CPUS" -m "$VM_MEMORY" \
		-kernel "$KERNEL_PATH" \
		-initrd "$WORK_DIR/initramfs.cpio.gz" \
		-append "console=ttyS0 rdinit=/init panic=-1 oops=panic quiet" \
		-nographic -no-reboot -nodefaults -serial stdio -monitor none \
		</dev/null | tee "$console" || status=$?
	if ((status == 124)); then
		log "error: guest exceeded ${VM_TIMEOUT}s"
	fi
	# Both must hold: a clean QEMU exit does not prove the tests ran, and a
	# complete log does not excuse a QEMU crash or kill.
	check_results "$console" "$status"
}

# pass_log prints a realistic passing console log in go test -v format.
pass_log() {
	local t
	echo 'RACER_BLOCK_GUEST_KERNEL 6.8.0-147-generic'
	echo 'RACER_BLOCK_SUITE_START ublk'
	for t in "${REQUIRED_ublk[@]}"; do
		printf '=== RUN   %s\n--- PASS: %s (0.01s)\n' "$t" "$t"
	done
	printf '=== RUN   TestIntegrationDeleteIDs\n    integration_linux_test.go:450: UBLK_TEST_DELETE_IDS not set\n--- SKIP: TestIntegrationDeleteIDs (0.00s)\n'
	printf '=== RUN   TestIntegrationFio\n    integration_linux_test.go:536: UBLK_TEST_FIO not set\n--- SKIP: TestIntegrationFio (0.00s)\n'
	printf 'PASS\nRACER_BLOCK_SUITE_RESULT ublk 0\n'
	echo 'RACER_BLOCK_SUITE_START racerblock'
	printf '=== RUN   TestEndToEndDevice\n--- PASS: TestEndToEndDevice (0.20s)\nPASS\n'
	printf 'RACER_BLOCK_SUITE_RESULT racerblock 0\nRACER_BLOCK_GUEST_DONE\n'
}

# skip_log TEST REASON replaces TEST's PASS with a realistic skip.
skip_log() {
	pass_log | awk -v t="$1" -v why="$2" '
		$0 == "--- PASS: " t " (0.01s)" || $0 == "--- PASS: " t " (0.20s)" {
			print "    x_test.go:41: " why; print "--- SKIP: " t " (0.00s)"; next }
		{ print }'
}

self_test() {
	local rc=0 got
	dir="$(mktemp -d "${TMPDIR:-/tmp}/racer-block-vm-selftest.XXXXXX")"
	trap 'rm -rf "$dir"' EXIT
	expect() {
		local want="$1" case="$2" status="${3:-0}" got=0
		check_results "$dir/$case.log" "$status" 2>/dev/null || got=1
		if [[ "$got" != "$want" ]]; then
			log "self-test $case: want $want got $got"
			rc=1
		fi
	}
	pass_log >"$dir/pass.log"
	expect 0 pass
	pass_log | sed 's/$/\r/' >"$dir/crlf.log"
	expect 0 crlf
	expect 1 pass 1
	expect 1 pass 124
	expect 1 pass 137
	skip_log TestIntegrationReadOnlyDevice "requires root" >"$dir/root-skip.log"
	expect 1 root-skip
	skip_log TestIntegrationExternalStop "/dev/ublk-control unavailable: no such file or directory" >"$dir/ublk-skip.log"
	expect 1 ublk-skip
	skip_log TestURingNop "io_uring unavailable: operation not permitted" >"$dir/uring-skip.log"
	expect 1 uring-skip
	skip_log TestIntegrationStartupFetchRejected "startup fault tests need RACER_BLOCK_TEST_VM=1 in a disposable VM" >"$dir/vm-skip.log"
	expect 1 vm-skip
	skip_log TestEndToEndDevice "requires root" >"$dir/e2e-skip.log"
	expect 1 e2e-skip
	skip_log TestIntegrationContextCancel "some new reason" >"$dir/unknown-skip.log"
	expect 1 unknown-skip
	skip_log TestIntegrationDeleteStale "/dev/ublk-control unavailable: no such file or directory" |
		grep -v 'ublk-control unavailable' >"$dir/stale-skip.log"
	expect 1 stale-skip
	# Large logs: an early fatal marker or skip followed by over 1 MiB of
	# passing output must still be found.
	{
		echo 'RACER_BLOCK_GUEST_FATAL early'
		awk 'BEGIN { for (i = 0; i < 30000; i++) print "    integration_linux_test.go:1: passing output line padding" }'
		pass_log
	} >"$dir/large-fatal.log"
	expect 1 large-fatal
	{
		printf -- '--- SKIP: TestSomethingNew (0.00s)\n'
		awk 'BEGIN { for (i = 0; i < 30000; i++) print "    integration_linux_test.go:1: passing output line padding" }'
		pass_log
	} >"$dir/large-skip.log"
	expect 1 large-skip
	{
		awk 'BEGIN { for (i = 0; i < 30000; i++) print "    integration_linux_test.go:1: passing output line padding" }'
		pass_log
	} >"$dir/large-pass.log"
	expect 0 large-pass
	pass_log | grep -v 'TestIntegrationStartupBrokenRingAtStartDev' >"$dir/missing-startup.log"
	expect 1 missing-startup
	pass_log | grep -vx 'PASS' >"$dir/no-pass.log"
	expect 1 no-pass
	pass_log | sed 's/^RACER_BLOCK_SUITE_RESULT racerblock 0$/RACER_BLOCK_SUITE_RESULT racerblock 1/' >"$dir/fail.log"
	expect 1 fail
	pass_log | sed '/RACER_BLOCK_SUITE_START racerblock/,$d' >"$dir/timeout.log"
	expect 1 timeout
	{ pass_log; echo 'Kernel panic - not syncing: Fatal exception'; } >"$dir/panic.log"
	expect 1 panic
	printf '%s\n' 'RACER_BLOCK_GUEST_FATAL suite ublk hung for 270s' 'RACER_BLOCK_GUEST_DONE' >"$dir/fatal.log"
	expect 1 fatal
	expect 1 missing-log

	# accel_arg: auto falls back inside QEMU, explicit values stay strict.
	[[ "$(accel_arg auto)" == kvm:tcg && "$(accel_arg kvm)" == kvm && "$(accel_arg tcg)" == tcg ]] || {
		log "self-test accel: wrong mapping"
		rc=1
	}
	if accel_arg bogus 2>/dev/null; then
		log "self-test accel: bogus value accepted"
		rc=1
	fi

	# run_vm with a stub QEMU: a passing log must not hide a failing exit.
	mkdir -p "$dir/work"
	pass_log >"$dir/stub.out"
	# shellcheck disable=SC2016 # expanded by the stub
	printf '#!/bin/sh\ncat %q\nexit "${STUB_EXIT:-0}"\n' "$dir/stub.out" >"$dir/qemu"
	chmod +x "$dir/qemu"
	for got in 0 1; do
		if (WORK_DIR="$dir/work" QEMU="$dir/qemu" KERNEL_PATH=none STUB_EXIT=$got run_vm >/dev/null 2>&1); then
			[[ $got == 0 ]] || {
				log "self-test run_vm: QEMU exit $got accepted"
				rc=1
			}
		else
			[[ $got != 0 ]] || {
				log "self-test run_vm: clean run rejected"
				rc=1
			}
		fi
	done
	if (WORK_DIR="$dir/work" QEMU="$dir/qemu" VM_ACCEL=bogus KERNEL_PATH=none run_vm >/dev/null 2>&1); then
		log "self-test run_vm: bogus VM_ACCEL accepted"
		rc=1
	fi

	# Kernel family selection.
	[[ "$(kernel_release 6.8)" == noble && "$(kernel_release 7.0)" == resolute ]] || {
		log "self-test kernel_release: wrong mapping"
		rc=1
	}
	for got in 6.9 7 "" 6.8.0 bogus; do
		if kernel_release "$got" >/dev/null 2>&1; then
			log "self-test kernel_release: '$got' accepted"
			rc=1
		fi
	done
	for got in 6.8.0-147-generic 6.8.0-1017-azure 6.8.12 6.8.0 6.8 6.8-rc1 6.8.0+custom; do
		if ! (GUEST_KERNEL=6.8 check_family "$got") 2>/dev/null; then
			log "self-test check_family: '$got' rejected for 6.8"
			rc=1
		fi
	done
	for got in 7.0.0-38-generic 7.0.0-1003-azure 7.0.1; do
		if ! (GUEST_KERNEL=7.0 check_family "$got") 2>/dev/null; then
			log "self-test check_family: '$got' rejected for 7.0"
			rc=1
		fi
	done
	for got in 7.0.0-14-generic 6.80.0-1-generic 6.81 6.8foo 6.9.0-1-generic 16.8.0 v6.8.0 ""; do
		if (GUEST_KERNEL=6.8 check_family "$got") 2>/dev/null; then
			log "self-test check_family: '$got' accepted for 6.8"
			rc=1
		fi
	done
	for got in 6.8.0-147-generic 7.00.1 7.01-generic 7.1.0-1-generic; do
		if (GUEST_KERNEL=7.0 check_family "$got") 2>/dev/null; then
			log "self-test check_family: '$got' accepted for 7.0"
			rc=1
		fi
	done
	# Overrides must match GUEST_KERNEL and are rejected before booting.
	: >"$dir/vmlinuz"
	mkdir -p "$dir/mods"
	got="$( (GUEST_KERNEL=6.8 KERNEL_IMAGE="$dir/vmlinuz" KERNEL_MODULES="$dir/mods" \
		KERNEL_VERSION=7.0.0-14-generic WORK_DIR="$dir/ovr" QEMU=true main run) 2>&1 || true)"
	if ! grep -q 'expected a Linux 6.8 kernel' <<<"$got"; then
		log "self-test override: wrong family accepted"
		rc=1
	fi

	# Package selection for 6.8 (with modules-extra) and 7.0 (without).
	got="$(printf '%s\n' linux-image-6.8.0-1-generic linux-modules-6.8.0-1-generic \
		linux-modules-extra-6.8.0-1-generic linux-firmware linux-image-6.8.0-10-generic |
		kernel_packages 6.8.0-1-generic | tr '\n' ' ')"
	[[ "$got" == "linux-image-6.8.0-1-generic linux-modules-6.8.0-1-generic linux-modules-extra-6.8.0-1-generic " ]] || {
		log "self-test kernel_packages 6.8: got '$got'"
		rc=1
	}
	got="$(printf '%s\n' linux-image-7.0.0-38-generic linux-modules-7.0.0-38-generic \
		linux-main-modules-zfs-7.0.0-38-generic | kernel_packages 7.0.0-38-generic | tr '\n' ' ')"
	[[ "$got" == "linux-image-7.0.0-38-generic linux-modules-7.0.0-38-generic " ]] || {
		log "self-test kernel_packages 7.0: got '$got'"
		rc=1
	}
	if printf '%s\n' linux-image-7.0.0-38-generic | kernel_packages 7.0.0-38-generic >/dev/null 2>&1; then
		log "self-test kernel_packages: missing modules accepted"
		rc=1
	fi

	# A same-family custom override passes the family check. It then stops at
	# the missing test binaries, before any VM boots.
	got="$( (GUEST_KERNEL=6.8 KERNEL_IMAGE="$dir/vmlinuz" KERNEL_MODULES="$dir/mods" \
		KERNEL_VERSION=6.8.0-1017-azure WORK_DIR="$dir/ovr" TEST_BIN_DIR="$dir/none" QEMU=true main run) 2>&1 || true)"
	if grep -q 'expected a Linux' <<<"$got" || ! grep -q 'kernel module ublk_drv not found\|test binary' <<<"$got"; then
		log "self-test override: same-family azure kernel not accepted: $got"
		rc=1
	fi

	# Private APT configuration: only the chosen release, signed, no host files.
	(cd "$dir" && ARCHIVE_KEYRING=/k.gpg ARCHIVE_URL=http://a/ubuntu apt_setup apt resolute)
	if [[ "$(cat "$dir/apt/etc/sources.list")" != "deb [arch=amd64 signed-by=/k.gpg] http://a/ubuntu resolute main
deb [arch=amd64 signed-by=/k.gpg] http://a/ubuntu resolute-updates main
deb [arch=amd64 signed-by=/k.gpg] http://a/ubuntu resolute-security main" ]]; then
		log "self-test apt_setup: unexpected sources.list"
		rc=1
	fi
	if grep -E '"/etc/|AllowUnauthenticated "true"|AllowInsecureRepositories "true"' "$dir/apt/apt.conf" >/dev/null ||
		! grep -qF "Dir::State::status \"$dir/apt/state/status\";" "$dir/apt/apt.conf" ||
		[[ -n "$(find "$dir/apt/etc" -type f ! -name sources.list)" ]]; then
		log "self-test apt_setup: config reads host APT files or allows unsigned data"
		rc=1
	fi

	# A failed private APT setup must stop fetch_kernel before any apt-get or
	# apt-cache call, even inside the command substitution main uses, where
	# errexit does not apply. Stubs record calls instead of touching APT.
	mkdir -p "$dir/stub"
	for got in apt-get apt-cache dpkg-deb; do
		printf '#!/bin/sh\necho "%s $*" >>%q\nexit 1\n' "$got" "$dir/apt-calls" >"$dir/stub/$got"
		chmod +x "$dir/stub/$got"
	done
	: >"$dir/keyring.gpg"
	: >"$dir/not-a-dir"
	for got in stub-fails mkdir-fails; do
		rm -f "$dir/apt-calls"
		(
			PATH="$dir/stub:$PATH"
			ARCHIVE_KEYRING="$dir/keyring.gpg"
			work="$dir/fetch"
			if [[ "$got" == stub-fails ]]; then
				apt_setup() { return 1; }
			else
				work="$dir/not-a-dir"
			fi
			version="$(WORK_DIR="$work" fetch_kernel | tail -n 1)"
			[[ -z "$version" ]] || exit 0
		) >/dev/null 2>&1 || true
		if [[ -e "$dir/apt-calls" ]]; then
			log "self-test fetch_kernel $got: APT ran after setup failed: $(tr '\n' ';' <"$dir/apt-calls")"
			rc=1
		fi
	done

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
		require_tools "$QEMU" busybox cpio gzip zstd ldd modinfo file timeout tee
		mkdir -p "$WORK_DIR"
		local version
		if [[ -n "$KERNEL_IMAGE" ]]; then
			[[ -f "$KERNEL_IMAGE" && -d "$KERNEL_MODULES" && -n "$KERNEL_VERSION" ]] ||
				die "KERNEL_IMAGE needs a KERNEL_MODULES directory and KERNEL_VERSION"
			kernel_release "$GUEST_KERNEL" >/dev/null || exit 1
			check_family "$KERNEL_VERSION" || exit 1
			version="$KERNEL_VERSION"
			KERNEL_PATH="$KERNEL_IMAGE"
			MODULES_DIR="$KERNEL_MODULES"
		else
			version="$(fetch_kernel | tail -n 1)"
			KERNEL_PATH="$WORK_DIR/kernel-root/boot/vmlinuz-$version"
			# 7.0 installs modules under /usr/lib/modules, 6.8 under /lib/modules.
			MODULES_DIR="$WORK_DIR/kernel-root/lib/modules/$version"
			[[ -d "$MODULES_DIR" ]] || MODULES_DIR="$WORK_DIR/kernel-root/usr/lib/modules/$version"
		fi
		build_initramfs "$version"
		run_vm
		;;
	self-test) self_test ;;
	*) die "usage: $0 run|self-test" ;;
	esac
}

main "$@"
