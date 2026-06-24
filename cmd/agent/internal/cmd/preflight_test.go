// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
)

func preflightConfig(apiServer string) provision.UnboundedAgentConfig {
	cfg := sampleConfig()
	cfg.Kubelet.ApiServer = apiServer
	cfg.OCIImage = "registry.example.com/unbounded/rootfs:v1"

	return cfg
}

func TestNewCmdPreflight(t *testing.T) {
	cmd := newCmdPreflight(&CommandContext{LogFormat: "text"})

	assert.Equal(t, "preflight", cmd.Use)
	assert.NotNil(t, cmd.Flags().Lookup("config"))
	assert.NotNil(t, cmd.Flags().Lookup("ignore-preflight-errors"))
	assert.NotNil(t, cmd.Flags().Lookup("fail-on-warnings"))
	assert.NotNil(t, cmd.Flags().Lookup("output"))
}

func TestPreflightJSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeConfigFile(t, preflightConfig(srv.URL))

	var out bytes.Buffer

	h := &preflightHandler{
		cmdCtx:                &CommandContext{LogFormat: "text"},
		configPath:            path,
		ignorePreflightErrors: []string{"all"},
		output:                "json",
		writer:                &out,
	}

	require.NoError(t, h.execute(context.Background()))

	var report struct {
		Status string `json:"status"`
		Checks []struct {
			Name    string `json:"name"`
			Ignored bool   `json:"ignored"`
		} `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	assert.Equal(t, "ok", report.Status)
	assert.NotEmpty(t, report.Checks)
}

func TestPreflightTextOutputError(t *testing.T) {
	path := writeConfigFile(t, preflightConfig("https://127.0.0.1:1"))

	var out bytes.Buffer

	h := &preflightHandler{
		cmdCtx:     &CommandContext{LogFormat: "text"},
		configPath: path,
		output:     "text",
		writer:     &out,
	}

	err := h.execute(context.Background())
	require.Error(t, err)
	assert.Contains(t, out.String(), "[ERROR api-server-reachable]")
	assert.NotContains(t, out.String(), "127.0.0.1")
}

func TestPreflightTextOutputIncludesOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeConfigFile(t, preflightConfig(srv.URL))

	var out bytes.Buffer

	h := &preflightHandler{
		cmdCtx:                &CommandContext{LogFormat: "text"},
		configPath:            path,
		ignorePreflightErrors: []string{"all"},
		output:                "text",
		writer:                &out,
	}

	require.NoError(t, h.execute(context.Background()))
	assert.Contains(t, out.String(), "[OK agent-config]")
}
