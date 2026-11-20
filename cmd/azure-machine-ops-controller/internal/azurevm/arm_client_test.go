// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/require"
)

type staticCredential struct{}

func (staticCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "test", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// recordingTransport answers every request as an accepted, finished write
// and keeps the requests.
type recordingTransport struct {
	requests []*http.Request
}

func (t *recordingTransport) Do(request *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, request)

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"properties":{"provisioningState":"Succeeded"}}`)),
		Request:    request,
	}, nil
}

// TestARMClientSendsConditionalWrites checks the two headers HostReplace
// relies on to never overwrite a VM it did not expect. Every other test
// uses a fake that enforces the conditions itself.
func TestARMClientSendsConditionalWrites(t *testing.T) {
	t.Parallel()

	transport := &recordingTransport{}

	vms, err := armcompute.NewVirtualMachinesClient(hostSubscription, staticCredential{}, &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: transport},
	})
	require.NoError(t, err)

	client := armAzureVMClient{armCapacityClient: &armCapacityClient{vms: vms}}

	require.NoError(t, client.CreateVM(t.Context(), hostResourceGroup, hostVMName, armcompute.VirtualMachine{Location: new("eastus2")}))
	require.NoError(t, client.UpdateVM(t.Context(), hostResourceGroup, hostVMName, armcompute.VirtualMachine{Location: new("eastus2")}, `W/"7"`))
	require.Len(t, transport.requests, 2)

	create, update := transport.requests[0], transport.requests[1]

	require.Equal(t, http.MethodPut, create.Method)
	require.Equal(t, "*", create.Header.Get("If-None-Match"))
	require.Empty(t, create.Header.Get("If-Match"))

	require.Equal(t, http.MethodPut, update.Method)
	require.Equal(t, `W/"7"`, update.Header.Get("If-Match"))
	require.Empty(t, update.Header.Get("If-None-Match"))

	// An update without an etag would be unconditional, so it is not sent.
	require.Error(t, client.UpdateVM(t.Context(), hostResourceGroup, hostVMName, armcompute.VirtualMachine{}, ""))
	require.Len(t, transport.requests, 2)
}
