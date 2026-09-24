// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	sdk "github.com/Azure/unbounded/pkg/racer"
)

var ErrQuarantined = errors.New("racer target temporarily quarantined")

// Backend owns one SDK connection pool and a bounded target-only quarantine.
// Credentials are attached to request-local SDK views and never enter keys.
type Backend struct {
	Client     *sdk.Client
	mu         sync.Mutex
	quarantine map[string]time.Time
}

func (b *Backend) Open(ctx context.Context, ref ifaces.OriginRef) (*sdk.Object, error) {
	target, err := Target(ref)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()

	until := b.quarantine[target]
	if !time.Now().Before(until) {
		delete(b.quarantine, target)
	}
	b.mu.Unlock()

	if time.Now().Before(until) {
		return nil, ErrQuarantined
	}

	view, err := b.Client.WithOriginData([]byte(registryauth.Authorization(ctx)))
	if err != nil {
		return nil, err
	}

	return view.Open(ctx, target)
}

// Quarantine bypasses a corrupt target for one minute, bounded to 1024 targets.
func (b *Backend) Quarantine(ref ifaces.OriginRef) {
	target, err := Target(ref)
	if err != nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.quarantine == nil {
		b.quarantine = make(map[string]time.Time)
	}

	for key, until := range b.quarantine {
		if time.Now().After(until) {
			delete(b.quarantine, key)
		}
	}

	if len(b.quarantine) >= 1024 {
		var (
			oldest string
			expiry time.Time
		)

		for key, until := range b.quarantine {
			if oldest == "" || until.Before(expiry) {
				oldest, expiry = key, until
			}
		}

		delete(b.quarantine, oldest)
	}

	b.quarantine[target] = time.Now().Add(time.Minute)
}
