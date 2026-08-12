// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
)

func preflightConfig(apiServer string) provision.UnboundedAgentConfig {
	cfg := sampleConfig()
	cfg.Kubelet.ApiServer = apiServer

	parsed, err := url.Parse(apiServer)
	if err == nil && parsed.Host != "" {
		cfg.OCIImage = parsed.Host + "/unbounded/rootfs:v1"
	} else {
		cfg.OCIImage = "127.0.0.1:1/unbounded/rootfs:v1"
	}

	baseURL := strings.TrimRight(apiServer, "/")
	cfg.Downloads = &provision.AgentDownloads{
		Kubernetes: &provision.AgentDownloadSource{URL: baseURL + "/kubernetes/%s/%s/%s"},
		Containerd: &provision.AgentDownloadSource{URL: baseURL + "/containerd/%s/containerd-%s-linux-%s.tar.gz"},
		Runc:       &provision.AgentDownloadSource{URL: baseURL + "/runc/%s/runc.%s"},
		CNI:        &provision.AgentDownloadSource{URL: baseURL + "/cni/%s/%s/cni-plugins-v%s.tgz"},
		Crictl:     &provision.AgentDownloadSource{URL: baseURL + "/crictl/%s/crictl-v%s-%s-%s.tar.gz"},
	}

	return cfg
}

func preflightTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	manifest := `{
		"schemaVersion":2,
		"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"config":{
			"mediaType":"application/vnd.oci.image.config.v1+json",
			"digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222",
			"size":2
		},
		"layers":[]
	}`

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/unbounded/rootfs/manifests/") {
			w.Header().Set("Docker-Content-Digest", "sha256:1111111111111111111111111111111111111111111111111111111111111111")
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.WriteHeader(http.StatusOK)

			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(manifest))
			}

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
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
	srv := preflightTestServer(t)
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := h.execute(ctx)
	require.Error(t, err)
	assert.Contains(t, out.String(), "[ERROR api-server-reachable]")
	assert.Contains(t, out.String(), `API server "https://127.0.0.1:1" is not reachable:`)
}

func TestPreflightTextOutputIncludesOK(t *testing.T) {
	srv := preflightTestServer(t)
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
	assert.Contains(t, out.String(), "[OK kubernetes-artifacts]")
}
