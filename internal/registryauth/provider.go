// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	providerVersion  = "credentialprovider.kubelet.k8s.io/v1"
	tokenUsername    = "00000000-0000-0000-0000-000000000000"
	maxResponseBytes = 1024 * 1024
)

// Credential is the stable kubelet exec response's registry credential.
type Credential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// ProviderResponse implements Kubernetes' v1 credential-provider wire contract.
// Keeping these small wire types local avoids importing the kubelet executable.
type ProviderResponse struct {
	Kind          string                `json:"kind"`
	APIVersion    string                `json:"apiVersion"`
	CacheKeyType  string                `json:"cacheKeyType"`
	CacheDuration string                `json:"cacheDuration"`
	Auth          map[string]Credential `json:"auth"`
}

// Provider exchanges the infrastructure identity's tokens at validated ACR endpoints only.
type Provider struct {
	Credential azcore.TokenCredential
	HTTP       *http.Client
}

// NewProvider uses only the broker's explicit projected workload identity.
// It never selects IMDS, application identities, CLI credentials, or an ambient chain.
func NewProvider(cfg Config, tokenFile string) (*Provider, error) {
	if cfg.ClientID == "" || cfg.TenantID == "" || tokenFile == "" {
		return nil, errors.New("broker workload identity requires clientID, tenantID, and a projected token file")
	}

	credential, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		ClientID: cfg.ClientID, TenantID: cfg.TenantID, TokenFilePath: tokenFile,
	})
	if err != nil {
		return nil, errors.New("initialize broker workload identity: invalid identity configuration")
	}

	return &Provider{Credential: credential, HTTP: &http.Client{Timeout: 25 * time.Second}}, nil
}

// Run handles exactly one exec request. No token is written except to kubelet's
// response stream; diagnostic errors never include registry response bodies.
func (p *Provider) Run(ctx context.Context, input io.Reader, output io.Writer, registries []string) error {
	data, err := io.ReadAll(io.LimitReader(input, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return errors.New("read credential-provider request: input exceeds limit or is unreadable")
	}

	var request struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
		Image      string `json:"image"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return errors.New("invalid credential-provider JSON request")
	}

	if request.Kind != "CredentialProviderRequest" || request.APIVersion != providerVersion {
		return errors.New("unsupported credential-provider request kind or API version")
	}

	response := ProviderResponse{Kind: "CredentialProviderResponse", APIVersion: providerVersion, CacheKeyType: "Registry", CacheDuration: "5m", Auth: map[string]Credential{}}

	host, _, _, err := ParseImage(request.Image)
	if err == nil && slices.Contains(registries, host) {
		refresh, err := p.RefreshToken(ctx, host)
		if err != nil {
			return err
		}

		response.Auth[host] = Credential{Username: tokenUsername, Password: refresh}
	}

	return json.NewEncoder(output).Encode(response)
}

// RefreshToken obtains a credential for containerd's normal ACR OAuth handshake.
func (p *Provider) RefreshToken(ctx context.Context, registry string) (string, error) {
	if !IsACR(registry) {
		return "", errors.New("refusing token exchange with a non-ACR endpoint")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	token, err := p.Credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://containerregistry.azure.net/.default"}})
	if err != nil {
		return "", errors.New("federated token acquisition failed; verify clientID, tenantID, infrastructure ServiceAccount federation, and token projection")
	}

	parts := strings.Split(token.Token, ".")

	var claims struct {
		Tenant string `json:"tid"`
	}

	if len(parts) != 3 {
		return "", errors.New("federated identity returned a malformed token")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Tenant == "" {
		return "", errors.New("federated identity token has no tenant")
	}

	var result struct {
		Refresh string `json:"refresh_token"`
	}

	err = p.post(ctx, registry, "/oauth2/exchange", url.Values{
		"grant_type": {"access_token"}, "service": {registry}, "tenant": {claims.Tenant}, "access_token": {token.Token},
	}, &result)
	if err != nil {
		return "", err
	}

	if result.Refresh == "" {
		return "", errors.New("ACR exchange returned no refresh token")
	}

	return result.Refresh, nil
}

// Probe verifies pull access to the configured private image without downloading
// layers or logging authorization. A registry-root challenge alone is not proof.
func (p *Provider) Probe(ctx context.Context, image string) error {
	host, repository, reference, err := ParseImage(image)
	if err != nil {
		return err
	}

	refresh, err := p.RefreshToken(ctx, host)
	if err != nil {
		return err
	}

	var result struct {
		Access string `json:"access_token"`
	}
	if err := p.post(ctx, host, "/oauth2/token", url.Values{
		"grant_type": {"refresh_token"}, "service": {host}, "scope": {"repository:" + repository + ":pull"}, "refresh_token": {refresh},
	}, &result); err != nil {
		return err
	}

	if result.Access == "" {
		return errors.New("ACR returned no scoped pull token; verify AcrPull for the selected identity")
	}

	u := url.URL{Scheme: "https", Host: host, Path: "/v2/" + repository + "/manifests/" + reference}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+result.Access)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json")

	resp, err := p.client().Do(req)
	if err != nil {
		return errors.New("ACR private-image probe could not reach the registry")
	}
	defer closeQuietly(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ACR private-image probe returned HTTP %d; verify AcrPull or the registry repository-reader role and that the probe image exists", resp.StatusCode)
	}

	return nil
}

func (p *Provider) client() *http.Client {
	client := *p.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil

	return &client
}

func (p *Provider) post(ctx context.Context, registry, path string, form url.Values, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+registry+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client().Do(req)
	if err != nil {
		return errors.New("ACR token exchange could not reach the registry")
	}
	defer closeQuietly(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ACR token exchange returned HTTP %d; verify the configured federated pull identity and AcrPull or repository-reader assignment", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return errors.New("ACR token response exceeds limit or is unreadable")
	}

	if err := json.Unmarshal(body, result); err != nil {
		return errors.New("ACR token response is not valid JSON")
	}

	return nil
}
