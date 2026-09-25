#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -eu

: "${OVERLAYBD_P2P_ADDRESS:?OVERLAYBD_P2P_ADDRESS is required}"
: "${HOST_ROOT:=/host}"
: "${OVERLAYBD_CONFIG_TOOL:=/opt/acr/tools/overlaybd/config.sh}"
: "${GANTRY_STREAMING_READYZ:=http://localhost:5000/artifact-streaming/readyz}"
: "${GANTRY_READY_TIMEOUT_SECONDS:=60}"
: "${NSENTER_BIN:=nsenter}"
: "${CURL_BIN:=curl}"
: "${JQ_BIN:=jq}"
: "${READY_MARKER:=/run/gantry-overlaybd/configured}"
: "${HOST_STATE_DIR:=/var/lib/gantry/overlaybd-config}"

HOST_CONFIG="$HOST_ROOT/etc/overlaybd/overlaybd.json"
STATE_DIR="$HOST_ROOT/var/lib/gantry/overlaybd-config"
ORIGINAL_CONFIG="$STATE_DIR/original.json"
MANAGED_CONFIG="$STATE_DIR/managed.json"

umask 077
mkdir -p "$STATE_DIR" "$(dirname "$READY_MARKER")"

acquire_lock() {
	exec 9>"$STATE_DIR/lock"
	flock -x 9
}

release_lock() {
	flock -u 9
	exec 9>&-
}

host_exec() {
	"$NSENTER_BIN" -t 1 -m -u -i -n -p --root=/proc/1/root --wd=/ -- "$@"
}

# Change directory after nsenter because its --wd resolves inside this container.
host_exec_writable() {
	host_exec sh -c 'cd "$1" || exit 1; shift; exec "$@"' sh "$HOST_STATE_DIR" "$@"
}

restart_services() {
	host_exec systemctl restart overlaybd-tcmu
	host_exec systemctl restart overlaybd-snapshotter
	host_exec systemctl is-active --quiet overlaybd-tcmu
	host_exec systemctl is-active --quiet overlaybd-snapshotter
}

wait_for_gantry() {
	elapsed=0
	while ! "$CURL_BIN" --fail --silent --show-error --max-time 2 "$GANTRY_STREAMING_READYZ" >/dev/null; do
		if [ "$elapsed" -ge "$GANTRY_READY_TIMEOUT_SECONDS" ]; then
			echo "Gantry artifact streaming endpoint did not become ready" >&2
			return 1
		fi
		sleep 1
		elapsed=$((elapsed + 1))
	done
}

read_value() {
	"$JQ_BIN" -r "$1" "$HOST_CONFIG"
}

verify_desired() {
	[ "$(read_value '.p2pConfig.enable // false')" = true ] &&
		[ "$(read_value '.p2pConfig.address // ""')" = "$OVERLAYBD_P2P_ADDRESS" ]
}

apply_config() {
	wait_for_gantry
	[ -f "$HOST_CONFIG" ] || { echo "missing host OverlayBD config: $HOST_CONFIG" >&2; return 1; }
	host_exec test -x "$OVERLAYBD_CONFIG_TOOL"

	if [ -f "$MANAGED_CONFIG" ] && ! cmp -s "$HOST_CONFIG" "$MANAGED_CONFIG"; then
		echo "refusing to overwrite concurrently changed OverlayBD config" >&2
		return 1
	fi

	if [ ! -f "$ORIGINAL_CONFIG" ]; then
		cp "$HOST_CONFIG" "$ORIGINAL_CONFIG"
	fi

	if verify_desired; then
		cp "$HOST_CONFIG" "$MANAGED_CONFIG"
	else
		host_exec_writable "$OVERLAYBD_CONFIG_TOOL" p2pConfig.enable true
		host_exec_writable "$OVERLAYBD_CONFIG_TOOL" p2pConfig.address "\"$OVERLAYBD_P2P_ADDRESS\""
		verify_desired || { echo "OverlayBD config did not converge" >&2; return 1; }
		cp "$HOST_CONFIG" "$MANAGED_CONFIG"
		restart_services
	fi

	touch "$READY_MARKER"
}

restore_config() {
	rm -f "$READY_MARKER"
	[ -f "$ORIGINAL_CONFIG" ] || return 0
	[ -f "$MANAGED_CONFIG" ] || { echo "missing managed OverlayBD config snapshot; preserving host config" >&2; return 0; }

	if ! cmp -s "$HOST_CONFIG" "$MANAGED_CONFIG"; then
		echo "OverlayBD config changed after Gantry configuration; preserving current host config" >&2
		return 0
	fi

	if ! cmp -s "$HOST_CONFIG" "$ORIGINAL_CONFIG"; then
		temporary="$HOST_CONFIG.gantry-restore"
		cp "$ORIGINAL_CONFIG" "$temporary"
		chmod --reference="$HOST_CONFIG" "$temporary"
		mv "$temporary" "$HOST_CONFIG"
		restart_services
	fi

	rm -f "$ORIGINAL_CONFIG" "$MANAGED_CONFIG"
}

case "${1:-apply}" in
apply)
	acquire_lock
	apply_config
	release_lock
	if [ "${GANTRY_OVERLAYBD_ONESHOT:-false}" != true ]; then
		exec tail -f /dev/null
	fi
	;;
restore)
	acquire_lock
	restore_config
	release_lock
	;;
*)
	echo "usage: $0 [apply|restore]" >&2
	exit 2
	;;
esac