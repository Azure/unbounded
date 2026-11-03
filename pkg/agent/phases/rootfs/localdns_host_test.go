// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestLocalDNSSupervisorScriptRewritesCoreDNSPath(t *testing.T) {
	t.Parallel()

	got := string(LocalDNSSupervisorScript("/opt/unbounded/node/current/bin/coredns"))
	require.Contains(t, got, "/opt/unbounded/node/current/bin/coredns -conf")
	require.NotContains(t, got, goalstates.LocalDNSCoreDNSBinaryPath)
	require.Equal(t, string(localDNSSupervisor), string(LocalDNSSupervisorScript(goalstates.LocalDNSCoreDNSBinaryPath)), "the default path is the nspawn script")
}

func TestInstallCoreDNSIsNoOpWhenDisabled(t *testing.T) {
	t.Parallel()

	gs := &goalstates.RootFS{}
	require.NoError(t, InstallCoreDNS(slog.New(slog.DiscardHandler), gs).Do(context.Background()))
	require.Equal(t, "install-coredns", InstallCoreDNS(slog.Default(), gs).Name())
}
