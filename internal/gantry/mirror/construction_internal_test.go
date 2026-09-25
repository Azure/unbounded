// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
)

func TestBackendConstructionDependencies(t *testing.T) {
	cfg := config.NewDefault()
	store := fakes.NewCache()

	direct := New(cfg, store, nil)
	if direct.store != store || direct.staleProviders == nil || direct.suspiciousProviders == nil || direct.unavailableProviders == nil {
		t.Fatal("direct backend is missing content or peer state")
	}

	if direct.staleProviderTTL != 3*time.Minute || direct.unavailablePeerTTL != 30*time.Second || direct.suspiciousPeerTTL != 5*time.Minute {
		t.Fatal("direct provider failure defaults changed")
	}

	overridden := New(cfg, store, nil, WithProviderFailureCacheTTL(time.Second, 2*time.Second, 3*time.Second))
	if overridden.staleProviderTTL != time.Second || overridden.unavailablePeerTTL != 2*time.Second || overridden.suspiciousPeerTTL != 3*time.Second {
		t.Fatal("direct provider failure options were overwritten")
	}

	racer := NewRacer(cfg, nil, nil)
	if racer.racer == nil || racer.store != nil || racer.origin != nil {
		t.Fatal("Racer must have no direct content dependencies, even without a backend")
	}

	if racer.staleProviders != nil || racer.suspiciousProviders != nil || racer.unavailableProviders != nil || racer.staleProviderTTL != 0 || racer.unavailablePeerTTL != 0 || racer.suspiciousPeerTTL != 0 {
		t.Fatal("Racer initialized direct-only peer state")
	}
}
