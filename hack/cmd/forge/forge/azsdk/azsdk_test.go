// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azsdk

import (
	"bytes"
	"log"
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

func Test_debugLog(t *testing.T) {
	// debugLog writes through the standard logger, so capture its output.
	var buf bytes.Buffer

	origWriter := log.Writer()
	log.SetOutput(&buf)

	t.Cleanup(func() { log.SetOutput(origWriter) })

	t.Run("substitutes_variadic_args", func(t *testing.T) {
		buf.Reset()

		t.Setenv(envDebugLog, "1")
		debugLog("source=%q count=%d", "ManagedIdentity", 2)

		out := buf.String()
		require.Contains(t, out, `source="ManagedIdentity" count=2`)
		require.NotContains(t, out, "%!")
		require.NotContains(t, out, "[]interface {}")
	})

	t.Run("no_args_leaves_message_unchanged", func(t *testing.T) {
		buf.Reset()

		t.Setenv(envDebugLog, "1")
		debugLog("plain message")

		out := buf.String()
		require.Contains(t, out, "plain message")
		require.NotContains(t, out, "%!")
	})

	t.Run("disabled_does_not_log", func(t *testing.T) {
		buf.Reset()

		t.Setenv(envDebugLog, "0")
		debugLog("should not appear %s", "anywhere")

		require.Empty(t, buf.String())
	})
}

func Test_isManagedIdentityResourceID(t *testing.T) {
	require.False(t, isManagedIdentityResourceID("notAResourceID"))
	require.True(t, isManagedIdentityResourceID("/subscriptions/foo/resourceGroups/bar/providers/Microsoft.ManagedIdentity/userAssignedIdentities/baz"))
	require.True(t, isManagedIdentityResourceID(strings.ToLower("/subscriptions/foo/resourceGroups/bar/providers/Microsoft.ManagedIdentity/userAssignedIdentities/baz")))
}
