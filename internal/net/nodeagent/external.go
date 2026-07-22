// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/rest"
)

// ExternalGatewayOptions identifies a temporary external gateway dataplane.
type ExternalGatewayOptions struct {
	NodeName   string
	RuntimeDir string
	RESTConfig *rest.Config
}

func externalGatewayConfig(options ExternalGatewayOptions) (*config, error) {
	if options.NodeName == "" {
		return nil, fmt.Errorf("external gateway node name is required")
	}
	if options.RuntimeDir == "" {
		return nil, fmt.Errorf("external gateway runtime directory is required")
	}
	if !filepath.IsAbs(options.RuntimeDir) {
		return nil, fmt.Errorf("external gateway runtime directory must be absolute")
	}

	cfg := defaultConfig()
	cfg.NodeName = options.NodeName
	cfg.CNIConfDir = filepath.Join(options.RuntimeDir, "cni")
	cfg.WireGuardDir = filepath.Join(options.RuntimeDir, "wireguard")
	cfg.StatusPushEnabled = false
	cfg.StatusWSEnabled = false
	cfg.KubeProxyHealthInterval = 0
	cfg.HealthPort = 0
	cfg.RouteTableID = 254
	cfg.RemoveConfigurationOnShutdown = true
	cfg.PreferredPublicEncap = "WireGuard"
	cfg.RESTConfig = options.RESTConfig

	return cfg, nil
}

// RunExternalGateway runs the normal node dataplane with external-gateway-safe
// defaults until ctx is cancelled.
func RunExternalGateway(ctx context.Context, options ExternalGatewayOptions) error {
	cfg, err := externalGatewayConfig(options)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.WireGuardDir, 0o700); err != nil {
		return fmt.Errorf("create WireGuard runtime directory: %w", err)
	}
	if err := os.MkdirAll(cfg.CNIConfDir, 0o700); err != nil {
		return fmt.Errorf("create CNI runtime directory: %w", err)
	}

	return run(ctx, cfg)
}
