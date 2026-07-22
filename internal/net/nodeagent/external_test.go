// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeagent

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestExternalGatewayConfigUsesSafeStandaloneDefaults(t *testing.T) {
	restConfig := &rest.Config{Host: "https://api.example.test"}
	cfg, err := externalGatewayConfig(ExternalGatewayOptions{
		NodeName:   "bootstrap-gateway",
		RuntimeDir: "/run/unbounded-netboot/bootstrap-gateway",
		RESTConfig: restConfig,
	})
	require.NoError(t, err)
	require.Equal(t, "bootstrap-gateway", cfg.NodeName)
	require.Equal(t, "/run/unbounded-netboot/bootstrap-gateway/wireguard", cfg.WireGuardDir)
	require.Equal(t, "/run/unbounded-netboot/bootstrap-gateway/cni", cfg.CNIConfDir)
	require.Same(t, restConfig, cfg.RESTConfig)
	require.False(t, cfg.StatusPushEnabled)
	require.False(t, cfg.StatusWSEnabled)
	require.Zero(t, cfg.KubeProxyHealthInterval)
	require.Zero(t, cfg.HealthPort)
	require.Equal(t, 254, cfg.RouteTableID)
	require.True(t, cfg.RemoveConfigurationOnShutdown)
	require.Equal(t, "WireGuard", cfg.PreferredPublicEncap)
}

func TestExternalGatewayConfigRequiresIdentityAndRuntimeDirectory(t *testing.T) {
	_, err := externalGatewayConfig(ExternalGatewayOptions{RuntimeDir: "/run/unbounded-netboot/gateway"})
	require.ErrorContains(t, err, "node name")

	_, err = externalGatewayConfig(ExternalGatewayOptions{NodeName: "gateway"})
	require.ErrorContains(t, err, "runtime directory")

	_, err = externalGatewayConfig(ExternalGatewayOptions{
		NodeName:   "gateway",
		RuntimeDir: "relative/path",
	})
	require.ErrorContains(t, err, "absolute")
}
