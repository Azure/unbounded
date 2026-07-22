// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeagent

import (
	"fmt"
	"path/filepath"
	"time"

	"k8s.io/client-go/rest"
)

// ExternalGatewayOptions identifies a temporary external gateway dataplane.
type ExternalGatewayOptions struct {
	NodeName   string
	RuntimeDir string
	RESTConfig *rest.Config
}

type config struct {
	NodeName                      string
	CNIConfDir                    string
	WireGuardDir                  string
	StatusPushEnabled             bool
	StatusWSEnabled               bool
	KubeProxyHealthInterval       time.Duration
	HealthPort                    int
	RouteTableID                  int
	RemoveConfigurationOnShutdown bool
	PreferredPublicEncap          string
	RESTConfig                    *rest.Config
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

	return &config{
		NodeName:                      options.NodeName,
		CNIConfDir:                    filepath.Join(options.RuntimeDir, "cni"),
		WireGuardDir:                  filepath.Join(options.RuntimeDir, "wireguard"),
		StatusPushEnabled:             false,
		StatusWSEnabled:               false,
		KubeProxyHealthInterval:       0,
		HealthPort:                    0,
		RouteTableID:                  254,
		RemoveConfigurationOnShutdown: true,
		PreferredPublicEncap:          "WireGuard",
		RESTConfig:                    options.RESTConfig,
	}, nil
}
