#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -eu

: "${HOST_ROOT:=/host}"
: "${CONFIG_TOOL:=/usr/local/bin/gantry-node-config}"
: "${GANTRY_STREAMING_READYZ:=http://localhost:5000/artifact-streaming/readyz}"
: "${GANTRY_SNAPSHOTTER_ADDRESS:=/run/gantry-overlaybd/gantry-overlaybd.sock}"
: "${GANTRY_READY_TIMEOUT_SECONDS:=120}"
: "${GANTRY_NBD_MAX_DEVICES:=1024}"
: "${GANTRY_NBD_MIN_DEVICES:=64}"
: "${NSENTER_BIN:=nsenter}"
: "${CURL_BIN:=curl}"
: "${READY_MARKER:=/run/gantry-overlaybd/configured}"
: "${HOST_STATE_DIR:=/var/lib/gantry/snapshotter-config}"

STATE_DIR="$HOST_ROOT$HOST_STATE_DIR"
HOST_SOCKET="$HOST_ROOT$GANTRY_SNAPSHOTTER_ADDRESS"
PENDING_ACTIVATION="$STATE_DIR/pending-activation"

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

wait_for_services() {
	elapsed=0
	while ! "$CURL_BIN" --fail --silent --show-error --max-time 2 "$GANTRY_STREAMING_READYZ" >/dev/null || [ ! -S "$HOST_SOCKET" ]; do
		if [ "$elapsed" -ge "$GANTRY_READY_TIMEOUT_SECONDS" ]; then
			echo "Gantry streaming endpoint or snapshotter socket did not become ready" >&2
			return 1
		fi
		sleep 1
		elapsed=$((elapsed + 1))
	done
}

prepare_nbd() {
	host_exec modprobe nbd "nbds_max=$GANTRY_NBD_MAX_DEVICES"
	count=$(host_exec sh -c 'set -- /dev/nbd[0-9]*; [ -e "$1" ] || { echo 0; exit; }; echo "$#"')
	if [ "$count" -lt "$GANTRY_NBD_MIN_DEVICES" ]; then
		echo "only $count NBD devices are available; need at least $GANTRY_NBD_MIN_DEVICES" >&2
		return 1
	fi
}

configure() {
	"$CONFIG_TOOL" \
		--action "$1" \
		--config "$HOST_ROOT/etc/containerd/config.toml" \
		--drop-in "$HOST_ROOT/etc/containerd/conf.d/99-gantry-overlaybd.toml" \
		--state "$STATE_DIR" \
		--address "$GANTRY_SNAPSHOTTER_ADDRESS"
}

restart_containerd() {
	host_exec systemctl restart containerd
	host_exec systemctl is-active --quiet containerd
}

activate_config() {
	pending=false
	[ ! -f "$PENDING_ACTIVATION" ] || pending=true
	# Persist intent before the tool can change files or remove restore state.
	touch "$PENDING_ACTIVATION"
	result=$(configure "$1")
	case "$result" in
	changed|unchanged) ;;
	*) echo "unexpected configuration result: $result" >&2; return 1 ;;
	esac
	if [ "$result" = changed ] || [ "$pending" = true ]; then
		restart_containerd
	else
		host_exec systemctl is-active --quiet containerd
	fi
	rm -f "$PENDING_ACTIVATION"
}

apply_config() {
	rm -f "$READY_MARKER"
	prepare_nbd
	wait_for_services
	activate_config apply
	touch "$READY_MARKER"
}

restore_config() {
	rm -f "$READY_MARKER"
	activate_config restore
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