// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package certs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIssueClockSkewAndRenewal(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, lifetime := range []time.Duration{time.Second, time.Hour, 365 * 24 * time.Hour} {
		t.Run(lifetime.String(), func(t *testing.T) {
			cert, key, err := issue(now, lifetime, nil, nil, nil)
			require.NoError(t, err)
			ca, signer, err := loadCA(cert, key, now.Add(-time.Minute))
			require.NoError(t, err)
			require.Equal(t, now.Add(-clockSkewAllowance), ca.NotBefore)
			require.Equal(t, now.Add(lifetime), ca.NotAfter)
			require.Equal(t, now.Add(lifetime*2/3), renewAt(ca))

			for _, leafLifetime := range []time.Duration{lifetime, 2 * lifetime} {
				leafPEM, _, err := issue(now, leafLifetime, []string{"rebound"}, ca, signer)
				require.NoError(t, err)
				leaf, err := parseCertificate(leafPEM)
				require.NoError(t, err)
				require.Equal(t, now.Add(-clockSkewAllowance), leaf.NotBefore)
				require.Equal(t, now.Add(lifetime), leaf.NotAfter)
				require.Equal(t, now.Add(lifetime*2/3), renewAt(leaf))
			}
		})
	}
}
