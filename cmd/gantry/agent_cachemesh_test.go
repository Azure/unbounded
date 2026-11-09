// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
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
	h.OnEvict(10)

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

func TestCgroupMemoryLimit(t *testing.T) {
	files := map[string]string{}
	read := func(path string) ([]byte, error) {
		v, ok := files[path]
		if !ok {
			return nil, errors.New("missing")
		}

		return []byte(v), nil
	}

	if got := cgroupMemoryLimit(read); got != 0 {
		t.Fatalf("no cgroup files: %d", got)
	}

	files["/sys/fs/cgroup/memory.max"] = "max\n"

	if got := cgroupMemoryLimit(read); got != 0 {
		t.Fatalf("unlimited: %d", got)
	}

	files["/sys/fs/cgroup/memory.max"] = "8589934592\n"

	if got := cgroupMemoryLimit(read); got != 8<<30 {
		t.Fatalf("v2 limit: %d", got)
	}

	delete(files, "/sys/fs/cgroup/memory.max")
	files["/sys/fs/cgroup/memory/memory.limit_in_bytes"] = "1073741824"

	if got := cgroupMemoryLimit(read); got != 1<<30 {
		t.Fatalf("v1 limit: %d", got)
	}

	// An explicit GOMEMLIMIT wins.
	env := func(k string) string {
		if k == "GOMEMLIMIT" {
			return "1GiB"
		}

		return ""
	}
	if got := setCachemeshMemoryLimit(slog.New(slog.DiscardHandler), env, read); got != 0 {
		t.Fatalf("GOMEMLIMIT set: applied %d", got)
	}
}
