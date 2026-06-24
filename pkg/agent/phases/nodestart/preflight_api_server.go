// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// CheckAPIServerReachableName is the stable name for API server reachability.
const CheckAPIServerReachableName = "api-server-reachable"

type apiServerReachableChecker struct {
	url        string
	httpClient *http.Client
}

// CheckAPIServerReachable returns a non-mutating checker that validates the
// configured Kubernetes API server can be reached from the host. The checker
// redacts the configured endpoint from result messages.
func CheckAPIServerReachable(apiServer string) preflight.Checker {
	return apiServerReachableChecker{url: apiServer}
}

func (c apiServerReachableChecker) Name() string { return CheckAPIServerReachableName }

func (c apiServerReachableChecker) Check(ctx context.Context) []preflight.Result {
	if strings.TrimSpace(c.url) == "" {
		return preflight.ResultsError(CheckAPIServerReachableName, "cluster API server", "API server is required")
	}

	parsed, err := url.Parse(c.url)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return preflight.ResultsError(CheckAPIServerReachableName, "cluster API server", "API server endpoint is invalid")
	}

	client := c.httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.url, "/")+"/readyz", http.NoBody)
	if err != nil {
		return preflight.ResultsError(CheckAPIServerReachableName, "cluster API server", "API server request could not be created")
	}

	resp, err := client.Do(req)
	if err != nil {
		return preflight.ResultsError(CheckAPIServerReachableName, "cluster API server", "API server is not reachable")
	}
	defer resp.Body.Close() //nolint:errcheck // best effort close

	if resp.StatusCode >= http.StatusInternalServerError {
		return preflight.ResultsError(CheckAPIServerReachableName, "cluster API server", fmt.Sprintf("API server returned status %d", resp.StatusCode))
	}

	return preflight.ResultsOK(CheckAPIServerReachableName, "cluster API server", "API server is reachable")
}
