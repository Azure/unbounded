// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkAPIServerReachableName = "api-server-reachable"

type apiServerReachableChecker struct {
	log        *slog.Logger
	config     config.AgentConfig
	httpClient *http.Client
}

// Preflight returns the standard node-start checks that can run before the
// nspawn machine starts.
func Preflight(log *slog.Logger, cfg config.AgentConfig, goalState *goalstates.MachineGoalState) []preflight.Checker {
	return []preflight.Checker{
		// TODO: Consider moving the kubelet bind address to the kubelet goal state.
		CheckBindAddress(log, checkKubeletBindAddressName, kubeletBindAddress, "kubelet bind address"),
		CheckBindAddress(
			log,
			checkContainerdMetricsBindAddressName,
			goalState.NodeStart.Containerd.MetricsAddress,
			"containerd metrics bind address",
		),
		CheckAPIServerReachable(log, cfg),
	}
}

// CheckAPIServerReachable returns a non-mutating checker that validates the
// cluster CA data and configured Kubernetes API server reachability. The
// checker redacts the configured endpoint from result messages.
func CheckAPIServerReachable(log *slog.Logger, cfg config.AgentConfig) preflight.Checker {
	return apiServerReachableChecker{log: log, config: cfg}
}

func (c apiServerReachableChecker) Name() string { return checkAPIServerReachableName }

func (c apiServerReachableChecker) Check(ctx context.Context) []preflight.Result {
	if errs := c.validateClusterCredentials(); len(errs) > 0 {
		return preflight.ResultsError(checkAPIServerReachableName, "cluster credentials", "%s", strings.Join(errs, "; "))
	}

	apiServer := c.config.Kubelet.ApiServer
	if strings.TrimSpace(apiServer) == "" {
		return preflight.ResultsError(checkAPIServerReachableName, "cluster API server", "API server is required")
	}

	parsed, err := url.Parse(apiServer)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return preflight.ResultsError(checkAPIServerReachableName, "cluster API server", "API server endpoint is invalid")
	}

	client := c.httpClient
	if client == nil {
		client = c.httpClientWithCA()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiServer, "/")+"/readyz", http.NoBody)
	if err != nil {
		return preflight.ResultsError(checkAPIServerReachableName, "cluster API server", "API server request could not be created")
	}

	resp, err := client.Do(req)
	if err != nil {
		return preflight.ResultsError(checkAPIServerReachableName, "cluster API server", "API server is not reachable")
	}
	defer resp.Body.Close() //nolint:errcheck // best effort close

	if resp.StatusCode >= http.StatusInternalServerError {
		return preflight.ResultsError(checkAPIServerReachableName, "cluster API server", "API server returned status %d", resp.StatusCode)
	}

	return preflight.ResultsOK(checkAPIServerReachableName, "cluster API server", "API server is reachable")
}

func (c apiServerReachableChecker) validateClusterCredentials() []string {
	var errs []string
	if _, err := base64.StdEncoding.DecodeString(c.config.Cluster.CaCertBase64); err != nil {
		errs = append(errs, "cluster CA data is invalid")
	}

	return errs
}

func (c apiServerReachableChecker) httpClientWithCA() *http.Client {
	transport := &http.Transport{}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	}

	caCertData, err := base64.StdEncoding.DecodeString(c.config.Cluster.CaCertBase64)
	if err == nil && len(caCertData) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}

		pool.AppendCertsFromPEM(caCertData)
		transport.TLSClientConfig = &tls.Config{RootCAs: pool} //nolint:gosec // uses configured root CAs.
	}

	return &http.Client{Timeout: 10 * time.Second, Transport: transport}
}
