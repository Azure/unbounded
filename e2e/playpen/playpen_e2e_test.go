//go:build e2e

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	playpenclient "github.com/Azure/unbounded/internal/playpen/client"
	"github.com/Azure/unbounded/internal/playpen/operator"
)

func TestAllocateKubeVirtPlaypenVM(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in short mode")
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		if clientcmd.IsEmptyConfig(err) {
			t.Skipf("skipping playpen e2e: no Kubernetes configuration was provided: %v", err)
		}

		t.Fatal(err)
	}

	client, err := playpenclient.New(playpenclient.Config{RESTConfig: restConfig})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	allocation, err := client.AllocateVM(ctx, playpenclient.AllocateVMOptions{Architecture: operator.ArchitectureAMD64})
	if err != nil {
		t.Fatal(err)
	}
	defer allocation.Close(context.Background()) //nolint:errcheck

	if allocation.Metadata.ID == "" {
		t.Fatal("allocation id is empty")
	}

	if allocation.Metadata.Network.GuestMAC == "" || allocation.Metadata.Network.GuestIPv4 == "" {
		t.Fatalf("incomplete network metadata: %#v", allocation.Metadata.Network)
	}

	if allocation.Metadata.Redfish["url"] == "" || allocation.Metadata.Redfish["password"] == "" {
		t.Fatalf("incomplete redfish metadata: %#v", allocation.Metadata.Redfish)
	}

	if allocation.Metadata.Endpoint.WireGuardUDPPort == 0 {
		t.Fatalf("missing wireguard endpoint: %#v", allocation.Metadata.Endpoint)
	}
}
