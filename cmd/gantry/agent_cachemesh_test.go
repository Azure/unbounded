// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/meshfetch"
	"github.com/Azure/unbounded/internal/gantry/metrics"
)

func TestCachemeshMemberIdentity(t *testing.T) {
	c := config.NewDefault()
	c.NodeName = "node-a"
	c.PodIP = "10.1.2.3"
	c.TransferListen = "0.0.0.0:5001"

	if got := cachemeshMemberID(c); got != "node-a" {
		t.Fatalf("member ID = %q", got)
	}

	addr, err := cachemeshAddr(c)
	if err != nil || addr != "10.1.2.3:5001" {
		t.Fatalf("addr = %q, %v", addr, err)
	}

	c.PodIP = "fd00::1"

	if addr, _ := cachemeshAddr(c); addr != "[fd00::1]:5001" {
		t.Fatalf("IPv6 addr = %q", addr)
	}

	c.TransferListen = "bad"

	if _, err := cachemeshAddr(c); err == nil {
		t.Fatal("want error for a bad transfer_listen")
	}

	c.NodeName = ""

	t.Setenv("POD_NAME", "gantry-xyz")

	if got := cachemeshMemberID(c); got != "gantry-xyz" {
		t.Fatalf("pod-name fallback = %q", got)
	}
}

type pullOnlyOrigin struct{}

func (pullOnlyOrigin) Pull(context.Context, ifaces.OriginRef) (io.ReadCloser, int64, error) {
	return nil, 0, nil
}

func (pullOnlyOrigin) Head(context.Context, ifaces.OriginRef) (int64, string, error) {
	return 0, "", nil
}

func TestNewCachemeshAgentRequiresRangeOrigin(t *testing.T) {
	c := config.NewDefault()
	c.Cachemesh.Enabled = true

	_, err := newCachemeshAgent(c, pullOnlyOrigin{}, metrics.New(), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "range") {
		t.Fatalf("want range capability error, got %v", err)
	}
}

func TestCachemeshMirrorOptionNilIsNoop(t *testing.T) {
	// A nil agent (cachemesh disabled) must not touch the mirror.
	cachemeshMirrorOption(nil)(nil)
}

func TestCachemeshMetricsHooks(t *testing.T) {
	m := newCachemeshMetrics(metrics.New())
	h := m.hooks()

	h.OnLookup("local", "hit")
	h.OnFill("origin", "ok", 100, time.Second)
	h.OnPeerResult("success", time.Millisecond)
	h.OnHedge("sent")
	h.OnServe("ok", 50)
	h.OnBlob("served", 200, time.Second)
	h.OnMeta("hit")

	if got := testutil.ToFloat64(m.fillBytes.WithLabelValues("origin")); got != 100 {
		t.Fatalf("fill bytes = %v", got)
	}

	if got := testutil.ToFloat64(m.blobBytes); got != 200 {
		t.Fatalf("blob bytes = %v", got)
	}

	if got := testutil.ToFloat64(m.lookups.WithLabelValues("local", "hit")); got != 1 {
		t.Fatalf("lookups = %v", got)
	}
}

// cachemeshContent reads the content root directly when it is readable
// and has no source otherwise.
func TestCachemeshContentSelection(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	root := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}

	blob := []byte("hello content")
	sum := sha256.Sum256(blob)
	hexd := hex.EncodeToString(sum[:])

	if err := os.WriteFile(filepath.Join(root, "blobs", "sha256", hexd), blob, 0o444); err != nil {
		t.Fatal(err)
	}

	src := cachemeshContent(root, logger)
	if src == nil {
		t.Fatal("readable root: no content source")
	}

	avail, total := src.Available("sha256:" + hexd)
	if avail != int64(len(blob)) || total != avail {
		t.Fatalf("Available = %d/%d", avail, total)
	}

	buf := make([]byte, 7)
	if _, err := src.ReadAt("sha256:"+hexd, buf, 6); err != nil || string(buf) != "content" {
		t.Fatalf("ReadAt = %q, %v", buf, err)
	}

	for _, r := range []string{"", filepath.Join(root, "missing")} {
		if src := cachemeshContent(r, logger); src != nil {
			t.Fatalf("root %q: want no content source", r)
		}
	}
}

type rangeOnlyOrigin struct{}

func (rangeOnlyOrigin) PullRange(context.Context, ifaces.OriginRef, int64) (io.ReadCloser, int64, string, error) {
	return nil, 0, "", errors.New("unused")
}

// The scrape-time metrics keep their names: dashboards and the cluster
// bench summary read them.
func TestCachemeshStateMetricNames(t *testing.T) {
	auth, err := meshfetch.NewPeerAuth([][]byte{[]byte(strings.Repeat("k", meshfetch.MinPeerKeyLen))})
	if err != nil {
		t.Fatal(err)
	}

	mesh, err := cachemesh.New(cachemesh.Config{ID: "a", Addr: "a:1", Transport: meshfetch.NewControlTransport(auth)})
	if err != nil {
		t.Fatal(err)
	}

	node, err := meshfetch.New(meshfetch.Config{Mesh: mesh, Auth: auth, Origin: rangeOnlyOrigin{}, ChunkSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}

	defer node.Close()

	reg := metrics.New()
	registerCachemeshState(reg, &cachemeshAgent{mesh: mesh, node: node})

	families, err := reg.PrometheusRegistry().Gather()
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, f := range families {
		got[f.GetName()] = true
	}

	for _, name := range []string{
		"members", "view_term", "view_seq", "leading", "ready", "neighbors", "chunk_store_chunks",
		"chunk_fills_inflight", "page_memory_bytes", "inflight_borrowed_bytes", "memory_chunks",
		"local_page_reads_total", "shared_page_reads_total", "local_errors_total", "local_fills_total",
		"holder_peeks_total", "holder_misses_total",
	} {
		if !got["gantry_cachemesh_"+name] {
			t.Errorf("missing gantry_cachemesh_%s", name)
		}
	}

	if len(got) != len(cachemeshStateMetrics) {
		t.Errorf("registered %d metrics, want %d", len(got), len(cachemeshStateMetrics))
	}
}

// Cachemesh never runs without valid peer keys.
func TestLoadPeerAuth(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}

		return p
	}
	key := strings.Repeat("a", meshfetch.MinPeerKeyLen)

	for name, path := range map[string]string{
		"unset":      "",
		"missing":    filepath.Join(dir, "missing"),
		"empty":      write("empty", "\n\n"),
		"short":      write("short", "tooshort\n"),
		"one short":  write("oneshort", key + "\ntooshort\n"),
	} {
		if _, err := loadPeerAuth(path); err == nil {
			t.Errorf("%s: want error", name)
		}
	}

	if _, err := loadPeerAuth(write("ok", key+"\n"+strings.Repeat("b", 40)+"\n")); err != nil {
		t.Fatalf("valid key file: %v", err)
	}
}
