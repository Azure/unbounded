//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const lifecycleSocket = "/run/racer/gantry/client/socket"

// A GET for the cold object sends a prefix, then holds the body. Cancellation
// is observed independently of release so the test can attempt a late write
// after the replacement listener is serving. HEAD always remains available.
type lifecycleOrigin struct {
	mu                                   sync.Mutex
	warm, cold                           string
	blobs                                map[string][]byte
	gets                                 map[string]int
	deny, hold                           bool
	started, canceled, release, finished chan struct{}
	once                                 sync.Once
	ctx                                  context.Context
}

func newLifecycleOrigin(t *testing.T) *lifecycleOrigin {
	t.Helper()

	warm := bytes.Repeat([]byte("populated-cache-page\n"), 16384)
	cold := bytes.Repeat([]byte("outstanding-cache-fill\n"), 16384)
	o := &lifecycleOrigin{
		warm: digest(warm), cold: digest(cold),
		blobs: map[string][]byte{digest(warm): warm, digest(cold): cold},
		gets:  make(map[string]int), hold: true, ctx: t.Context(),
		started: make(chan struct{}), canceled: make(chan struct{}),
		release: make(chan struct{}), finished: make(chan struct{}),
	}
	t.Cleanup(o.unblock)

	return o
}

func (o *lifecycleOrigin) unblock() { o.once.Do(func() { close(o.release) }) }

func (o *lifecycleOrigin) allow(allow bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.deny = !allow
}

func (o *lifecycleOrigin) count(id string) int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.gets[id]
}

func (o *lifecycleOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v2/fixture/lifecycle/blobs/")

	body, ok := o.blobs[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	o.mu.Lock()

	get := r.Method == http.MethodGet
	if get {
		o.gets[id]++
	}

	deny := get && o.deny

	hold := get && !deny && id == o.cold && o.hold
	if hold {
		o.hold = false
	}
	o.mu.Unlock()

	if deny {
		http.Error(w, "lifecycle origin GET disabled", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", id)

	if !hold {
		http.ServeContent(w, r, id, time.Time{}, bytes.NewReader(body))
		return
	}

	defer close(o.finished)

	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)

	_, _ = w.Write(body[:1024])
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}

	close(o.started)

	select {
	case <-r.Context().Done():
		close(o.canceled)
	case <-o.release:
	case <-o.ctx.Done():
		return
	}

	select {
	case <-o.release:
		_, _ = w.Write(body[1024:])
	case <-o.ctx.Done():
	}
}

type lifecycleResponse struct {
	status int
	header http.Header
	body   []byte
	err    error
}

// No test assertions run in this helper: the outstanding request runs in a
// goroutine and reports its complete result to the owning test goroutine.
func (h *harness) lifecycleRead(ctx context.Context, node peerNode, socket, id, method string) lifecycleResponse {
	args := []string{
		"exec", node.name, "curl", "--include", "--silent", "--show-error", "--max-time", "90", "--noproxy", "*", "--unix-socket", socket,
		"-H", "Host: racer", "-H", "If-Match: \"" + id + "\"", "-H", `Racer-Metadata: {"version":1,"registry":"` + registry + `","repository":"fixture/lifecycle","kind":"blob"}`,
	}
	if method == http.MethodHead {
		args = append(args, "--head")
	} else {
		args = append(args, "-H", "Range: bytes=0-16777215")
	}

	args = append(args, "http://racer/v1/objects/"+strings.TrimPrefix(id, "sha256:"))

	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stderr = &stderr

	raw, err := cmd.Output()
	if err != nil {
		return lifecycleResponse{err: fmt.Errorf("curl: %w: %s", err, stderr.String())}
	}

	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), &http.Request{Method: method})
	if err != nil {
		return lifecycleResponse{err: err}
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)

	return lifecycleResponse{status: response.StatusCode, header: response.Header, body: body, err: err}
}

func (h *harness) lifecycleBytes(node peerNode, socket, id string, fixture *lifecycleOrigin) {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(h.ctx, 25*time.Second)
	defer cancel()

	r := h.lifecycleRead(ctx, node, socket, id, http.MethodGet)
	require.NoError(h.t, r.err)
	require.Equal(h.t, http.StatusPartialContent, r.status)
	require.Equal(h.t, "\""+id+"\"", r.header.Get("ETag"))
	require.Equal(h.t, fmt.Sprintf("bytes 0-%d/%d", len(fixture.blobs[id])-1, len(fixture.blobs[id])), r.header.Get("Content-Range"))
	require.Equal(h.t, fixture.blobs[id], r.body)
}

func (h *harness) awaitLifecycle(signal <-chan struct{}, message string) {
	h.t.Helper()

	select {
	case <-signal:
	case <-time.After(45 * time.Second):
		h.t.Fatal(message)
	case <-h.ctx.Done():
		h.t.Fatal(h.ctx.Err(), ": ", message)
	}
}

func (h *harness) verifyCacheRecreation(nodes [2]peerNode, fixture *lifecycleOrigin) {
	h.t.Helper()
	oldUID := strings.TrimSpace(h.kubectl("get", "clustercache", "gantry", "-o", "jsonpath={.metadata.uid}"))
	require.NotEmpty(h.t, oldUID)
	// A process restart would trivially discard memory and evade live retirement.
	podState := func() string {
		return h.kubectl("get", "pods", "-n", namespace, "-l", "app.kubernetes.io/name=racer-dataplane", "--sort-by=.metadata.uid", "-o", `jsonpath={range .items[*]}{.metadata.uid}{":"}{.status.containerStatuses[*].restartCount}{"\n"}{end}`)
	}
	beforePods := podState()

	oldSocket := "/run/racer/gantry/client/e2e-old-" + oldUID
	for _, node := range nodes {
		h.run("docker", "exec", node.name, "ln", lifecycleSocket, oldSocket)
		h.t.Cleanup(func() { h.command(context.Background(), "docker", "exec", node.name, "rm", "-f", oldSocket) })
		h.lifecycleBytes(node, oldSocket, fixture.warm, fixture)
	}

	warmGets := fixture.count(fixture.warm)
	require.Positive(h.t, warmGets)

	for _, node := range nodes {
		h.lifecycleBytes(node, lifecycleSocket, fixture.warm, fixture)
	}

	require.Equal(h.t, warmGets, fixture.count(fixture.warm), "old UID must have populated reusable pages")

	ctx, cancel := context.WithTimeout(h.ctx, 90*time.Second)
	defer cancel()

	result := make(chan lifecycleResponse, 1)
	readStarted := time.Now()

	go func() { result <- h.lifecycleRead(ctx, nodes[0], lifecycleSocket, fixture.cold, http.MethodGet) }()

	h.awaitLifecycle(fixture.started, "cold fill never reached held origin body")

	select {
	case r := <-result:
		h.t.Fatalf("held read completed before deletion: %+v", r)
	case <-fixture.canceled:
		h.t.Fatal("held fill canceled before deletion")
	default:
	}

	fixture.allow(false)

	retireBy := time.NewTimer(20 * time.Second)
	defer retireBy.Stop()

	h.kubectl("delete", "clustercache", "gantry", "--wait=true", "--timeout=15s")
	// Racer's ordinary request timeout is 30 seconds. Cancellation must beat it
	// so a timed-out fill cannot masquerade as controller-driven retirement.
	select {
	case <-fixture.canceled:
	case <-retireBy.C:
		h.t.Fatal("cache deletion did not cancel the held fill before the ordinary request timeout")
	case <-h.ctx.Done():
		h.t.Fatal(h.ctx.Err())
	}

	require.Less(h.t, time.Since(readStarted), 25*time.Second, "ordinary 30-second request expiry must not satisfy cancellation")
	// Observe the empty catalog on both Rust dataplanes before recreating. This
	// cannot be satisfied by Kubernetes deletion alone or a manually staged cut.
	for _, node := range nodes {
		require.Eventually(h.t, func() bool {
			probe, stop := context.WithTimeout(h.ctx, 2*time.Second)
			defer stop()

			return exec.CommandContext(probe, "docker", "exec", node.name, "test", "!", "-S", lifecycleSocket).Run() == nil
		}, 45*time.Second, 200*time.Millisecond, "controller removal never retired %s socket", node.name)
	}

	select {
	case r := <-result:
		require.True(h.t, r.err != nil || r.status >= 400, "removed UID completed a held read successfully")
	case <-ctx.Done():
		h.t.Fatal("old read did not retire before its deadline")
	}

	require.NoError(h.t, ctx.Err(), "client timeout must not masquerade as retirement")
	h.apply("apiVersion: racer.unbounded-cloud.io/v1alpha1\nkind: ClusterCache\nmetadata:\n  name: gantry\n")
	newUID := strings.TrimSpace(h.kubectl("get", "clustercache", "gantry", "-o", "jsonpath={.metadata.uid}"))
	require.NotEmpty(h.t, newUID)
	require.NotEqual(h.t, oldUID, newUID)

	for _, node := range nodes {
		require.Eventually(h.t, func() bool {
			probe, stop := context.WithTimeout(h.ctx, 2*time.Second)
			defer stop()

			r := h.lifecycleRead(probe, node, lifecycleSocket, fixture.warm, http.MethodHead)

			return r.err == nil && r.status == http.StatusOK
		}, 60*time.Second, 250*time.Millisecond, "replacement cache socket not ready on %s", node.name)
		require.NotEqual(h.t,
			h.run("docker", "exec", node.name, "stat", "--format=%d:%i", oldSocket),
			h.run("docker", "exec", node.name, "stat", "--format=%d:%i", lifecycleSocket),
			"same-name replacement must own a new socket inode")
		probe, stop := context.WithTimeout(h.ctx, 5*time.Second)
		r := h.lifecycleRead(probe, node, oldSocket, fixture.warm, http.MethodGet)
		probeErr := probe.Err()

		stop()
		require.NoError(h.t, probeErr, "old listener must reject promptly, not time out")
		require.Error(h.t, r.err, "old listener identity accepted a request after replacement")
	}

	fixture.unblock()
	h.awaitLifecycle(fixture.finished, "late origin handler did not finish")
	// With metadata still available but all data GETs denied, neither warmed old
	// pages nor the released late fill may satisfy either replacement node.
	for _, node := range nodes {
		for _, id := range []string{fixture.warm, fixture.cold} {
			before := fixture.count(id)
			probe, stop := context.WithTimeout(h.ctx, 25*time.Second)
			r := h.lifecycleRead(probe, node, lifecycleSocket, id, http.MethodGet)

			stop()
			require.NoError(h.t, r.err, "replacement must return a framed origin failure")
			require.GreaterOrEqual(h.t, r.status, 400, "old bytes leaked into replacement UID on %s", node.name)
			require.Greater(h.t, fixture.count(id), before, "replacement must try origin for old cached object")
		}
	}

	fixture.allow(true)

	for _, id := range []string{fixture.warm, fixture.cold} {
		before := fixture.count(id)
		for _, node := range nodes {
			h.lifecycleBytes(node, lifecycleSocket, id, fixture)
		}

		require.Greater(h.t, fixture.count(id), before, "replacement must acquire fresh bytes")

		after := fixture.count(id)
		for _, node := range nodes {
			h.lifecycleBytes(node, lifecycleSocket, id, fixture)
		}

		require.Equal(h.t, after, fixture.count(id), "replacement pages must be reusable")
	}

	require.Equal(h.t, beforePods, podState(), "live deletion/recreation must not rely on dataplane restart")
	h.t.Logf("cache lifecycle: %s -> %s, old listeners rejected, held fill canceled, replacement cold and reusable on both nodes", oldUID, newUID)
}

func TestLifecycleOriginHoldRelease(t *testing.T) {
	o := newLifecycleOrigin(t)
	server := httptest.NewServer(o)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v2/fixture/lifecycle/blobs/"+o.cold, nil)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)

	defer response.Body.Close()

	select {
	case <-o.started:
	case <-time.After(5 * time.Second):
		t.Fatal("origin did not start held response")
	}

	prefix := make([]byte, 1024)
	_, err = io.ReadFull(response.Body, prefix)
	require.NoError(t, err)
	require.Equal(t, o.blobs[o.cold][:1024], prefix)
	cancel()

	select {
	case <-o.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("origin did not observe cancellation")
	}

	o.unblock()
	o.unblock()

	select {
	case <-o.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("release did not finish canceled handler")
	}

	require.Equal(t, 1, o.count(o.cold))
}

func TestLifecycleOriginDeniedGETAllowsHEAD(t *testing.T) {
	o := newLifecycleOrigin(t)
	o.allow(false)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		response := httptest.NewRecorder()
		o.ServeHTTP(response, httptest.NewRequest(method, "/v2/fixture/lifecycle/blobs/"+o.warm, nil))

		if method == http.MethodGet {
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
		} else {
			require.Equal(t, http.StatusOK, response.Code)
		}
	}

	require.Equal(t, 1, o.count(o.warm))
	o.allow(true)

	response := httptest.NewRecorder()
	o.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v2/fixture/lifecycle/blobs/"+o.warm, nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, o.blobs[o.warm], response.Body.Bytes())

	missing := httptest.NewRecorder()
	o.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/missing", nil))
	require.Equal(t, http.StatusNotFound, missing.Code)
}

func TestLifecycleOriginReleaseCompletesBody(t *testing.T) {
	o := newLifecycleOrigin(t)
	server := httptest.NewServer(o)
	t.Cleanup(server.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(server.URL + "/v2/fixture/lifecycle/blobs/" + o.cold)
	require.NoError(t, err)

	defer response.Body.Close()

	o.unblock()

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, o.blobs[o.cold], body)
	require.Equal(t, int64(len(body)), response.ContentLength)

	select {
	case <-o.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("released handler did not finish")
	}
}
