// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azsdk

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

type ManagedIdentityCredential struct {
	// ClientID is a managed identity client ID or ARM resource ID.
	ClientID string
	// IMDSTimeout specifies a timeout for trying to communicate with the Azure instance metadata
	// service.
	IMDSTimeout time.Duration
}

func (c *ManagedIdentityCredential) Configure(ao AuthConfig) (azcore.TokenCredential, error) {
	if c.ClientID == "" {
		c.ClientID = os.Getenv(EnvAzureClientID)
	}

	var mik azidentity.ManagedIDKind
	if isManagedIdentityResourceID(c.ClientID) {
		mik = azidentity.ResourceID(c.ClientID)
	}

	// Don't allow an empty string for ManagedClustersCli ID. Updates to the Azure SDK, don't allow this.
	if mik == nil && c.ClientID != "" {
		mik = azidentity.ClientID(c.ClientID)
	}

	msiCred, err := azidentity.NewManagedIdentityCredential(&azidentity.ManagedIdentityCredentialOptions{
		ClientOptions: ao.ClientOptions,
		ID:            mik,
	})
	if err != nil {
		return nil, err
	}

	return &managedIdentityCredentialWrapper{msiCred, mik, c.IMDSTimeout}, nil
}

// managedIdentityCredentialWrapper wraps a ManagedIdentityCredential to allow overriding its
// default timeout.
//
// This pattern is based on https://github.com/Azure/azure-sdk-for-go/issues/19699#issuecomment-1379290680.
type managedIdentityCredentialWrapper struct {
	cred        *azidentity.ManagedIdentityCredential
	id          azidentity.ManagedIDKind
	imdsTimeout time.Duration
}

func (w *managedIdentityCredentialWrapper) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	timeout := time.Second * 10

	if w.imdsTimeout > 0 {
		timeout = w.imdsTimeout
	}

	debugLog("Attempting to GetToken(...) from IMDS (timeout=%s)", timeout)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tk, err := w.cred.GetToken(ctx, opts)
	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		// Timeout: Signal the chain to try its next credential, if any.
		err = azidentity.NewCredentialUnavailableError("managed identity timed out")
	}

	return tk, err
}
