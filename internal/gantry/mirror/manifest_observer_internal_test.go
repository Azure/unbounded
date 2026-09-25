// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

type blockingManifestObserver struct {
	started chan context.Context
	release chan struct{}
}

func (o *blockingManifestObserver) OnManifestServed(ctx context.Context, _, _ string, _ digest.Digest) {
	o.started <- ctx

	<-o.release
}

func TestRacerManifestObservationBoundAndDetachedContext(t *testing.T) {
	observer := &blockingManifestObserver{started: make(chan context.Context, 17), release: make(chan struct{})}
	defer close(observer.release)

	server := NewRacer(config.NewDefault(), nil, nil, WithManifestObserver(observer))
	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))
	ctx, cancel := context.WithCancel(registryauth.WithAuthorization(context.Background(), "Bearer delegated"))
	cancel()

	for range 16 {
		server.notifyManifestServed(ctx, ifaces.KindManifest, "registry.example", "pull", d)
	}

	for range 16 {
		select {
		case observed := <-observer.started:
			if observed.Err() != nil || registryauth.Authorization(observed) != "Bearer delegated" {
				t.Fatal("observer lost detached context or delegated authorization")
			}
		case <-time.After(time.Second):
			t.Fatal("manifest observation did not start")
		}
	}

	server.notifyManifestServed(ctx, ifaces.KindManifest, "registry.example", "pull", d)

	select {
	case <-observer.started:
		t.Fatal("more than 16 concurrent observations admitted")
	case <-time.After(20 * time.Millisecond):
	}

	observer.release <- struct{}{}

	deadline := time.Now().Add(time.Second)
	for len(server.racer.manifestObservations) != 15 {
		if time.Now().After(deadline) {
			t.Fatal("completed observation did not release admission")
		}

		time.Sleep(time.Millisecond)
	}

	server.notifyManifestServed(ctx, ifaces.KindManifest, "registry.example", "pull", d)

	select {
	case <-observer.started:
	case <-time.After(time.Second):
		t.Fatal("observation admission did not recover")
	}
}
