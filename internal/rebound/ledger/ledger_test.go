// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func testGrant(name string, nodes ...string) *Grant {
	return &Grant{
		Namespace: "tenant", Name: name, UID: types.UID(name), Nodes: nodes, Holder: "alice",
		CreatedAt: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 10, 13, 13, 0, 0, 0, time.UTC),
		Labels:    map[string]string{"team": "test"}, Annotations: map[string]string{"note": "test"},
	}
}

func TestRoundTrip(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a", "node-b")
	g.NamespaceUID = "tenant-uid"
	require.NoError(t, l.Add(g))
	data := l.Encode()
	require.Equal(t, "tenant/first", data["node-a"])
	parsed := Parse(data)
	require.Equal(t, data, parsed.Encode())
	require.Equal(t, []*Grant{g}, parsed.Grants())
	key, ok := parsed.NodeGrant("node-b")
	require.True(t, ok)
	require.Equal(t, g.Key(), key)
	got, ok := parsed.Get(key)
	require.True(t, ok)
	require.Equal(t, g, got)
}

func TestLegacyNamespaceUID(t *testing.T) {
	l := New()
	g := testGrant("legacy", "node-a")
	require.NoError(t, l.Add(g))
	data := l.Encode()
	require.NotContains(t, data[GrantKey(g.Key())], "namespaceUID")
	parsed, ok := Parse(data).Get(g.Key())
	require.True(t, ok)
	require.Empty(t, parsed.NamespaceUID)
}

func TestParseRepairs(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a")
	require.NoError(t, l.Add(g))
	valid := l.Encode()

	for _, record := range []string{"{", "null", `{"nodes":[]}`, `{"nodes":"bad"}`, `{"nodes":["n"],"expiresAt":"bad"}`} {
		t.Run(record, func(t *testing.T) {
			require.Empty(t, Parse(map[string]string{"_grant.tenant.bad": record}).Encode())
		})
	}

	for _, key := range []string{"_grant.tenant.name.with.dots", "_grant..name", "_grant.tenant.", "_grant.tenant", "_grant.."} {
		t.Run(key, func(t *testing.T) {
			require.Empty(t, Parse(map[string]string{key: valid[GrantKey(g.Key())]}).Encode())
		})
	}

	data := l.Encode()
	data["stray"] = "tenant/missing"
	data["node-a"] = "wrong/grant"
	data["_grant.tenant.zlater"] = valid[GrantKey(g.Key())]
	require.Equal(t, valid, Parse(data).Encode())

	later := New()
	require.NoError(t, later.Add(testGrant("zlater", "free", "node-a")))
	data["_grant.tenant.zlater"] = later.Encode()["_grant.tenant.zlater"]
	require.Equal(t, valid, Parse(data).Encode())
}

func TestAddAtomic(t *testing.T) {
	l := New()
	require.NoError(t, l.Add(testGrant("first", "node-b", "node-a")))
	before := l.Encode()
	err := l.Add(testGrant("second", "free", "node-b", "node-a", "node-b"))

	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"node-a", "node-b"}, conflict.Nodes)
	require.Equal(t, before, l.Encode())
	_, ok := l.NodeGrant("free")
	require.False(t, ok)

	err = l.Add(testGrant("first", "free"))

	var exists *ExistsError
	require.ErrorAs(t, err, &exists)
	require.Equal(t, testGrant("first").Key(), exists.Key)
	require.Equal(t, before, l.Encode())
}

func TestRemove(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a", "node-b")
	require.NoError(t, l.Add(g))
	require.NoError(t, l.Add(testGrant("second", "node-c")))
	now := g.CreatedAt.Add(time.Minute)
	require.True(t, l.Remove(g.Key(), now))
	require.False(t, l.Remove(g.Key(), now))
	_, ok := l.NodeGrant("node-a")
	require.False(t, ok)
	_, ok = l.NodeGrant("node-b")
	require.False(t, ok)
	_, ok = l.NodeGrant("node-c")
	require.True(t, ok)
	require.Len(t, l.Grants(), 1)
	require.Equal(t, []*Release{{
		UID: "first", Namespace: "tenant", Nodes: []string{"node-a", "node-b"},
		CreatedAt: g.CreatedAt, ReleasedAt: now, DropAt: now.Add(ReleaseRetention),
	}}, l.Releases())
}

func TestReleaseRoundTrip(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a")
	require.NoError(t, l.Add(g))
	require.True(t, l.Remove(g.Key(), g.CreatedAt.Add(time.Minute)))

	data := l.Encode()
	require.Contains(t, data, "_release.first")
	require.NotContains(t, data, "node-a")
	require.Equal(t, data, Parse(data).Encode())
	require.Equal(t, l.Releases(), Parse(data).Releases())
	require.Equal(t, g.CreatedAt.Add(time.Minute+ReleaseRetention), Parse(data).Releases()[0].DropAt)

	// Malformed release records are dropped.
	data["_release.bad"] = "{"
	data["_release.empty"] = `{"namespace":"tenant"}`
	data["_release."] = `{"namespace":"tenant","nodes":["node-a"]}`
	parsed := Parse(data)
	require.Len(t, parsed.Releases(), 1)
	require.Equal(t, types.UID("first"), parsed.Releases()[0].UID)
}

func TestRemoveWithoutUIDRecordsNoRelease(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a")
	g.UID = ""
	require.NoError(t, l.Add(g))
	require.True(t, l.Remove(g.Key(), g.CreatedAt))
	require.Empty(t, l.Releases())
}

func TestReleaseParseLegacyRetention(t *testing.T) {
	for name, record := range map[string]string{
		"missing": `{"namespace":"tenant","nodes":["node-a"],"releasedAt":"2026-10-13T13:00:00Z"}`,
		"zero":    `{"namespace":"tenant","nodes":["node-a"],"releasedAt":"2026-10-13T13:00:00Z","dropAt":"0001-01-01T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			l := Parse(map[string]string{"_release.first": record})
			require.Len(t, l.Releases(), 1)
			r := l.Releases()[0]
			require.Equal(t, r.ReleasedAt.Add(ReleaseRetention), r.DropAt)
			_, next := l.Reap(r.ReleasedAt, nil)
			require.Equal(t, r.DropAt, next)
			_, next = l.Reap(r.DropAt, nil)
			require.True(t, next.IsZero())
			require.Empty(t, l.Releases())
		})
	}
}

func TestReapLateExpiry(t *testing.T) {
	l := New()
	g := testGrant("expired", "node-a")
	require.NoError(t, l.Add(g))
	now := g.ExpiresAt.Add(time.Hour)
	dropAt := now.Add(ReleaseRetention)

	removed, next := l.Reap(now, nil)
	require.Equal(t, []*Grant{g}, removed)
	require.Empty(t, l.Grants())
	require.Equal(t, []*Release{{
		UID: g.UID, Namespace: g.Namespace, Nodes: g.Nodes,
		CreatedAt: g.CreatedAt, ReleasedAt: g.ExpiresAt, DropAt: dropAt,
	}}, l.Releases())
	require.Equal(t, dropAt, next)

	// Persisting and reading the record must preserve its later drop deadline.
	l = Parse(l.Encode())
	removed, next = l.Reap(dropAt.Add(-time.Nanosecond), nil)
	require.Empty(t, removed)
	require.Len(t, l.Releases(), 1)
	require.Equal(t, dropAt, next)

	removed, next = l.Reap(dropAt, nil)
	require.Empty(t, removed)
	require.True(t, next.IsZero())
	require.Empty(t, l.Encode())
}

func TestReap(t *testing.T) {
	l := New()

	now := testGrant("unused").ExpiresAt
	for _, entry := range []struct {
		name string
		ttl  time.Duration
	}{{"expired", -time.Second}, {"boundary", 0}, {"rejected", time.Hour}, {"live", time.Minute}, {"later", time.Hour}} {
		g := testGrant(entry.name, entry.name)
		g.ExpiresAt = now.Add(entry.ttl)
		require.NoError(t, l.Add(g))
	}

	removed, next := l.Reap(now, func(g *Grant) bool { return g.Name != "rejected" })
	require.Len(t, removed, 3)
	require.Equal(t, []string{"boundary", "expired", "rejected"}, []string{removed[0].Name, removed[1].Name, removed[2].Name})
	require.Equal(t, now.Add(time.Minute), next)
	require.Len(t, l.Grants(), 2)
	require.Len(t, l.Releases(), 3)
	require.Equal(t, now, l.Releases()[0].ReleasedAt)
	require.Equal(t, now.Add(-time.Second), l.Releases()[1].ReleasedAt)
	require.Equal(t, now, l.Releases()[2].ReleasedAt)

	for _, r := range l.Releases() {
		require.Equal(t, now.Add(ReleaseRetention), r.DropAt)
	}

	// Releases are kept for ReleaseRetention and drive the next reap.
	removed, next = l.Reap(now.Add(time.Minute), nil)
	require.Len(t, removed, 1)
	require.Equal(t, now.Add(ReleaseRetention), next)
	require.Len(t, l.Releases(), 4)

	removed, next = l.Reap(now.Add(time.Hour), nil)
	require.Len(t, removed, 1)
	require.Equal(t, now.Add(time.Hour+ReleaseRetention), next)
	require.Len(t, l.Releases(), 1)

	removed, next = l.Reap(now.Add(time.Hour+ReleaseRetention), nil)
	require.Empty(t, removed)
	require.True(t, next.IsZero())
	require.Empty(t, l.Encode())
}

func TestSize(t *testing.T) {
	require.Zero(t, Size(nil))
	require.Zero(t, Size(map[string]string{}))
	require.Equal(t, 9, Size(map[string]string{"abc": "de", "f": "ghi"}))
	require.Equal(t, 4, Size(map[string]string{"é": "é"}))
}
