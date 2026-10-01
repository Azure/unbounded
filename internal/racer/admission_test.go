// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIdentityAdmission(t *testing.T) {
	a := newIdentityAdmission[string](1)
	b := newIdentityAdmission[string](1)

	require.True(t, a.acquire("one"))
	require.False(t, a.acquire("one"))
	require.False(t, a.acquire("two"))
	require.True(t, b.acquire("one"), "independent endpoint admission")
	a.release("one")
	require.True(t, a.acquire("two"))
	require.False(t, newIdentityAdmission[string](0).acquire("one"))
	require.False(t, newIdentityAdmission[string](-1).acquire("one"))
}

func TestAdmissionLimitsFrozen(t *testing.T) {
	s := &Server{Config: testConfig(t)}
	s.Config.Limits.MaxPolls = 1
	s.Config.Limits.MaxConcurrentBootstrap = 1
	s.initializeAdmission()
	s.Config.Limits.MaxPolls = 2
	s.Config.Limits.MaxConcurrentBootstrap = 2
	s.initializeAdmission()
	require.Equal(t, 1, s.polls.limit)
	require.Equal(t, 1, s.keyringPolls.limit)
	require.Equal(t, cap(s.bootstrapSlots), s.replicationPolls.limit)
}
