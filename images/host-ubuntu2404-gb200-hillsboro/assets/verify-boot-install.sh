#!/bin/sh
set -eu

test "$(uname -m)" = aarch64
test "$(getconf PAGESIZE)" = 65536
test "$(uname -r)" = "$(python3 -c 'import json; print(json.load(open("/usr/share/unbounded/image-inputs.json"))["kernel"])')"
systemctl is-active --quiet systemd-networkd.service
systemctl is-active --quiet ssh.service
test -x /usr/bin/ssh-import-id
test -s /etc/unbounded/agent/config.json
test -f /etc/apt/apt.conf.d/99-unbounded-no-restart-systemd
test -f /etc/needrestart/conf.d/99-unbounded.conf
modprobe --dry-run mana
modprobe --dry-run nvidia

users="bcho cchildress chokevin imiller31 jveski jwilder phealy plombardi89 qike-ms vpatelsj"
for user in $users; do
    getent passwd "$user" | awk -F: '$7 == "/bin/bash" { found = 1 } END { exit !found }'
    passwd -S "$user" | awk '$2 == "L" { found = 1 } END { exit !found }'
done

test ! -s /home/bcho/.ssh/authorized_keys
for user in cchildress chokevin imiller31 jveski jwilder phealy plombardi89 qike-ms vpatelsj; do
    keys="/home/$user/.ssh/authorized_keys"
    test -s "$keys"
    grep -Fq "gh:$user" "$keys"
    ssh-keygen -l -f "$keys" >/dev/null
done

echo UNBOUNDED_GB200_UBUNTU_BOOT_PASS > /dev/ttyAMA0
systemctl poweroff --no-block
