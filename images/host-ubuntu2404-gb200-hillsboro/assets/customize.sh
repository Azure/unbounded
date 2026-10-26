#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -eu

dpkg --install \
    /tmp/packages/wget_1.21.4-1ubuntu4.5_arm64.deb \
    /tmp/packages/ssh-import-id_5.11-0ubuntu2.24.04.1_all.deb
test "$(dpkg-query -W -f='${Version}' ssh-import-id)" = 5.11-0ubuntu2.24.04.1
cloud-init schema --config-file /etc/cloud/cloud.cfg.d/90-hillsboro-users.cfg

rm -rf /tmp/packages /var/lib/cloud/*
rm -f /etc/ssh/ssh_host_*
: > /etc/machine-id
ln -sf /etc/machine-id /var/lib/dbus/machine-id
