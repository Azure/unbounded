package acrcredentialprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeCredentialSource struct {
	registry string
	cred     RegistryCredential
	err      error
}

func (f *fakeCredentialSource) Credential(_ context.Context, registry string) (RegistryCredential, error) {
	f.registry = registry

	return f.cred, f.err
}

func TestRegistryFromImage(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "acr image",
			image: "example.azurecr.io/ns/app:tag",
			want:  "example.azurecr.io",
		},
		{
			name:  "acr image with path digest",
			image: "Example.AzureCR.io/ns/app@sha256:abc",
			want:  "example.azurecr.io",
		},
		{
			name:  "docker hub shorthand",
			image: "library/busybox:latest",
			want:  "",
		},
		{
			name:  "localhost registry",
			image: "localhost:5000/app:latest",
			want:  "localhost:5000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, RegistryFromImage(tt.image))
		})
	}
}

func TestResponseForACRImage(t *testing.T) {
	source := &fakeCredentialSource{cred: RegistryCredential{Username: "user", Password: "refresh-token"}}
	provider := Provider{CredentialSource: source, CacheDuration: 30 * time.Minute}

	resp, err := provider.Response(context.Background(), CredentialProviderRequest{
		APIVersion: APIVersion,
		Kind:       RequestKind,
		Image:      "demo.azurecr.io/team/app:latest",
	})
	require.NoError(t, err)
	require.Equal(t, "demo.azurecr.io", source.registry)
	require.Equal(t, APIVersion, resp.APIVersion)
	require.Equal(t, ResponseKind, resp.Kind)
	require.Equal(t, CacheKeyTypeRegistry, resp.CacheKeyType)
	require.Equal(t, "30m0s", resp.CacheDuration)
	require.Equal(t, AuthConfig{Username: "user", Password: "refresh-token"}, resp.Auth["demo.azurecr.io"])
}

func TestResponseForNonACRImageReturnsNoAuth(t *testing.T) {
	source := &fakeCredentialSource{cred: RegistryCredential{Username: "user", Password: "refresh-token"}}
	provider := Provider{CredentialSource: source}

	resp, err := provider.Response(context.Background(), CredentialProviderRequest{Image: "registry.k8s.io/pause:3.10"})
	require.NoError(t, err)
	require.Empty(t, source.registry)
	require.Nil(t, resp.Auth)
	require.Equal(t, DefaultCacheDuration.String(), resp.CacheDuration)
}

func TestResponseRejectsUnsupportedAPIVersion(t *testing.T) {
	provider := Provider{CredentialSource: &fakeCredentialSource{}}

	_, err := provider.Response(context.Background(), CredentialProviderRequest{
		APIVersion: "credentialprovider.kubelet.k8s.io/v1beta1",
		Image:      "demo.azurecr.io/app:latest",
	})
	require.Error(t, err)
}

func TestResponseReturnsCredentialError(t *testing.T) {
	provider := Provider{CredentialSource: &fakeCredentialSource{err: errors.New("boom")}}

	_, err := provider.Response(context.Background(), CredentialProviderRequest{Image: "demo.azurecr.io/app:latest"})
	require.ErrorContains(t, err, "get credential")
}

func TestHandleRoundTrip(t *testing.T) {
	provider := Provider{CredentialSource: &fakeCredentialSource{cred: RegistryCredential{Username: "u", Password: "p"}}}
	req := CredentialProviderRequest{APIVersion: APIVersion, Kind: RequestKind, Image: "demo.azurecr.io/app:latest"}
	input, err := json.Marshal(req)
	require.NoError(t, err)

	var output bytes.Buffer
	err = provider.Handle(context.Background(), bytes.NewReader(input), &output)
	require.NoError(t, err)

	var resp CredentialProviderResponse
	require.NoError(t, json.Unmarshal(output.Bytes(), &resp))
	require.Equal(t, AuthConfig{Username: "u", Password: "p"}, resp.Auth["demo.azurecr.io"])
}
