// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestControllerStatusJSONPreservesSummary(t *testing.T) {
	nodes := make([]string, 11)
	for i := range nodes {
		nodes[i] = fmt.Sprintf(`{"name":"node-%d","k8sReady":"Ready","futureNodeField":{"value":9007199254740993}}`, i)
	}

	fixture := `{
		"seq":9007199254740993,"nodeCount":11,"siteCount":2,"pullEnabled":false,
		"buildInfo":{"commit":"024fd308","futureBuildField":"preserved"},
		"leaderInfo":{"podName":"leader"},"sites":[{"name":"site"}],
		"gatewayPools":[],"warnings":["warning"],"errors":["explicit error"],
		"futureMetadata":{"precision":9007199254740993},
		"nodeSummaries":[` + strings.Join(nodes, ",") + `]}`

	for _, pretty := range []bool{true, false} {
		t.Run(fmt.Sprintf("pretty=%t", pretty), func(t *testing.T) {
			var calls atomic.Int32

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assertControllerOverviewRequest(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, fixture)
			}))
			defer server.Close()

			cmd := newControllerRootCommand(newControllerStatusTestRuntime(t, server.URL))

			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"status-json", fmt.Sprintf("--pretty=%t", pretty)})

			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}

			var want, got bytes.Buffer
			if err := json.Compact(&want, []byte(fixture)); err != nil {
				t.Fatal(err)
			}

			if err := json.Compact(&got, out.Bytes()); err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(want.Bytes(), got.Bytes()) {
				t.Fatalf("summary fields changed:\nwant %s\ngot %s", want.String(), got.String())
			}

			if calls.Load() != 1 {
				t.Errorf("export made %d requests, want one overview request", calls.Load())
			}

			if !strings.HasSuffix(out.String(), "\n") ||
				strings.Contains(strings.TrimSuffix(out.String(), "\n"), "\n") != pretty {
				t.Errorf("incorrect pretty=%t formatting: %s", pretty, out.String())
			}
		})
	}
}

func TestControllerStatusJSONLegacyCompatibility(t *testing.T) {
	for _, fixture := range []string{
		strings.Replace(legacyOverviewFixture, `"seq":7`, `"buildInfo":{"commit":"old"},"futureMetadata":42,"seq":7`, 1),
		`{"nodeSummaries":[{"name":"current","futureNodeField":42}],"nodeCount":1,"nodes":"ignored legacy details"}`,
		`{"nodes":[]}`,
		`{"nodeSummaries":[],"nodeCount":0}`,
	} {
		for _, pretty := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/pretty=%t", fixture, pretty), func(t *testing.T) {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assertControllerOverviewRequest(t, r)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, fixture)
				}))
				defer server.Close()

				cmd := newControllerRootCommand(newControllerStatusTestRuntime(t, server.URL))

				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(io.Discard)
				cmd.SetArgs([]string{"status-json", fmt.Sprintf("--pretty=%t", pretty)})

				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}

				var fields map[string]json.RawMessage
				if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
					t.Fatal(err)
				}

				if fields["nodeSummaries"] == nil || fields["nodeCount"] == nil {
					t.Fatalf("missing summary fields: %s", out.String())
				}

				for _, forbidden := range []string{`"nodes"`, `"peers"`, `"routingTable"`, `"bpfEntries"`, "secret-detail", "ignored legacy details"} {
					if strings.Contains(out.String(), forbidden) {
						t.Errorf("export leaked %q: %s", forbidden, out.String())
					}
				}

				summary, err := decodeClusterSummary(out.Bytes())
				if err != nil {
					t.Fatal(err)
				}

				if strings.Contains(fixture, `"buildInfo"`) {
					if summary.NodeCount != 1 || summary.NodeSummaries[0].PeerCount != 2 ||
						summary.NodeSummaries[0].FirstError != "not ready" || summary.Seq != 7 {
						t.Fatalf("lost projected summary facts: %+v", summary)
					}

					var build map[string]string
					if err := json.Unmarshal(fields["buildInfo"], &build); err != nil {
						t.Fatal(err)
					}

					if build["commit"] != "old" || string(fields["futureMetadata"]) != "42" {
						t.Fatalf("lost legacy metadata: %s", out.String())
					}
				}

				if strings.Contains(fixture, `"current"`) && !strings.Contains(out.String(), `"futureNodeField"`) {
					t.Fatalf("lost current summary field: %s", out.String())
				}
			})
		}
	}
}

func TestControllerStatusJSONFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "invalid JSON", body: `{`},
		{name: "missing nodes", body: `{}`},
		{name: "null", body: `null`},
		{name: "invalid summaries", body: `{"nodeSummaries":"bad"}`},
		{name: "invalid legacy nodes", body: `{"nodes":"bad"}`},
		{name: "unauthorized", status: http.StatusUnauthorized, body: "Unauthorized"},
		{name: "forbidden", status: http.StatusForbidden, body: "Forbidden"},
		{name: "server error", status: http.StatusInternalServerError, body: "controller failed"},
	} {
		for _, pretty := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/pretty=%t", tc.name, pretty), func(t *testing.T) {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || strings.Contains(r.URL.Path, "/details") {
						t.Errorf("failure requested diagnostics: %s %s", r.Method, r.URL)
					}

					w.Header().Set("Content-Type", "application/json")

					if tc.status != 0 {
						w.WriteHeader(tc.status)
					} else {
						assertControllerOverviewRequest(t, r)
					}

					_, _ = io.WriteString(w, tc.body)
				}))
				defer server.Close()

				cmd := newControllerRootCommand(newControllerStatusTestRuntime(t, server.URL))

				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(io.Discard)
				cmd.SilenceUsage = true
				cmd.SetArgs([]string{"status-json", fmt.Sprintf("--pretty=%t", pretty)})

				wantError := "cluster overview"
				if tc.status != 0 {
					wantError = "fetch /status/json failed"
				}

				if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), wantError) {
					t.Fatalf("failed response error = %v, want %q", err, wantError)
				}

				if out.Len() != 0 {
					t.Fatalf("failed export wrote output: %s", out.String())
				}
			})
		}
	}
}

func TestControllerStatusJSONOutputFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertControllerOverviewRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"nodeSummaries":[],"nodeCount":0}`)
	}))
	defer server.Close()

	for _, pretty := range []bool{true, false} {
		cmd := newControllerRootCommand(newControllerStatusTestRuntime(t, server.URL))
		outputErr := errors.New("output closed")
		cmd.SetOut(controllerStatusErrorWriter{err: outputErr})
		cmd.SetErr(io.Discard)
		cmd.SilenceUsage = true
		cmd.SetArgs([]string{"status-json", fmt.Sprintf("--pretty=%t", pretty)})

		if err := cmd.Execute(); !errors.Is(err, outputErr) {
			t.Fatalf("output failure = %v, want %v", err, outputErr)
		}
	}
}

type controllerStatusErrorWriter struct {
	err error
}

func (w controllerStatusErrorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func assertControllerOverviewRequest(t *testing.T, r *http.Request) {
	t.Helper()

	if r.Method != http.MethodGet || r.URL.Path != "/apis/status.net.unbounded-cloud.io/v1alpha1/status/json" {
		t.Errorf("export requested non-overview endpoint: %s %s", r.Method, r.URL)
	}

	if r.Header.Get("Authorization") != "Bearer test-token" {
		t.Error("export did not reuse Kubernetes authentication")
	}
}

func newControllerStatusTestRuntime(t *testing.T, serverURL string) *pluginRuntime {
	t.Helper()

	rt := newDetailTestRuntime(t, serverURL)
	*rt.configFlags.Insecure = true

	return rt
}
