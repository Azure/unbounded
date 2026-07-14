package acrcredentialprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExchangeACRRefreshToken(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/oauth2/exchange", r.URL.Path)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "access_token", r.Form.Get("grant_type"))
		require.Equal(t, strings.TrimPrefix(serverURL(t, r), "https://"), r.Form.Get("service"))
		require.Equal(t, "aad-token", r.Form.Get("access_token"))
		require.Equal(t, "tenant-id", r.Form.Get("tenant"))

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]string{"refresh_token": "refresh-token"}))
	}))
	t.Cleanup(server.Close)

	refreshToken, err := exchangeACRRefreshToken(
		context.Background(),
		server.Client(),
		strings.TrimPrefix(server.URL, "https://"),
		"aad-token",
		"tenant-id",
	)
	require.NoError(t, err)
	require.Equal(t, "refresh-token", refreshToken)
}

func TestExchangeACRRefreshTokenDoesNotLeakAccessTokenOnHTTPError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	_, err := exchangeACRRefreshToken(
		context.Background(),
		server.Client(),
		strings.TrimPrefix(server.URL, "https://"),
		"sensitive-aad-token",
		"tenant-id",
	)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "sensitive-aad-token")
}

func serverURL(t *testing.T, r *http.Request) string {
	t.Helper()

	return "https://" + r.Host
}
