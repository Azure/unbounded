package azsdk

import (
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

type CLICredential struct{}

func (c *CLICredential) Configure(ao AuthConfig) (azcore.TokenCredential, error) {
	opts := &azidentity.AzureCLICredentialOptions{TenantID: ao.TenantID}

	cliCred, err := azidentity.NewAzureCLICredential(opts)
	if err != nil {
		return nil, fmt.Errorf("configure azure CLI cred source: %w", err)
	}

	return cliCred, nil
}
