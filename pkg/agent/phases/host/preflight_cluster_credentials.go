// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	"encoding/base64"
	"log/slog"
	"strings"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkClusterCredentialsName = "cluster-credentials"

type clusterCredentialsChecker struct {
	log    *slog.Logger
	config *provision.UnboundedAgentConfig
}

// CheckClusterCredentials returns a checker that validates cluster CA data and
// the bootstrap credential. When attestation is configured, missing kubelet auth
// is allowed because attestation can provide the credential later.
func CheckClusterCredentials(log *slog.Logger, cfg *provision.UnboundedAgentConfig) preflight.Checker {
	return clusterCredentialsChecker{log: log, config: cfg}
}

// Name returns the stable check name used in reports and ignore rules.
func (c clusterCredentialsChecker) Name() string { return checkClusterCredentialsName }

// Check validates cluster credential inputs without printing credential values.
func (c clusterCredentialsChecker) Check(context.Context) []preflight.Result {
	if c.config == nil {
		return preflight.ResultsError(checkClusterCredentialsName, "cluster credentials", "agent config is missing")
	}

	var errs []string
	if _, err := base64.StdEncoding.DecodeString(c.config.Cluster.CaCertBase64); err != nil {
		errs = append(errs, "cluster CA data is invalid")
	}

	if c.config.Attest == nil {
		auth := c.config.Kubelet.Auth
		if err := auth.Validate(); err != nil {
			errs = append(errs, "bootstrap credential is invalid")
		}
	}

	if len(errs) > 0 {
		return preflight.ResultsError(checkClusterCredentialsName, "cluster credentials", "%s", strings.Join(errs, "; "))
	}

	return preflight.ResultsOK(checkClusterCredentialsName, "cluster credentials", "cluster credentials are valid")
}
