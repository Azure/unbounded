// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azsdk

import (
	"strings"
	"testing"

	azcorecloud "github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"

	"github.com/stretchr/testify/require"
)

func TestCloudConfig(t *testing.T) {
	publicCloud, err := CloudConfig("AzurePublicCloud")
	require.NoError(t, err)
	require.Equal(t, "https://management.azure.com", publicCloud.Services[azcorecloud.ResourceManager].Endpoint)
}

func TestChainFromEnv(t *testing.T) {
	t.Run("chain_config_empty_or_not_set_returns_default_chain", func(t *testing.T) {
		t.Setenv("AZURE_AUTH_CHAIN_ORDER", "")
		require.Equal(t, []CredSource{
			&AzureDevOpsPipelineCredential{},
			&ManagedIdentityCredential{},
			&CLICredential{},
		}, ChainFromEnv())
	})

	t.Run("chain_config_set_returns_configured_chain", func(t *testing.T) {
		t.Setenv("AZURE_AUTH_CHAIN_ORDER", "ManagedIdentity,CLI")
		require.Equal(t, []CredSource{&ManagedIdentityCredential{}, &CLICredential{}}, ChainFromEnv())
	})
}

func Test_isManagedIdentityResourceID(t *testing.T) {
	require.False(t, isManagedIdentityResourceID("notAResourceID"))
	require.True(t, isManagedIdentityResourceID("/subscriptions/foo/resourceGroups/bar/providers/Microsoft.ManagedIdentity/userAssignedIdentities/baz"))
	require.True(t, isManagedIdentityResourceID(strings.ToLower("/subscriptions/foo/resourceGroups/bar/providers/Microsoft.ManagedIdentity/userAssignedIdentities/baz")))
}
