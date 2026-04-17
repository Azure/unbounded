// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	sprig "github.com/Masterminds/sprig/v3"
)

type manifestData struct {
	Namespace       string
	ControllerImage string
	NodeImage       string
	ForceNotLeader  string

	// Common
	AzureTenantID string
	ApiserverURL  string
	LogLevel      string

	// Controller
	ControllerHealthPort                       string
	ControllerNodeAgentHealthPort              string
	ControllerInformerResyncPeriod             string
	ControllerStatusStaleThreshold             string
	ControllerStatusWebsocketKeepaliveInterval string
	ControllerStatusWsKeepaliveFailureCount    string
	ControllerRegisterAggregatedAPIServer      string
	ControllerLeaderElectionEnabled            string
	ControllerLeaderElectionLeaseDuration      string
	ControllerLeaderElectionRenewDeadline      string
	ControllerLeaderElectionRetryPeriod        string
	ControllerLeaderElectionResourceName       string
	ControllerKubeProxyHealthInterval          string

	// Node
	NodeCNIConfDir                           string
	NodeCNIConfFile                          string
	NodeBridgeName                           string
	NodeWireGuardDir                         string
	NodeWireGuardPort                        string
	NodeEnablePolicyRouting                  string
	NodeMTU                                  string
	NodeHealthPort                           string
	NodeInformerResyncPeriod                 string
	NodeStatusWebsocketEnabled               string
	NodeStatusWebsocketURL                   string
	NodeStatusWebsocketApiserverMode         string
	NodeStatusWebsocketApiserverURL          string
	NodeStatusWebsocketApiserverStartupDelay string
	NodeStatusWebsocketKeepaliveInterval     string
	NodeStatusWsKeepaliveFailureCount        string
	NodeRemoveConfigurationOnShutdown        string
	NodeShutdownRemoveWireGuardConfiguration string
	NodeShutdownRemoveIPRoutes               string
	NodeShutdownRemoveMasqueradeRules        string
	NodeCriticalDeltaEvery                   string
	NodeStatsDeltaEvery                      string
	NodeStatusPushEnabled                    string
	NodeStatusPushURL                        string
	NodeStatusPushDelta                      string
	NodeStatusPushInterval                   string
	NodeStatusPushApiserverInterval          string
	NodeHealthCheckPort                      string
	NodeBaseMetric                           string
	NodeHealthFlapMaxBackoff                 string
	NodeRouteTableId                         string
	NodeKubeProxyHealthInterval              string
	NodeNetlinkResyncPeriod                  string
}

func main() {
	var (
		templatesDir string
		outputDir    string
		data         manifestData
	)

	flag.StringVar(&templatesDir, "templates-dir", "deploy", "Directory containing *.yaml.tmpl manifest templates")
	flag.StringVar(&outputDir, "output-dir", "", "Directory where rendered manifests are written")
	flag.StringVar(&data.Namespace, "namespace", "", "Kubernetes namespace for manifests")
	flag.StringVar(&data.ControllerImage, "controller-image", "", "Controller image reference")
	flag.StringVar(&data.NodeImage, "node-image", "", "Node image reference")
	flag.StringVar(&data.ForceNotLeader, "force-not-leader", "false", "Value for force-not-leader flag in controller deployment")

	// Common
	flag.StringVar(&data.AzureTenantID, "azure-tenant-id", "", "Azure tenant ID")
	flag.StringVar(&data.ApiserverURL, "apiserver-url", "", "Kubernetes API server URL")
	flag.StringVar(&data.LogLevel, "log-level", "", "Log verbosity level")

	// Controller
	flag.StringVar(&data.ControllerHealthPort, "controller-health-port", "", "Controller health check port")
	flag.StringVar(&data.ControllerNodeAgentHealthPort, "controller-node-agent-health-port", "", "Controller node agent health check port")
	flag.StringVar(&data.ControllerInformerResyncPeriod, "controller-informer-resync-period", "", "Controller informer resync period")
	flag.StringVar(&data.ControllerStatusStaleThreshold, "controller-status-stale-threshold", "", "Controller status stale threshold")
	flag.StringVar(&data.ControllerStatusWebsocketKeepaliveInterval, "controller-status-websocket-keepalive-interval", "", "Controller status websocket keepalive interval")
	flag.StringVar(&data.ControllerStatusWsKeepaliveFailureCount, "controller-status-ws-keepalive-failure-count", "", "Controller status websocket keepalive failure count")
	flag.StringVar(&data.ControllerRegisterAggregatedAPIServer, "controller-register-aggregated-api-server", "", "Register aggregated API server")
	flag.StringVar(&data.ControllerLeaderElectionEnabled, "controller-leader-election-enabled", "", "Enable leader election")
	flag.StringVar(&data.ControllerLeaderElectionLeaseDuration, "controller-leader-election-lease-duration", "", "Leader election lease duration")
	flag.StringVar(&data.ControllerLeaderElectionRenewDeadline, "controller-leader-election-renew-deadline", "", "Leader election renew deadline")
	flag.StringVar(&data.ControllerLeaderElectionRetryPeriod, "controller-leader-election-retry-period", "", "Leader election retry period")
	flag.StringVar(&data.ControllerLeaderElectionResourceName, "controller-leader-election-resource-name", "", "Leader election resource name")
	flag.StringVar(&data.ControllerKubeProxyHealthInterval, "controller-kube-proxy-health-interval", "", "Controller kube-proxy health check interval")

	// Node
	flag.StringVar(&data.NodeCNIConfDir, "node-cni-conf-dir", "", "CNI configuration directory")
	flag.StringVar(&data.NodeCNIConfFile, "node-cni-conf-file", "", "CNI configuration file name")
	flag.StringVar(&data.NodeBridgeName, "node-bridge-name", "", "Bridge device name")
	flag.StringVar(&data.NodeWireGuardDir, "node-wireguard-dir", "", "WireGuard configuration directory")
	flag.StringVar(&data.NodeWireGuardPort, "node-wireguard-port", "", "WireGuard listen port")
	flag.StringVar(&data.NodeEnablePolicyRouting, "node-enable-policy-routing", "", "Enable policy routing")
	flag.StringVar(&data.NodeMTU, "node-mtu", "", "Network MTU (0 for auto)")
	flag.StringVar(&data.NodeHealthPort, "node-health-port", "", "Node agent health check port")
	flag.StringVar(&data.NodeInformerResyncPeriod, "node-informer-resync-period", "", "Node informer resync period")
	flag.StringVar(&data.NodeStatusWebsocketEnabled, "node-status-websocket-enabled", "", "Enable status websocket")
	flag.StringVar(&data.NodeStatusWebsocketURL, "node-status-websocket-url", "", "Status websocket URL")
	flag.StringVar(&data.NodeStatusWebsocketApiserverMode, "node-status-websocket-apiserver-mode", "", "Status websocket API server mode")
	flag.StringVar(&data.NodeStatusWebsocketApiserverURL, "node-status-websocket-apiserver-url", "", "Status websocket API server URL")
	flag.StringVar(&data.NodeStatusWebsocketApiserverStartupDelay, "node-status-websocket-apiserver-startup-delay", "", "Status websocket API server startup delay")
	flag.StringVar(&data.NodeStatusWebsocketKeepaliveInterval, "node-status-websocket-keepalive-interval", "", "Node status websocket keepalive interval")
	flag.StringVar(&data.NodeStatusWsKeepaliveFailureCount, "node-status-ws-keepalive-failure-count", "", "Node status websocket keepalive failure count")
	flag.StringVar(&data.NodeRemoveConfigurationOnShutdown, "node-remove-configuration-on-shutdown", "", "Remove all managed configuration on shutdown")
	flag.StringVar(&data.NodeShutdownRemoveWireGuardConfiguration, "node-shutdown-remove-wireguard-config", "", "Remove WireGuard config on shutdown (deprecated)")
	flag.StringVar(&data.NodeShutdownRemoveIPRoutes, "node-shutdown-remove-ip-routes", "", "Remove IP routes on shutdown (deprecated)")
	flag.StringVar(&data.NodeShutdownRemoveMasqueradeRules, "node-shutdown-remove-masquerade-rules", "", "Remove masquerade rules on shutdown (deprecated)")
	flag.StringVar(&data.NodeCriticalDeltaEvery, "node-critical-delta-every", "", "Critical delta interval")
	flag.StringVar(&data.NodeStatsDeltaEvery, "node-stats-delta-every", "", "Stats delta interval")
	flag.StringVar(&data.NodeStatusPushEnabled, "node-status-push-enabled", "", "Enable status push")
	flag.StringVar(&data.NodeStatusPushURL, "node-status-push-url", "", "Status push URL")
	flag.StringVar(&data.NodeStatusPushDelta, "node-status-push-delta", "", "Enable status push delta")
	flag.StringVar(&data.NodeStatusPushInterval, "node-status-push-interval", "", "Status push interval")
	flag.StringVar(&data.NodeStatusPushApiserverInterval, "node-status-push-apiserver-interval", "", "Status push API server interval")
	flag.StringVar(&data.NodeHealthCheckPort, "node-health-check-port", "", "Node health check UDP port")
	flag.StringVar(&data.NodeBaseMetric, "node-base-metric", "", "Node base metric for route metrics")
	flag.StringVar(&data.NodeHealthFlapMaxBackoff, "node-health-flap-max-backoff", "", "Health check flap max backoff")
	flag.StringVar(&data.NodeRouteTableId, "node-route-table-id", "", "Route table ID for managed routes")
	flag.StringVar(&data.NodeKubeProxyHealthInterval, "node-kube-proxy-health-interval", "", "Interval between kube-proxy health checks (0 to disable)")

	flag.Parse()

	if outputDir == "" {
		exitWithError("--output-dir is required")
	}

	if data.Namespace == "" {
		exitWithError("--namespace is required")
	}

	if err := renderTemplates(templatesDir, outputDir, data); err != nil {
		exitWithError(err.Error())
	}
}

func renderTemplates(templatesDir, outputDir string, data manifestData) error {
	return filepath.WalkDir(templatesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if !strings.HasSuffix(path, ".yaml.tmpl") {
			return nil
		}

		relPath, err := filepath.Rel(templatesDir, path)
		if err != nil {
			return err
		}

		outputRelPath := strings.TrimSuffix(relPath, ".tmpl")
		outputPath := filepath.Join(outputDir, outputRelPath)

		templateBytes, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read template %q: %w", path, err)
		}

		tmpl, err := template.New(relPath).Funcs(sprig.TxtFuncMap()).Option("missingkey=error").Parse(string(templateBytes))
		if err != nil {
			return fmt.Errorf("parse template %q: %w", path, err)
		}

		if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
			return fmt.Errorf("create output dir for %q: %w", outputPath, err)
		}

		var rendered bytes.Buffer
		if err := tmpl.Execute(&rendered, data); err != nil {
			return fmt.Errorf("execute template %q: %w", path, err)
		}

		if err := os.WriteFile(outputPath, rendered.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write rendered manifest %q: %w", outputPath, err)
		}

		return nil
	})
}

func exitWithError(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
