// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package acrcredentialprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	APIVersion = "credentialprovider.kubelet.k8s.io/v1"

	RequestKind  = "CredentialProviderRequest"
	ResponseKind = "CredentialProviderResponse"

	CacheKeyTypeRegistry = "Registry"

	DefaultCacheDuration = time.Hour

	acrLoginServerSuffix = ".azurecr.io"

	// Azure accepts this sentinel username with an ACR refresh token as the
	// password, matching `az acr login --expose-token` behavior.
	ACRTokenUsername = "00000000-0000-0000-0000-000000000000"
)

type CredentialProviderRequest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Image      string `json:"image"`
}

type CredentialProviderResponse struct {
	APIVersion    string                `json:"apiVersion"`
	Kind          string                `json:"kind"`
	CacheKeyType  string                `json:"cacheKeyType"`
	CacheDuration string                `json:"cacheDuration,omitempty"`
	Auth          map[string]AuthConfig `json:"auth,omitempty"`
}

type AuthConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type RegistryCredential struct {
	Username string
	Password string
}

type RegistryCredentialSource interface {
	Credential(ctx context.Context, registry string) (RegistryCredential, error)
}

type Provider struct {
	CredentialSource RegistryCredentialSource
	CacheDuration    time.Duration
}

func (p Provider) Handle(ctx context.Context, input io.Reader, output io.Writer) error {
	var req CredentialProviderRequest
	if err := json.NewDecoder(input).Decode(&req); err != nil {
		return fmt.Errorf("decode credential provider request: %w", err)
	}

	resp, err := p.Response(ctx, req)
	if err != nil {
		return err
	}

	encoder := json.NewEncoder(output)
	if err := encoder.Encode(resp); err != nil {
		return fmt.Errorf("encode credential provider response: %w", err)
	}

	return nil
}

func (p Provider) Response(ctx context.Context, req CredentialProviderRequest) (CredentialProviderResponse, error) {
	if req.APIVersion != "" && req.APIVersion != APIVersion {
		return CredentialProviderResponse{}, fmt.Errorf("unsupported credential provider apiVersion %q", req.APIVersion)
	}

	registry := RegistryFromImage(req.Image)
	resp := CredentialProviderResponse{
		APIVersion:    APIVersion,
		Kind:          ResponseKind,
		CacheKeyType:  CacheKeyTypeRegistry,
		CacheDuration: durationString(p.cacheDuration()),
	}

	if !IsACRLoginServer(registry) {
		return resp, nil
	}

	if p.CredentialSource == nil {
		return CredentialProviderResponse{}, fmt.Errorf("credential source is required for ACR image %q", req.Image)
	}

	cred, err := p.CredentialSource.Credential(ctx, registry)
	if err != nil {
		return CredentialProviderResponse{}, fmt.Errorf("get credential for registry %q: %w", registry, err)
	}

	resp.Auth = map[string]AuthConfig{
		registry: {
			Username: cred.Username,
			Password: cred.Password,
		},
	}

	return resp, nil
}

func RegistryFromImage(image string) string {
	image = strings.TrimSpace(image)
	image = strings.TrimPrefix(image, "http://")
	image = strings.TrimPrefix(image, "https://")

	first, _, ok := strings.Cut(image, "/")
	if !ok {
		return ""
	}

	first = strings.ToLower(first)
	if first == "localhost" || strings.Contains(first, ".") || strings.Contains(first, ":") {
		return first
	}

	return ""
}

func IsACRLoginServer(registry string) bool {
	registry = strings.ToLower(strings.TrimSpace(registry))

	return strings.HasSuffix(registry, acrLoginServerSuffix)
}

func (p Provider) cacheDuration() time.Duration {
	if p.CacheDuration <= 0 {
		return DefaultCacheDuration
	}

	return p.CacheDuration
}

func durationString(d time.Duration) string {
	return d.Truncate(time.Second).String()
}
