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
	publicNetworkAccess       string
	gantryPublicNetworkAccess string
}

func (r azurePreflightRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	command := name + " " + strings.Join(args, " ")

	switch {
	case strings.Contains(command, "account show"):
		return []byte(`{}`), nil
	case strings.Contains(command, "acr show"):
		if strings.Contains(command, "--ids") {
			return nil, fmt.Errorf("unexpected ACR lookup: %s", command)
		}

		registryName := "acr"
		publicNetworkAccess := r.publicNetworkAccess

		for _, candidate := range []string{"baseline", "gantry", "acr"} {
			if strings.Contains(command, "--name "+candidate) {
				registryName = candidate
				break
			}
		}

		if registryName == "gantry" && r.gantryPublicNetworkAccess != "" {
			publicNetworkAccess = r.gantryPublicNetworkAccess
		}

		return []byte(fmt.Sprintf(
			`{"id":"/subscriptions/s/registries/%s","loginServer":"%s.azurecr.io","publicNetworkAccess":%q}`,
			registryName,
			registryName,
			publicNetworkAccess,
		)), nil
	case strings.Contains(command, "private-endpoint show"):
		registryName := "acr"
		if strings.Contains(command, "privateEndpoints/baseline") {
			registryName = "baseline"
		} else if strings.Contains(command, "privateEndpoints/gantry") {
			registryName = "gantry"
		}

		return []byte(fmt.Sprintf(`{
			"provisioningState":"Succeeded",
			"privateLinkServiceConnections":[{
				"privateLinkServiceId":"/subscriptions/s/registries/%s",
				"privateLinkServiceConnectionState":{"status":"Approved"}
			}]
		}`, registryName)), nil
	case strings.Contains(command, "metrics list-definitions"):
		return []byte(`[{"name":{"value":"PEBytesIn"}}]`), nil
	case strings.Contains(command, "az rest"):
		return []byte(`{"tables":[{"name":"PrimaryResult","columns":[],"rows":[]}]}`), nil
	default:
		return nil, fmt.Errorf("unexpected command: %s", command)
	}
}

func dualAzurePreflightBenchmark(gantryPublicNetworkAccess string) *benchmark {
	return &benchmark{
		config: benchmarkConfig{
			Mode:                      benchmarkModeDirect,
			Namespace:                 "gantry-benchmark",
			BaselineACRLoginServer:    "baseline.azurecr.io",
			BaselineACRResourceID:     "/subscriptions/s/registries/baseline",
			BaselinePrivateEndpointID: "/subscriptions/s/privateEndpoints/baseline",
			GantryACRLoginServer:      "gantry.azurecr.io",
			GantryACRResourceID:       "/subscriptions/s/registries/gantry",
			GantryPrivateEndpointID:   "/subscriptions/s/privateEndpoints/gantry",
			LogAnalyticsWorkspaceID:   "workspace-id",
			AKSResourceID:             "/subscriptions/s/managedClusters/aks",
			TelemetryPollInterval:     15,
			TelemetryTimeout:          60,
			StateRoot:                 "unused",
		},
		commands: azurePreflightRunner{
			publicNetworkAccess:       "Disabled",
			gantryPublicNetworkAccess: gantryPublicNetworkAccess,
		},
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

func TestCheckAzureTelemetryValidatesBothDirectRegistries(t *testing.T) {
	if err := dualAzurePreflightBenchmark("Disabled").checkAzureTelemetry(context.Background()); err != nil {
		t.Fatalf("checkAzureTelemetry: %v", err)
	}
}

func TestCheckAzureTelemetryRejectsPublicGantryACR(t *testing.T) {
	err := dualAzurePreflightBenchmark("Enabled").checkAzureTelemetry(context.Background())
	if err == nil || !strings.Contains(err.Error(), "gantry_cold ACR telemetry") || !strings.Contains(err.Error(), "public network access") {
		t.Fatalf("error = %v, want public Gantry ACR rejection", err)
	}
}
