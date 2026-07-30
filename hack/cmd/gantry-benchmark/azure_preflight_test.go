// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type azurePreflightRunner struct {
	publicNetworkAccess string
}

func (r azurePreflightRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	command := name + " " + strings.Join(args, " ")

	switch {
	case strings.Contains(command, "account show"):
		return []byte(`{}`), nil
	case strings.Contains(command, "acr show"):
		if !strings.Contains(command, "--name acr") || strings.Contains(command, "--ids") {
			return nil, fmt.Errorf("unexpected ACR lookup: %s", command)
		}

		return []byte(fmt.Sprintf(
			`{"id":"/subscriptions/s/registries/acr","loginServer":"acr.azurecr.io","publicNetworkAccess":%q}`,
			r.publicNetworkAccess,
		)), nil
	case strings.Contains(command, "private-endpoint show"):
		return []byte(`{
			"provisioningState":"Succeeded",
			"privateLinkServiceConnections":[{
				"privateLinkServiceId":"/subscriptions/s/registries/acr",
				"privateLinkServiceConnectionState":{"status":"Approved"}
			}]
		}`), nil
	case strings.Contains(command, "metrics list-definitions"):
		return []byte(`[{"name":{"value":"PEBytesIn"}}]`), nil
	case strings.Contains(command, "az rest"):
		return []byte(`{"tables":[{"name":"PrimaryResult","columns":[],"rows":[]}]}`), nil
	default:
		return nil, fmt.Errorf("unexpected command: %s", command)
	}
}

func azurePreflightBenchmark(publicNetworkAccess string) *benchmark {
	return &benchmark{
		config: benchmarkConfig{
			Namespace:                    "gantry-benchmark",
			ACRLoginServer:               "acr.azurecr.io",
			LogAnalyticsWorkspaceID:      "workspace-id",
			ACRResourceID:                "/subscriptions/s/registries/acr",
			AKSResourceID:                "/subscriptions/s/managedClusters/aks",
			ACRPrivateEndpointResourceID: "/subscriptions/s/privateEndpoints/acr",
			TelemetryPollInterval:        15,
			TelemetryTimeout:             60,
			StateRoot:                    "unused",
		},
		commands: azurePreflightRunner{publicNetworkAccess: publicNetworkAccess},
	}
}

func TestCheckAzureTelemetry(t *testing.T) {
	if err := azurePreflightBenchmark("Disabled").checkAzureTelemetry(context.Background()); err != nil {
		t.Fatalf("checkAzureTelemetry: %v", err)
	}
}

func TestCheckAzureTelemetryRejectsPublicACR(t *testing.T) {
	err := azurePreflightBenchmark("Enabled").checkAzureTelemetry(context.Background())
	if err == nil || !strings.Contains(err.Error(), "public network access") {
		t.Fatalf("error = %v, want public network access rejection", err)
	}
}
