// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh

import (
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// UnsafeKeyspace returns a keyspace with any TTL, bypassing MinTTL, so tests
// can observe expiry quickly.
func UnsafeKeyspace(name string, ttl time.Duration) Keyspace {
	return Keyspace{name: name, ttl: ttl, seed: view.Seed(name)}
}

// SetInitial makes the member start from v.
func SetInitial(c *Config, v *view.View) { c.initial = v }

// Epoch returns the member's current epoch.
func (m *Mesh) Epoch() uint64 { return m.st.Load().Env().Epoch().Current() }
