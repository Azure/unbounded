// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azsdk

import (
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	envSystemAccessToken             = "SYSTEM_ACCESSTOKEN"
	envAzureCLITaskV2Prefix          = "AZURESUBSCRIPTION_"
	envAzureCLIv2TenantID            = envAzureCLITaskV2Prefix + "TENANT_ID"
	envAzureCLIv2ClientID            = envAzureCLITaskV2Prefix + "CLIENT_ID"
	envAzureCLIv2ServiceConnectionID = envAzureCLITaskV2Prefix + "SERVICE_CONNECTION_ID"
)

type AzureDevOpsPipelineCredential struct {
	ClientID            string
	ServiceConnectionID string
	SystemAccessToken   string
}

func (c *AzureDevOpsPipelineCredential) Configure(ao AuthConfig) (azcore.TokenCredential, error) {
	// support the standard AZURE_* environment variables but also the AzureCLI@2 task
	// specific ones to minimize manual remapping.
	if ao.TenantID == "" {
		ao.TenantID = getEnvValueCheckingMultipleVars(EnvAzureTenantID, envAzureCLIv2TenantID)
	}

	if c.ClientID == "" {
		c.ClientID = getEnvValueCheckingMultipleVars(EnvAzureClientID, envAzureCLIv2ClientID)
	}

	if c.SystemAccessToken == "" {
		c.SystemAccessToken = os.Getenv(envSystemAccessToken)
	}

	if c.ServiceConnectionID == "" {
		c.ServiceConnectionID = os.Getenv(envAzureCLIv2ServiceConnectionID)
	}

	if c.ServiceConnectionID != "" && c.SystemAccessToken != "" && c.ClientID != "" && ao.TenantID != "" {
		pipeCred, err := azidentity.NewAzurePipelinesCredential(
			ao.TenantID, c.ClientID, c.ServiceConnectionID, c.SystemAccessToken,
			&azidentity.AzurePipelinesCredentialOptions{
				ClientOptions: ao.ClientOptions,
			})
		if err != nil {
			return nil, err
		}

		return pipeCred, nil
	}

	return nil, nil
}

func getEnvValueCheckingMultipleVars(vars ...string) string {
	for _, v := range vars {
		if val := os.Getenv(v); val != "" {
			return val
		}
	}

	return ""
}
