// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/preflight"
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

// testInstallStore is an empty install-state store in a temporary directory, so
// admission never reads the host's record.
func testInstallStore(t *testing.T) *installstate.Store {
	t.Helper()

	dir := t.TempDir()

	return installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock"))
}

// Admission reads the injected store, not the host's: a completed record for
// this identity skips the checks, and one for another identity is refused.
func TestPreflightAdmitsAgainstTheInjectedStore(t *testing.T) {
	cfg := preflightConfig("https://127.0.0.1:1")
	path := writeConfigFile(t, cfg)

	id, err := bootstrapIdentity(&cfg)
	require.NoError(t, err)

	t.Run("complete record skips the checks", func(t *testing.T) {
		store := testInstallStore(t)
		r, err := installstate.NewRecord(id.MachineName, id.ConfigFingerprint)
		require.NoError(t, err)
		require.NoError(t, store.MarkComplete(r))

		var out bytes.Buffer

		h := &preflightHandler{
			cmdCtx:       &CommandContext{LogFormat: "text"},
			installation: store,
			configPath:   path,
			output:       "json",
			writer:       &out,
		}
		require.NoError(t, h.execute(context.Background()))

		var report preflight.Report
		require.NoError(t, json.Unmarshal(out.Bytes(), &report))
		assert.Empty(t, report.Checks)
	})

	t.Run("record for another identity is refused", func(t *testing.T) {
		store := testInstallStore(t)
		r, err := installstate.NewRecord("another-machine", id.ConfigFingerprint)
		require.NoError(t, err)
		require.NoError(t, store.Save(r))

		h := &preflightHandler{
			cmdCtx:       &CommandContext{LogFormat: "text"},
			installation: store,
			configPath:   path,
			output:       "json",
			writer:       &bytes.Buffer{},
		}
		require.ErrorContains(t, h.execute(context.Background()), "installation intent differs")
	})

	t.Run("unreadable record fails closed", func(t *testing.T) {
		dir := t.TempDir()
		// A directory where the record belongs cannot be read as one.
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "state", "install-state.json"), 0o755))

		h := &preflightHandler{
			cmdCtx:       &CommandContext{LogFormat: "text"},
			installation: installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock")),
			configPath:   path,
			output:       "json",
			writer:       &bytes.Buffer{},
		}
		require.ErrorContains(t, h.execute(context.Background()), "cannot read installation ownership")
	})
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
		installation:          testInstallStore(t),
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
		cmdCtx:       &CommandContext{LogFormat: "text"},
		installation: testInstallStore(t),
		configPath:   path,
		output:       "text",
		writer:       &out,
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
		installation:          testInstallStore(t),
		configPath:            path,
		ignorePreflightErrors: []string{"all"},
		output:                "text",
		writer:                &out,
	}

	require.NoError(t, h.execute(context.Background()))
	assert.Contains(t, out.String(), "[OK kubernetes-artifacts]")
}

type preflightResultChecker struct {
	result preflight.Result
}

func (c preflightResultChecker) Name() string { return c.result.Name }

func (c preflightResultChecker) Check(context.Context) []preflight.Result {
	return []preflight.Result{c.result}
}

func TestPreflightCLIReportsNVIDIAReadinessFailure(t *testing.T) {
	for _, tt := range []struct {
		name       string
		output     string
		ignore     bool
		otherError bool
	}{
		{name: "JSON failure", output: "json"},
		{name: "JSON ignored", output: "json", ignore: true},
		{name: "text failure", output: "text"},
		{name: "text ignored", output: "text", ignore: true},
		{name: "unrelated failure remains fatal", output: "json", ignore: true, otherError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := preflightTestServer(t)
			t.Cleanup(srv.Close)
			path := writeConfigFile(t, preflightConfig(srv.URL))

			var out bytes.Buffer

			other := preflight.OK("other-check", "host", "other diagnostics ran")
			if tt.otherError {
				other = preflight.Error("other-check", "host", "unrelated failure")
			}

			h := &preflightHandler{
				cmdCtx:       &CommandContext{LogFormat: "text"},
				installation: testInstallStore(t),
				writer:       &out,
				checks: []preflight.Checker{
					preflightResultChecker{result: preflight.Error("nvidia-driver", "NVIDIA diagnostic tooling",
						"NVIDIA GPUs are not ready: nvidia-smi did not report valid NVIDIA GPU enumeration")},
					preflightResultChecker{result: other},
				},
			}

			args := []string{"--config", path, "--output", tt.output}
			if tt.ignore {
				args = append(args, "--ignore-preflight-errors=nvidia-driver")
			}

			cmd := h.command()
			cmd.SetArgs(args)
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true

			err := cmd.ExecuteContext(context.Background())
			if tt.ignore && !tt.otherError {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}

			if tt.output == "json" {
				var report preflight.Report

				require.NoError(t, json.Unmarshal(out.Bytes(), &report))
				require.Len(t, report.Checks, 2)

				if tt.ignore && !tt.otherError {
					require.Equal(t, "ok", report.Status)
				} else {
					require.Equal(t, "failed", report.Status)
				}

				gpu := report.Checks[0]
				require.Equal(t, "nvidia-driver", gpu.Name)
				require.Equal(t, "NVIDIA diagnostic tooling", gpu.Target)
				require.Equal(t, tt.ignore, gpu.Ignored)

				if tt.ignore {
					require.Equal(t, preflight.SeverityWarning, gpu.Severity)
				} else {
					require.Equal(t, preflight.SeverityError, gpu.Severity)
				}

				require.Equal(t, "other-check", report.Checks[1].Name)
			} else {
				require.Contains(t, out.String(), "nvidia-driver")
				require.Contains(t, out.String(), "other diagnostics ran")

				if tt.ignore {
					require.Contains(t, out.String(), "[WARNING nvidia-driver]")
				} else {
					require.Contains(t, out.String(), "[ERROR nvidia-driver]")
				}
			}
		})
	}
}
