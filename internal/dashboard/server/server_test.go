// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/dashboard/authz"
	"github.com/Azure/unbounded/internal/dashboard/examplemodule"
	"github.com/Azure/unbounded/internal/dashboard/registry"
	"github.com/Azure/unbounded/internal/dashboard/server"
)

// newStack wires a real example module behind an httptest server, then a
// dashboard Server pointed at it. It returns the dashboard handler and a
// cleanup func.
func newStack(t *testing.T, authorizer authz.Authorizer) http.Handler {
	t.Helper()

	moduleMux := http.NewServeMux()
	examplemodule.New().Routes(moduleMux, "/dashboard/v1")
	moduleSrv := httptest.NewServer(moduleMux)
	t.Cleanup(moduleSrv.Close)

	srv, err := server.New(server.Options{
		Registry: &registry.Config{
			Modules: []registry.Module{{ID: "example", BaseURL: moduleSrv.URL + "/dashboard/v1"}},
		},
		Authorizer: authorizer,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	return srv.Handler()
}

func get(t *testing.T, h http.Handler, path string) (*http.Response, string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	resp := rec.Result()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	return resp, string(body)
}

func TestOverviewRendersModule(t *testing.T) {
	h := newStack(t, authz.AllowAll{})

	resp, body := get(t, h, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	if !strings.Contains(body, "Example") {
		t.Errorf("overview missing module title; body=%s", body)
	}
}

func TestAPIModules(t *testing.T) {
	h := newStack(t, authz.AllowAll{})

	resp, body := get(t, h, "/api/dashboard/v1/modules")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	if !strings.Contains(body, `"id":"example"`) {
		t.Errorf("modules JSON missing example; body=%s", body)
	}
}

func TestResourceListAndDetail(t *testing.T) {
	h := newStack(t, authz.AllowAll{})

	if _, body := get(t, h, "/modules/example/resources/widgets"); !strings.Contains(body, "alpha") {
		t.Errorf("resource list missing widget alpha; body=%s", body)
	}

	resp, body := get(t, h, "/modules/example/resources/widgets/alpha")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d", resp.StatusCode)
	}

	if !strings.Contains(body, "Widget alpha") {
		t.Errorf("detail missing title; body=%s", body)
	}

	if !strings.Contains(body, "Toggle Health") {
		t.Errorf("detail missing action form; body=%s", body)
	}
}

func TestActionInvokesModuleAndRedirects(t *testing.T) {
	h := newStack(t, authz.AllowAll{})

	form := strings.NewReader("name=alpha&_return=/modules/example/resources/widgets/alpha")
	req := httptest.NewRequest(http.MethodPost, "/modules/example/actions/toggle-health", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}

	if loc := rec.Header().Get("Location"); loc != "/modules/example/resources/widgets/alpha" {
		t.Errorf("unexpected redirect location %q", loc)
	}
}

func TestUnknownModule404(t *testing.T) {
	h := newStack(t, authz.AllowAll{})

	if resp, _ := get(t, h, "/modules/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown module, got %d", resp.StatusCode)
	}
}

func TestOverviewPanelsRender(t *testing.T) {
	h := newStack(t, authz.AllowAll{})

	resp, body := get(t, h, "/modules/example")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	for _, want := range []string{"ub-graph", "ub-matrix-cell", "sse-connect", "panel-summary"} {
		if !strings.Contains(body, want) {
			t.Errorf("module overview missing %q", want)
		}
	}
}

func TestPanelFragmentRefetch(t *testing.T) {
	h := newStack(t, authz.AllowAll{})

	resp, body := get(t, h, "/modules/example/panels/summary")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Fragment must be the panel only, not a full HTML page.
	if strings.Contains(body, "<html") {
		t.Errorf("panel fragment should not include full page chrome")
	}

	if !strings.Contains(body, "panel-summary") {
		t.Errorf("panel fragment missing panel wrapper; body=%s", body)
	}
}

func TestPanelFragmentUnknownKey404(t *testing.T) {
	h := newStack(t, authz.AllowAll{})

	if resp, _ := get(t, h, "/modules/example/panels/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown panel key, got %d", resp.StatusCode)
	}
}

func TestAPIGraphAndMatrix(t *testing.T) {
	h := newStack(t, authz.AllowAll{})

	if _, body := get(t, h, "/api/dashboard/v1/modules/example/graph"); !strings.Contains(body, `"nodes"`) {
		t.Errorf("graph JSON missing nodes; body=%s", body)
	}

	if _, body := get(t, h, "/api/dashboard/v1/modules/example/matrix"); !strings.Contains(body, `"rows"`) {
		t.Errorf("matrix JSON missing rows; body=%s", body)
	}

	// The JS graph primitive consumes this HTML-path endpoint too.
	if resp, _ := get(t, h, "/modules/example/graph"); resp.StatusCode != http.StatusOK {
		t.Errorf("graph data endpoint status = %d", resp.StatusCode)
	}
}

func TestStreamProxyEmitsSummaryEvent(t *testing.T) {
	h := newStack(t, authz.AllowAll{})

	ts := httptest.NewServer(h)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/modules/example/stream", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// Read just enough to observe the first event, then cancel to close the
	// long-lived stream.
	buf := make([]byte, 256)

	n, err := resp.Body.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("reading stream: %v", err)
	}

	if !strings.Contains(string(buf[:n]), "event: summary") {
		t.Errorf("stream missing summary event; got %q", string(buf[:n]))
	}
}
