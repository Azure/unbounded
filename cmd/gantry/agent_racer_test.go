// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/coord"
	"github.com/Azure/unbounded/internal/gantry/discovery"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racer"
)

func TestRacerOriginStartupReadinessAndCollision(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace socket via proc fd")
	}

	dir, err := os.MkdirTemp(".", ".racer-origin-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	folder, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer folder.Close()

	socket := fmt.Sprintf("/proc/self/fd/%d/origin", folder.Fd())
	if racerSocketReady(t.Context(), socket) {
		t.Fatal("missing socket ready")
	}

	handler, err := sdk.NewRangeOrigin(&gantryracer.Origin{})
	if err != nil {
		t.Fatal(err)
	}

	server, _, err := startRacerOrigin(socket, handler)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	if !racerSocketReady(t.Context(), socket) {
		t.Fatal("running origin not ready")
	}

	if _, _, err := startRacerOrigin(socket, handler); err == nil {
		t.Fatal("replaced live origin")
	}

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	if racerSocketReady(t.Context(), socket) {
		t.Fatal("closed origin ready")
	}

	bad, _, err := startRacerOrigin(socket, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()

	if racerSocketReady(t.Context(), socket) {
		t.Fatal("unavailable cache ready")
	}
}

func TestRacerRejectsDirectCoordProtocol(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Libp2pIdentityPath = ""
	cfg.Libp2pListen = []string{"/ip4/127.0.0.1/tcp/0"}

	opts := racerDiscoveryOptions(cfg)
	if opts.ProtocolPrefix != "/gantry/racer" || opts.SelfTestPeriod != 0 {
		t.Fatal("direct discovery enabled")
	}

	racer, err := discovery.New(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer racer.Close()

	directOpts := discovery.FromConfig(cfg)
	directOpts.SelfTestPeriod = 0

	direct, err := discovery.New(t.Context(), directOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()

	direct.LibP2P().Peerstore().AddAddrs(racer.PeerID(), racer.Addrs(), time.Minute)

	stream, err := direct.LibP2P().NewStream(t.Context(), racer.PeerID(), coord.ProtocolID)
	if err == nil {
		_ = stream.Close()

		t.Fatal("Racer accepted incompatible direct coord")
	}
}
