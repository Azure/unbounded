#!/usr/bin/env bash
# Runs in the host namespaces on one worker (see bench.sh diskprep). Frees the
# Azure resource disk for racer and prints "DISK <node> <by-id name> <dev>",
# or "DISK <node> none <reason>" when the node has no usable disk.
set -u
node=$(hostname | tr "[:upper:]" "[:lower:]")
rootdisk=$(lsblk -no pkname "$(findmnt -n -o SOURCE /)" | head -1)
disk=""
src=$(findmnt -n -o SOURCE /mnt 2>/dev/null || true)
if [ -n "$src" ]; then
	disk=/dev/$(lsblk -no pkname "$src" | head -1)
	if [ "$disk" = "/dev/$rootdisk" ] || [ "$disk" = /dev/ ]; then
		echo "DISK $node none mnt-on-root-disk"
		exec sleep infinity
	fi
	if ! umount /mnt; then
		echo "DISK $node none umount-failed"
		exec sleep infinity
	fi
	# Keep the disk free across reboots.
	sed -i 's|^\([^#].*[[:space:]]/mnt[[:space:]]\)|#\1|' /etc/fstab
	wipefs -a "$disk" >/dev/null
	mb=$(($(blockdev --getsize64 "$disk") / 1048576))
	dd if=/dev/zero of="$disk" bs=1M count=2 oflag=direct status=none
	dd if=/dev/zero of="$disk" bs=1M seek=$((mb - 2)) count=2 oflag=direct status=none
	blockdev --rereadpt "$disk" 2>/dev/null || true
	udevadm settle 2>/dev/null || true
fi
if [ -z "$disk" ]; then
	# Already freed on an earlier pass (do not wipe again; racer may own it):
	# report the unmounted sd disk that is not the root disk.
	for d in /sys/block/sd*; do
		[ -e "$d" ] || continue
		b=$(basename "$d")
		[ "$b" = "$rootdisk" ] && continue
		findmnt -n -S "/dev/$b" >/dev/null && continue
		disk=/dev/$b
	done
fi
if [ -z "$disk" ]; then
	echo "DISK $node none no-disk"
	exec sleep infinity
fi
id=""
for l in /dev/disk/by-id/wwn-* /dev/disk/by-id/scsi-3* /dev/disk/by-id/*; do
	[ -e "$l" ] || continue
	if [ "$(readlink -f "$l")" = "$disk" ]; then
		id=$(basename "$l")
		break
	fi
done
echo "DISK $node ${id:-none} $disk"
exec sleep infinity
