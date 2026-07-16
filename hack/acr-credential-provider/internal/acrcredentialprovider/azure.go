// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package acrcredentialprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	DefaultAADScope = "https://management.azure.com/.default"
)

type AzureCredentialSource struct {
	TokenCredential azcore.TokenCredential
	HTTPClient      *http.Client
	AADScope        string
	TenantID        string
}

func NewDefaultAzureCredentialSource() (*AzureCredentialSource, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create default Azure credential: %w", err)
	}

	return &AzureCredentialSource{
		TokenCredential: cred,
		TenantID:        os.Getenv("AZURE_TENANT_ID"),
	}, nil
}

func (s *AzureCredentialSource) Credential(ctx context.Context, registry string) (RegistryCredential, error) {
	if s.TokenCredential == nil {
		return RegistryCredential{}, fmt.Errorf("no Azure token credential configured")
	}

	aadScope := s.AADScope
	if aadScope == "" {
		aadScope = DefaultAADScope
	}

	token, err := s.TokenCredential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{aadScope}})
	if err != nil {
		return RegistryCredential{}, fmt.Errorf("get Azure AD token: %w", err)
	}

	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	refreshToken, err := exchangeACRRefreshToken(ctx, client, registry, token.Token, s.TenantID)
	if err != nil {
		return RegistryCredential{}, err
	}

	return RegistryCredential{
		Username: ACRTokenUsername,
		Password: refreshToken,
	}, nil
}

func exchangeACRRefreshToken(ctx context.Context, client *http.Client, registry, accessToken, tenantID string) (string, error) {
	registry = strings.TrimPrefix(registry, "https://")
	registry = strings.TrimPrefix(registry, "http://")

	form := url.Values{}
	form.Set("grant_type", "access_token")
	form.Set("service", registry)
	form.Set("access_token", accessToken)

	if tenantID != "" {
		form.Set("tenant", tenantID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+registry+"/oauth2/exchange", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create ACR token exchange request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange Azure AD token for ACR refresh token: %w", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ACR token exchange failed for %q: %s", registry, resp.Status)
	}

	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode ACR token exchange response: %w", err)
	}

	if body.RefreshToken == "" {
		return "", fmt.Errorf("ACR token exchange response did not include refresh_token")
	}

	return body.RefreshToken, nil
}
