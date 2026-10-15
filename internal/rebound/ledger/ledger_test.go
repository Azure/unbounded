// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
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
	g.Selector = &metav1.LabelSelector{
		MatchLabels:      map[string]string{"pool": "gpu"},
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "zone", Operator: metav1.LabelSelectorOpIn, Values: []string{"a", "b"}}},
	}
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

func TestParseInformationalSelector(t *testing.T) {
	for _, selector := range []string{"", `,"selector":null`, `,"selector":"bad"`, `,"selector":{"matchLabels":42}`, `,"selector":{"matchExpressions":[{"key":"pool","operator":"Bad"}]}`} {
		t.Run(selector, func(t *testing.T) {
			l := Parse(map[string]string{"_grant.tenant.first": `{"nodes":["node-a"]` + selector + `}`})
			require.Len(t, l.Grants(), 1)
			require.Equal(t, "tenant/first", l.Encode()["node-a"])
			require.Len(t, Parse(l.Encode()).Grants(), 1)
		})
	}
}

func TestParseRepairs(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a")
	require.NoError(t, l.Add(g))
	valid := l.Encode()

	for _, record := range []string{
		"{", "null", `{"nodes":[]}`, `{"nodes":"bad"}`, `{"nodes":["n"],"expiresAt":"bad"}`,
		`{"nodes":[""]}`, `{"nodes":["node-a",""]}`, `{"nodes":["Node_A"]}`, `{"nodes":["_grant.x"]}`,
	} {
		t.Run(record, func(t *testing.T) {
			require.Empty(t, Parse(map[string]string{"_grant.tenant.bad": record}).Encode())
		})
	}

	for _, key := range []string{
		"_grant.tenant.name.with.dots", "_grant..name", "_grant.tenant.", "_grant.tenant", "_grant..",
		"_grant.Tenant.first", "_grant.tenant.First", "_grant.tenant.-first", "_grant.ten_ant.first",
	} {
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

func TestParseDropsGrantsWithInvalidReleaseKey(t *testing.T) {
	// ConfigMap keys share the DNS subdomain length limit.
	maxUID := validation.DNS1123SubdomainMaxLength - len(releaseKeyPrefix)

	valid := testGrant("valid", "node-a")
	slash := testGrant("slash", "node-b")
	slash.UID = "bad/uid"
	long := testGrant("long", "node-c")
	long.UID = types.UID(strings.Repeat("u", maxUID+1))
	edge := testGrant("edge", "node-e")
	edge.UID = types.UID(strings.Repeat("u", maxUID))
	legacy := testGrant("legacy", "node-d")
	legacy.UID = ""

	data := map[string]string{}

	for _, g := range []*Grant{valid, slash, long, edge, legacy} {
		l := New()
		require.NoError(t, l.Add(g))
		maps.Copy(data, l.Encode())
	}

	l := Parse(data)
	require.Equal(t, []*Grant{edge, legacy, valid}, l.Grants())

	// Removing every grant must leave only keys the API server accepts.
	removed, _ := l.Reap(valid.ExpiresAt, nil)
	require.Len(t, removed, 3)

	encoded := l.Encode()
	require.Len(t, encoded, 2)
	require.Contains(t, encoded, "_release.valid")
	require.Contains(t, encoded, releaseKey(edge.UID))

	for k := range encoded {
		require.Empty(t, validation.IsConfigMapKey(k), k)
	}
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

func TestRemoveExpiredEndsAtExpiry(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a")
	require.NoError(t, l.Add(g))
	now := g.ExpiresAt.Add(5 * time.Minute)
	require.True(t, l.Remove(g.Key(), now))
	require.Equal(t, []*Release{{
		UID: "first", Namespace: "tenant", Nodes: []string{"node-a"},
		CreatedAt: g.CreatedAt, ReleasedAt: g.ExpiresAt, ConfirmedAt: g.ExpiresAt, DropAt: now.Add(ReleaseRetention),
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
	data["_release.badnode"] = `{"namespace":"tenant","nodes":[""]}`
	data["_release.badns"] = `{"namespace":"Tenant","nodes":["node-a"]}`
	data["_release.bad/uid"] = `{"namespace":"tenant","nodes":["node-a"]}`
	data["_release."+strings.Repeat("u", validation.DNS1123SubdomainMaxLength)] = `{"namespace":"tenant","nodes":["node-a"]}`
	parsed := Parse(data)
	require.Len(t, parsed.Releases(), 1)
	require.Equal(t, types.UID("first"), parsed.Releases()[0].UID)
}

func TestReleaseNamespaceUID(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a")
	g.NamespaceUID = "tenant-uid"
	require.NoError(t, l.Add(g))
	require.True(t, l.Remove(g.Key(), g.CreatedAt.Add(time.Minute)))
	require.Equal(t, types.UID("tenant-uid"), l.Releases()[0].NamespaceUID)

	data := l.Encode()
	require.Contains(t, data["_release.first"], `"namespaceUID":"tenant-uid"`)
	require.Equal(t, data, Parse(data).Encode())
	require.Equal(t, l.Releases(), Parse(data).Releases())

	// Releases of grants without a namespace UID, and older records, omit it.
	legacy := New()
	g = testGrant("legacy", "node-a")
	require.NoError(t, legacy.Add(g))
	require.True(t, legacy.Remove(g.Key(), g.CreatedAt.Add(time.Minute)))
	data = legacy.Encode()
	require.NotContains(t, data["_release.legacy"], "namespaceUID")
	require.Empty(t, Parse(data).Releases()[0].NamespaceUID)
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
			// Older records are unconfirmed and kept until confirmed.
			_, next := l.Reap(r.DropAt, nil)
			require.True(t, next.IsZero())
			require.Len(t, l.Releases(), 1)
			l.Confirm(r.ReleasedAt)
			_, next = l.Reap(r.ReleasedAt, nil)
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
		CreatedAt: g.CreatedAt, ReleasedAt: g.ExpiresAt, ConfirmedAt: g.ExpiresAt, DropAt: dropAt,
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

	// The live removal of "rejected" is unconfirmed; confirm it as the
	// reaper would after the removal commits.
	require.True(t, l.Releases()[2].ConfirmedAt.IsZero())
	require.True(t, l.Confirm(now))

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

func TestCheckCooldown(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a", "node-b")
	require.NoError(t, l.Add(g))

	// Removed before expiry: the cooldown counts from the confirmation,
	// here at the same time as the removal.
	removed := g.CreatedAt.Add(time.Minute)
	require.True(t, l.Remove(g.Key(), removed))
	require.True(t, l.Confirm(removed))

	require.NoError(t, l.CheckCooldown([]string{"node-c"}, removed))

	err := l.CheckCooldown([]string{"node-c", "node-b", "node-a"}, removed.Add(time.Second))

	var cooldown *CooldownError
	require.ErrorAs(t, err, &cooldown)
	require.Equal(t, []string{"node-a", "node-b"}, cooldown.Nodes)
	require.Equal(t, HandoffCooldown-time.Second, cooldown.RetryAfter)
	require.False(t, cooldown.Unconfirmed)

	require.NoError(t, l.CheckCooldown([]string{"node-a"}, removed.Add(HandoffCooldown)))

	// Release records outlive the cooldown, so pruning cannot end it early.
	_, next := l.Reap(removed.Add(HandoffCooldown-time.Nanosecond), nil)
	require.Len(t, l.Releases(), 1)
	require.Equal(t, removed.Add(ReleaseRetention), next)
}

func TestCheckCooldownExpiredGrant(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a")
	require.NoError(t, l.Add(g))

	// A grant reaped late ends at its expiry, so the cooldown does too.
	reaped := g.ExpiresAt.Add(HandoffCooldown / 2)
	l.Reap(reaped, nil)
	require.Error(t, l.CheckCooldown([]string{"node-a"}, reaped))
	require.NoError(t, l.CheckCooldown([]string{"node-a"}, g.ExpiresAt.Add(HandoffCooldown)))

	l = New()
	require.NoError(t, l.Add(testGrant("second", "node-a")))
	l.Reap(g.ExpiresAt.Add(time.Hour), nil)
	require.NoError(t, l.CheckCooldown([]string{"node-a"}, g.ExpiresAt.Add(time.Hour)))
}

func TestCheckCooldownLatestRelease(t *testing.T) {
	end := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	// Older records without dropAt still fence their nodes.
	l := Parse(map[string]string{
		"_release.old": `{"namespace":"tenant","nodes":["node-a"],"createdAt":"2026-10-13T11:00:00Z","releasedAt":"2026-10-13T11:59:58Z"}`,
		"_release.new": `{"namespace":"tenant","nodes":["node-a"],"createdAt":"2026-10-13T11:00:00Z","releasedAt":"2026-10-13T12:00:00Z","dropAt":"2026-10-13T12:10:00Z"}`,
	})
	require.Len(t, l.Releases(), 2)

	err := l.CheckCooldown([]string{"node-a"}, end)

	var cooldown *CooldownError
	require.ErrorAs(t, err, &cooldown)
	// Neither record is confirmed, so both fence the node for a full
	// cooldown from now.
	require.Equal(t, HandoffCooldown, cooldown.RetryAfter)
	require.True(t, cooldown.Unconfirmed)
	require.Contains(t, err.Error(), "retry in 10s")
}

// TestCheckCooldownUnconfirmed checks that a live removal fences its nodes
// until it is confirmed, however long ago it was stamped, and then for a full
// HandoffCooldown from the confirmation.
func TestCheckCooldownUnconfirmed(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a")
	require.NoError(t, l.Add(g))

	removed := g.CreatedAt.Add(time.Minute)
	require.True(t, l.Remove(g.Key(), removed))
	require.True(t, l.Releases()[0].ConfirmedAt.IsZero())
	require.True(t, l.Unconfirmed())

	// The removal write was slow: it is still unconfirmed well after the
	// cooldown from ReleasedAt would have ended.
	late := removed.Add(3 * HandoffCooldown)

	var cooldown *CooldownError
	require.ErrorAs(t, l.CheckCooldown([]string{"node-a"}, late), &cooldown)
	require.True(t, cooldown.Unconfirmed)
	require.Equal(t, HandoffCooldown, cooldown.RetryAfter)
	require.NoError(t, l.CheckCooldown([]string{"node-b"}, late))

	require.True(t, l.Confirm(late))
	require.False(t, l.Unconfirmed())
	require.Equal(t, late, l.Releases()[0].ConfirmedAt)
	require.Equal(t, removed, l.Releases()[0].ReleasedAt)

	// Confirming again keeps the first confirmation.
	require.False(t, l.Confirm(late.Add(time.Hour)))
	require.Equal(t, late, l.Releases()[0].ConfirmedAt)

	require.ErrorAs(t, l.CheckCooldown([]string{"node-a"}, late.Add(HandoffCooldown-time.Nanosecond)), &cooldown)
	require.False(t, cooldown.Unconfirmed)
	require.Equal(t, time.Nanosecond, cooldown.RetryAfter)
	require.NoError(t, l.CheckCooldown([]string{"node-a"}, late.Add(HandoffCooldown)))
}

// TestReapConfirmation checks that Reap confirms releases of expired grants
// at their expiry and leaves live removals unconfirmed.
func TestReapConfirmation(t *testing.T) {
	l := New()
	expired := testGrant("expired", "node-a")
	live := testGrant("live", "node-b")
	live.ExpiresAt = expired.ExpiresAt.Add(time.Hour)

	require.NoError(t, l.Add(expired))
	require.NoError(t, l.Add(live))

	now := expired.ExpiresAt.Add(time.Minute)
	removed, _ := l.Reap(now, func(*Grant) bool { return false })
	require.Len(t, removed, 2)

	byUID := map[types.UID]*Release{}
	for _, r := range l.Releases() {
		byUID[r.UID] = r
	}

	require.Equal(t, expired.ExpiresAt, byUID[expired.UID].ConfirmedAt)
	require.True(t, byUID[live.UID].ConfirmedAt.IsZero())

	// The expired grant needs no wait beyond its expiry plus the cooldown.
	require.NoError(t, l.CheckCooldown([]string{"node-a"}, expired.ExpiresAt.Add(HandoffCooldown)))
	require.Error(t, l.CheckCooldown([]string{"node-b"}, now.Add(time.Hour)))
}

func TestConfirmedAtRoundTrip(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a")
	require.NoError(t, l.Add(g))
	require.True(t, l.Remove(g.Key(), g.CreatedAt))

	data := l.Encode()
	require.NotContains(t, data[releaseKey(g.UID)], "confirmedAt")
	require.True(t, Parse(data).Releases()[0].ConfirmedAt.IsZero())

	confirmed := g.CreatedAt.Add(time.Second)
	require.True(t, l.Confirm(confirmed))
	require.Equal(t, confirmed, Parse(l.Encode()).Releases()[0].ConfirmedAt.UTC())
}

// TestReapKeepsReleasesUntilCooldownEnds checks that Reap drops a release
// no earlier than the end of its handoff cooldown, and never while it is
// unconfirmed, so dropping a record cannot end the fence early.
func TestReapKeepsReleasesUntilCooldownEnds(t *testing.T) {
	l := New()
	g := testGrant("first", "node-a")
	require.NoError(t, l.Add(g))

	removed := g.CreatedAt
	require.True(t, l.Remove(g.Key(), removed))

	dropAt := removed.Add(ReleaseRetention)
	_, next := l.Reap(dropAt.Add(time.Hour), nil)
	require.True(t, next.IsZero())
	require.Len(t, l.Releases(), 1)

	// Confirmed just before DropAt: kept until the cooldown ends.
	confirmed := dropAt.Add(-time.Second)
	require.True(t, l.Confirm(confirmed))

	_, next = l.Reap(dropAt, nil)
	require.Len(t, l.Releases(), 1)
	require.Equal(t, confirmed.Add(HandoffCooldown), next)
	require.Error(t, l.CheckCooldown([]string{"node-a"}, dropAt))

	_, next = l.Reap(confirmed.Add(HandoffCooldown), nil)
	require.True(t, next.IsZero())
	require.Empty(t, l.Releases())
}
