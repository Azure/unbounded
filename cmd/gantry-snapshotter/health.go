// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// probeKey is a snapshot key nothing ever creates. Statting it walks the whole
// serving path and ends in a not-found, which is the cheapest honest answer the
// daemon can give.
const probeKey = "gantry-snapshotter.healthz"

// healthTimeout bounds a single probe. It has to be comfortably shorter than
// the probe period so a slow answer is reported as slow rather than piling up
// behind the previous one.
const healthTimeout = 5 * time.Second

// prober asks the daemon the same question containerd asks it.
//
// The point is that it goes over the socket rather than calling the snapshotter
// in process. The ways this daemon goes bad without dying are a listener that
// stopped accepting, a gRPC server whose handlers are all blocked, and a bbolt
// transaction nobody will ever commit. None of those stop an in-process
// function from returning, and none of them stop an HTTP handler that only
// writes "ok". All three stop this.
type prober struct {
	socket    string
	namespace string

	mu     sync.Mutex
	conn   *grpc.ClientConn
	client snapshotsapi.SnapshotsClient
}

func newProber(cfg *Config) *prober {
	return &prober{socket: cfg.Socket, namespace: cfg.ContainerdNamespace}
}

// check reports whether the daemon can still answer a snapshot lookup.
//
// A not-found is the healthy answer: the key does not exist, so the request
// reached the metadata store and came back. A missing answer, any other error,
// or a timeout means something on the path containerd uses is stuck.
func (p *prober) check(ctx context.Context) error {
	client, err := p.dial()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	ctx = namespaces.WithNamespace(ctx, p.namespace)

	// A hit would mean somebody created the probe key, which is not a
	// reason to restart the daemon. Only the answer matters, not which one.
	if _, err := client.Stat(ctx, &snapshotsapi.StatSnapshotRequest{Key: probeKey}); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}

		return fmt.Errorf("stat over %s: %w", p.socket, err)
	}

	return nil
}

// dial opens the connection on first use and keeps it.
//
// grpc.NewClient is lazy and reconnects on its own, so a connection made while
// the server was still coming up recovers without the prober noticing.
func (p *prober) dial() (snapshotsapi.SnapshotsClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client != nil {
		return p.client, nil
	}

	conn, err := grpc.NewClient(
		"unix://"+p.socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", p.socket, err)
	}

	p.conn, p.client = conn, snapshotsapi.NewSnapshotsClient(conn)

	return p.client, nil
}

// close releases the probe connection.
func (p *prober) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	conn := p.conn
	p.conn, p.client = nil, nil

	if conn == nil {
		return nil
	}

	return conn.Close()
}
