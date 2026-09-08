// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package discovery

import (
	"context"
	"testing"
	"time"

	basicconnmgr "github.com/libp2p/go-libp2p/p2p/net/connmgr"

	"github.com/Azure/unbounded/internal/gantry/config"
)

func TestBuildResourceManagersAppliesConfiguredWatermarks(t *testing.T) {
	cm, rm, err := buildResourceManagers(Options{
		ConnManagerLow:   1000,
		ConnManagerHigh:  2000,
		ConnManagerGrace: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildResourceManagers: %v", err)
	}

	defer rm.Close() //nolint:errcheck // test cleanup

	basic, ok := cm.(*basicconnmgr.BasicConnMgr)
	if !ok {
		t.Fatalf("conn manager is %T, want *connmgr.BasicConnMgr", cm)
	}

	defer basic.Close() //nolint:errcheck // test cleanup

	info := basic.GetInfo()
	if info.LowWater != 1000 || info.HighWater != 2000 {
		t.Fatalf("watermarks = %d/%d, want 1000/2000", info.LowWater, info.HighWater)
	}

	if info.GracePeriod != 15*time.Second {
		t.Fatalf("grace = %v, want 15s", info.GracePeriod)
	}
}

// The go-libp2p defaults are 160/192, which trims connections a cluster-wide
// chair cohort is about to reuse. Zero-valued options must land on Gantry's
// larger defaults rather than falling through to libp2p's.
func TestBuildResourceManagersDefaultsExceedLibp2pDefaults(t *testing.T) {
	cm, rm, err := buildResourceManagers(Options{})
	if err != nil {
		t.Fatalf("buildResourceManagers: %v", err)
	}

	defer rm.Close() //nolint:errcheck // test cleanup

	basic, ok := cm.(*basicconnmgr.BasicConnMgr)
	if !ok {
		t.Fatalf("conn manager is %T, want *connmgr.BasicConnMgr", cm)
	}

	defer basic.Close() //nolint:errcheck // test cleanup

	info := basic.GetInfo()
	if info.HighWater != DefaultConnManagerHigh {
		t.Fatalf("high water = %d, want %d", info.HighWater, DefaultConnManagerHigh)
	}

	if info.LowWater != DefaultConnManagerLow {
		t.Fatalf("low water = %d, want %d", info.LowWater, DefaultConnManagerLow)
	}

	const libp2pDefaultHighWater = 192
	if info.HighWater <= libp2pDefaultHighWater {
		t.Fatalf("high water %d does not exceed the libp2p default %d", info.HighWater, libp2pDefaultHighWater)
	}

	if info.GracePeriod != DefaultConnManagerGrace {
		t.Fatalf("grace = %v, want %v", info.GracePeriod, DefaultConnManagerGrace)
	}
}

func TestBuildResourceManagersRejectsInvertedWatermarks(t *testing.T) {
	cm, rm, err := buildResourceManagers(Options{ConnManagerLow: 500, ConnManagerHigh: 400})
	if err == nil {
		if rm != nil {
			_ = rm.Close() //nolint:errcheck // test cleanup
		}

		if closer, ok := cm.(*basicconnmgr.BasicConnMgr); ok {
			_ = closer.Close() //nolint:errcheck // test cleanup
		}

		t.Fatal("expected an error when low water is not below high water")
	}
}

func TestFromConfigCarriesConnManagerSettings(t *testing.T) {
	c := config.NewDefault()
	c.Libp2pConnManagerHigh = 4096
	c.Libp2pConnManagerLow = 2048
	c.Libp2pConnManagerGrace = 42 * time.Second

	opts := FromConfig(c)
	if opts.ConnManagerHigh != 4096 || opts.ConnManagerLow != 2048 {
		t.Fatalf("watermarks = %d/%d, want 2048/4096", opts.ConnManagerLow, opts.ConnManagerHigh)
	}

	if opts.ConnManagerGrace != 42*time.Second {
		t.Fatalf("grace = %v, want 42s", opts.ConnManagerGrace)
	}
}

// Guards the wiring rather than the builder: dropping the ConnectionManager
// option from libp2p.New would silently restore the 160/192 defaults.
func TestNewHostUsesConfiguredConnManager(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, err := New(ctx, Options{
		ListenAddrs:     []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix:  "/gantry",
		ConnManagerLow:  1234,
		ConnManagerHigh: 5678,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = h.Close() }) //nolint:errcheck // best-effort close

	basic, ok := h.h.ConnManager().(*basicconnmgr.BasicConnMgr)
	if !ok {
		t.Fatalf("host conn manager is %T, want *connmgr.BasicConnMgr", h.h.ConnManager())
	}

	info := basic.GetInfo()
	if info.LowWater != 1234 || info.HighWater != 5678 {
		t.Fatalf("host watermarks = %d/%d, want 1234/5678", info.LowWater, info.HighWater)
	}

	if got := h.ConnCount(); got != 0 {
		t.Fatalf("ConnCount() = %d on a fresh host, want 0", got)
	}
}
