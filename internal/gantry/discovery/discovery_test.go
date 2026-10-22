// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestDigestToCID_Deterministic(t *testing.T) {
	d := digest.MustParse("sha256:" + zeros(64))

	c1, err := DigestToCID(d)
	if err != nil {
		t.Fatalf("DigestToCID: %v", err)
	}

	c2, err := DigestToCID(d)
	if err != nil {
		t.Fatalf("DigestToCID (2nd): %v", err)
	}

	if !c1.Equals(c2) {
		t.Errorf("CIDs differ across calls: %s vs %s", c1, c2)
	}

	if c1.Version() != 1 {
		t.Errorf("CID version = %d, want 1", c1.Version())
	}
	// Two distinct digests must produce two distinct CIDs.
	d2 := digest.MustParse("sha256:" + ones(64))

	c3, err := DigestToCID(d2)
	if err != nil {
		t.Fatalf("DigestToCID (d2): %v", err)
	}

	if c1.Equals(c3) {
		t.Error("CIDs equal across different digests")
	}
}

func TestMergePeerAddrInfoCombinesAddressesByPeer(t *testing.T) {
	peerID := peer.ID("peer-a")
	tcp := multiaddr.StringCast("/ip4/10.0.0.1/tcp/4001")
	quic := multiaddr.StringCast("/ip4/10.0.0.1/udp/4001/quic-v1")
	pool := []peer.AddrInfo{}
	positions := map[peer.ID]int{}

	mergePeerAddrInfo(&pool, positions, peer.AddrInfo{ID: peerID, Addrs: []multiaddr.Multiaddr{tcp}})
	mergePeerAddrInfo(&pool, positions, peer.AddrInfo{ID: peerID, Addrs: []multiaddr.Multiaddr{tcp, quic}})

	if len(pool) != 1 {
		t.Fatalf("peer count = %d, want 1", len(pool))
	}

	if len(pool[0].Addrs) != 2 {
		t.Fatalf("address count = %d, want 2", len(pool[0].Addrs))
	}
}

func TestHostBringUpEphemeral(t *testing.T) {
	// Smoke test: New with an ephemeral identity returns a usable host;
	// Provide on a fresh DHT errors because there are no peers yet, but
	// the host itself must boot cleanly and Close cleanly.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, err := New(ctx, Options{
		IdentityPath:   "",
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		BootstrapPeers: nil,
		ProtocolPrefix: "/gantry",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = h.Close() }) //nolint:errcheck // best-effort close

	if h.PeerID() == "" {
		t.Error("PeerID empty")
	}

	if len(h.Addrs()) == 0 {
		t.Error("no listen addrs")
	}

	if got, want := h.Health(), 1.0; got != want {
		t.Errorf("Health() = %v, want %v (no monitor wired in test mode)", got, want)
	}
}

func TestHostPersistsIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "libp2p.key")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := New(ctx, Options{
		IdentityPath:   path,
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-test",
	})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}

	id1 := first.PeerID()
	_ = first.Close() //nolint:errcheck // best-effort close

	second, err := New(ctx, Options{
		IdentityPath:   path,
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-test",
	})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}

	defer func() { _ = second.Close() }() //nolint:errcheck // best-effort close

	if second.PeerID() != id1 {
		t.Errorf("PeerID changed across restarts: %s vs %s", id1, second.PeerID())
	}
}

func newConnectedProviderPair(t *testing.T, ctx context.Context, opts Options) (*Host, *Host) {
	t.Helper()

	opts.ListenAddrs = []string{"/ip4/127.0.0.1/tcp/0"}

	first, err := New(ctx, opts)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}

	t.Cleanup(func() { _ = first.Close() })

	p2p, err := multiaddr.NewMultiaddr("/p2p/" + first.PeerID().String())
	if err != nil {
		t.Fatalf("peer multiaddr: %v", err)
	}

	bootstrap := first.Addrs()[0].Encapsulate(p2p).String()

	opts.BootstrapPeers = []string{bootstrap}

	second, err := New(ctx, opts)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}

	t.Cleanup(func() { _ = second.Close() })

	convergenceDeadline := time.NewTimer(5 * time.Second)
	convergenceTick := time.NewTicker(10 * time.Millisecond)

	defer convergenceDeadline.Stop()
	defer convergenceTick.Stop()

	for second.RoutingTableSize() == 0 {
		select {
		case <-convergenceDeadline.C:
			t.Fatal("second DHT routing table did not converge")
		case <-convergenceTick.C:
		}
	}

	for {
		snapshot, err := second.provider.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}

		if snapshot.Schedule.AvgPrefixLength >= 0 {
			break
		}

		select {
		case <-convergenceDeadline.C:
			t.Fatal("sweeping provider did not come online")
		case <-convergenceTick.C:
		}
	}

	return first, second
}

func TestHostProvideRegistersSweeperAndWithdrawRemoves(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	first, second := newConnectedProviderPair(t, ctx, Options{ProtocolPrefix: "/gantry-provider-lifecycle-test"})

	d := digest.MustParse("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	if err := second.Provide(ctx, d); err != nil {
		t.Fatalf("Provide: %v", err)
	}

	stats, err := second.provider.Stats(ctx)
	if err != nil {
		t.Fatalf("provider Stats after Provide: %v", err)
	}

	if stats.Schedule.Keys != 1 {
		t.Fatalf("scheduled keys after Provide = %d, want 1", stats.Schedule.Keys)
	}

	if stats.Schedule.Regions == 0 {
		t.Fatal("registered key has no periodic reprovide schedule")
	}

	c, err := DigestToCID(d)
	if err != nil {
		t.Fatal(err)
	}

	providers, err := first.d.FindProviders(ctx, c)
	if err != nil {
		t.Fatal(err)
	}

	found := false

	for _, provider := range providers {
		if provider.ID == second.PeerID() {
			found = true
		}
	}

	if !found {
		t.Fatal("Provide returned before the digest was discoverable")
	}

	observeUntil := time.NewTimer(250 * time.Millisecond)
	observeTick := time.NewTicker(5 * time.Millisecond)

	defer observeUntil.Stop()
	defer observeTick.Stop()

observe:
	for {
		stats, err = second.provider.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}

		if stats.Queues.PendingKeyProvides != 0 ||
			stats.Operations.Ongoing.KeyProvides != 0 ||
			stats.Operations.Past.KeysProvided != 0 ||
			stats.Operations.Past.KeysFailed != 0 {
			t.Fatal("sweeper queued or repeated the synchronous initial publication")
		}

		select {
		case <-observeUntil.C:
			break observe
		case <-observeTick.C:
		}
	}

	if err := second.Withdraw(ctx, d); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	stats, err = second.provider.Stats(ctx)
	if err != nil {
		t.Fatalf("provider Stats after Withdraw: %v", err)
	}

	if stats.Schedule.Keys != 0 {
		t.Fatalf("scheduled keys after Withdraw = %d, want 0", stats.Schedule.Keys)
	}
}

func TestHostReprovideRefreshesAndWithdrawExpiresRemoteRecord(t *testing.T) {
	const (
		validity = 2 * time.Second
		refresh  = 200 * time.Millisecond
	)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	observer, publisher := newConnectedProviderPair(t, ctx, Options{
		ProtocolPrefix:    "/gantry-provider-expiration-test",
		ProviderValidity:  validity,
		ReprovideInterval: refresh,
		MaxReprovideDelay: 50 * time.Millisecond,
	})
	d := digest.MustParse("sha256:dededededededededededededededededededededededededededededededede")

	key, err := DigestToCID(d)
	if err != nil {
		t.Fatal(err)
	}

	remoteHasProvider := func() bool {
		t.Helper()
		// A network lookup could rediscover and recache the provider,
		// obscuring whether periodic publication or expiration worked.
		providers, err := observer.d.ProviderStore().GetProviders(ctx, key.Hash())
		if err != nil {
			t.Fatal(err)
		}

		for _, provider := range providers {
			if provider.ID == publisher.PeerID() {
				return true
			}
		}

		return false
	}

	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()

	waitForRemote := func(present bool, limit time.Duration) {
		t.Helper()

		deadline := time.NewTimer(limit)
		defer deadline.Stop()

		for remoteHasProvider() != present {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-deadline.C:
				t.Fatalf("remote provider presence did not become %v", present)
			case <-tick.C:
			}
		}
	}

	if err := publisher.Provide(ctx, d); err != nil {
		t.Fatal(err)
	}

	waitForRemote(true, validity)

	observe := func(present bool, duration time.Duration) {
		t.Helper()

		deadline := time.NewTimer(duration)
		defer deadline.Stop()

		for {
			if remoteHasProvider() != present {
				t.Fatalf("remote provider presence changed while expecting %v", present)
			}

			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-deadline.C:
				return
			case <-tick.C:
			}
		}
	}
	observe(true, 3*validity)

	if err := publisher.Withdraw(ctx, d); err != nil {
		t.Fatal(err)
	}

	waitForRemote(false, 3*validity)
	observe(false, 2*refresh)
}

func TestTransferAddrWithPortSkipsLoopback(t *testing.T) {
	tests := []struct {
		name  string
		addrs []string
		want  []string
	}{
		{
			name:  "IPv4 pod address after loopback",
			addrs: []string{"/ip4/127.0.0.1/tcp/4001", "/ip4/10.245.17.155/tcp/4001"},
			want:  []string{"10.245.17.155:5001"},
		},
		{
			name:  "IPv6 pod address after loopback",
			addrs: []string{"/ip6/::1/tcp/4001", "/ip6/fd00::42/tcp/4001"},
			want:  []string{"[fd00::42]:5001"},
		},
		{
			name:  "loopback only is unusable",
			addrs: []string{"/ip4/127.0.0.1/tcp/4001", "/ip6/::1/tcp/4001"},
		},
		{
			name: "old and current pod IPs remain separate",
			addrs: []string{
				"/ip4/10.65.56.20/tcp/4001",
				"/ip4/10.65.56.191/tcp/4001",
			},
			want: []string{"10.65.56.20:5001", "10.65.56.191:5001"},
		},
		{
			name: "deduplicate transports and preserve IP order",
			addrs: []string{
				"/ip4/10.65.56.20/tcp/4001",
				"/ip4/10.65.56.20/udp/4001/quic-v1",
				"/ip6/fd00::42/tcp/4001",
				"/ip4/10.65.56.191/tcp/4002",
				"/ip6/fd00::42/udp/4001/quic-v1",
			},
			want: []string{"10.65.56.20:5001", "[fd00::42]:5001", "10.65.56.191:5001"},
		},
		{
			name: "unusable addresses do not hide later IPs",
			addrs: []string{
				"/ip4/0.0.0.0/tcp/4001",
				"/ip6/::/tcp/4001",
				"/ip4/169.254.1.1/tcp/4001",
				"/ip6/fe80::1/tcp/4001",
				"/dns4/provider.example/tcp/4001",
				"/ip4/10.65.56.191/tcp/4001",
			},
			want: []string{"10.65.56.191:5001"},
		},
		{name: "no addresses"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ai := peer.AddrInfo{Addrs: make([]multiaddr.Multiaddr, 0, len(tt.addrs))}
			for _, addr := range tt.addrs {
				ai.Addrs = append(ai.Addrs, multiaddr.StringCast(addr))
			}

			if got := transferAddrsWithPort(ai, 5001); !slices.Equal(got, tt.want) {
				t.Fatalf("transferAddrsWithPort() = %q, want %q", got, tt.want)
			}
		})
	}
}

func zeros(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}

	return string(b)
}

func ones(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '1'
	}

	return string(b)
}

func TestHostStreamProvidersClosesWhenLookupBudgetEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	host, err := New(ctx, Options{
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-stream-providers-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = host.Close() })

	d := digest.MustParse("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	providers, lookupErr := host.StreamProviders(ctx, d, 200*time.Millisecond)

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()

	for {
		select {
		case provider, ok := <-providers:
			if !ok {
				if err := lookupErr(); err != nil {
					t.Fatalf("lookup error = %v, want nil", err)
				}

				return
			}

			t.Fatalf("provider = %+v, want none from an isolated host", provider)
		case <-deadline.C:
			t.Fatal("provider stream did not close after its lookup budget")
		}
	}
}

func TestHostStreamProvidersReportsInvalidDigest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	host, err := New(ctx, Options{
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-stream-providers-invalid-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = host.Close() })

	providers, lookupErr := host.StreamProviders(ctx, digest.Digest{}, time.Second)
	for provider := range providers {
		t.Fatalf("provider = %+v, want none for an invalid digest", provider)
	}

	if lookupErr() == nil {
		t.Fatal("lookup error = nil, want unsupported digest error")
	}
}

func TestHostStreamProvidersReportsLookupDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := New(ctx, Options{
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-stream-deadline-test",
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = h.Close() })

	d := digest.MustParse("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")

	providers, lookupErr := h.StreamProviders(ctx, d, time.Nanosecond)
	for range providers {
	}

	if err := lookupErr(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lookup error = %v, want deadline exceeded", err)
	}
}

func TestHostStreamProvidersQueriesPastFullLocalProviderStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const prefix = "/gantry-full-local-provider-test"

	first, err := New(ctx, Options{
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: prefix,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = first.Close() })

	second, err := New(ctx, Options{
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		BootstrapPeers: []string{first.Addrs()[0].String() + "/p2p/" + first.PeerID().String()},
		ProtocolPrefix: prefix,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = second.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for first.RoutingTableSize() == 0 || second.RoutingTableSize() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("DHT peers did not connect")
		}

		time.Sleep(10 * time.Millisecond)
	}

	d := digest.MustParse("sha256:edededededededededededededededededededededededededededededededed")

	key, err := DigestToCID(d)
	if err != nil {
		t.Fatal(err)
	}

	newProvider := func(ip string) peer.AddrInfo {
		t.Helper()

		private, _, err := crypto.GenerateEd25519Key(nil)
		if err != nil {
			t.Fatal(err)
		}

		id, err := peer.IDFromPrivateKey(private)
		if err != nil {
			t.Fatal(err)
		}

		return peer.AddrInfo{ID: id, Addrs: []multiaddr.Multiaddr{
			multiaddr.StringCast("/ip4/" + ip + "/tcp/4001"),
		}}
	}
	for i := range 21 {
		provider := newProvider(fmt.Sprintf("10.200.0.%d", i+1))
		if err := first.d.ProviderStore().AddProvider(ctx, key.Hash(), provider); err != nil {
			t.Fatal(err)
		}
	}

	fresh := newProvider("10.200.0.250")
	if err := second.d.ProviderStore().AddProvider(ctx, key.Hash(), fresh); err != nil {
		t.Fatal(err)
	}

	capped, err := first.FindProviders(ctx, d)
	if err != nil {
		t.Fatal(err)
	}

	if len(capped) != 20 {
		t.Fatalf("capped lookup returned %d providers, want 20 local records", len(capped))
	}

	for _, provider := range capped {
		if string(provider.NodeID) == fresh.ID.String() {
			t.Fatal("capped lookup unexpectedly queried the remote provider store")
		}
	}

	providers, lookupErr := first.StreamProviders(ctx, d, 3*time.Second)
	foundFresh := false

	count := 0
	for provider := range providers {
		count++

		if string(provider.NodeID) == fresh.ID.String() {
			foundFresh = true
		}
	}

	if err := lookupErr(); err != nil {
		t.Fatalf("uncapped lookup: %v", err)
	}

	if !foundFresh || count < 22 {
		t.Fatalf("uncapped lookup returned %d providers, found remote=%v; want all 21 local plus remote", count, foundFresh)
	}
}

func TestHostProviderLookupsPreserveAllTransferAddresses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	host, err := New(ctx, Options{
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-provider-address-test",
		TransferPort:   6501,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = host.Close() })

	private, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatal(err)
	}

	id, err := peer.IDFromPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}

	info := peer.AddrInfo{ID: id, Addrs: []multiaddr.Multiaddr{
		multiaddr.StringCast("/ip4/127.0.0.1/tcp/4001"),
		multiaddr.StringCast("/ip4/10.65.56.20/tcp/4001"),
		multiaddr.StringCast("/ip4/10.65.56.20/udp/4001/quic-v1"),
		multiaddr.StringCast("/ip4/10.65.56.191/tcp/4001"),
		multiaddr.StringCast("/ip6/fd00::42/tcp/4001"),
	}}
	want := []ifaces.Provider{
		{NodeID: ifaces.NodeID(id.String()), Addr: "10.65.56.20:6501"},
		{NodeID: ifaces.NodeID(id.String()), Addr: "10.65.56.191:6501"},
		{NodeID: ifaces.NodeID(id.String()), Addr: "[fd00::42]:6501"},
	}

	if got := host.transferProviders(info); !slices.Equal(got, want) {
		t.Fatalf("transferProviders = %+v, want %+v", got, want)
	}

	if addresses := host.h.Peerstore().Addrs(id); len(addresses) != len(info.Addrs) {
		t.Fatalf("cached %d addresses, want %d", len(addresses), len(info.Addrs))
	}

	d := digest.MustParse("sha256:abababababababababababababababababababababababababababababababab")

	key, err := DigestToCID(d)
	if err != nil {
		t.Fatal(err)
	}

	if err := host.d.ProviderStore().AddProvider(ctx, key.Hash(), info); err != nil {
		t.Fatal(err)
	}

	compare := func(a, b ifaces.Provider) int { return strings.Compare(a.Addr, b.Addr) }
	slices.SortFunc(want, compare)

	t.Run("synchronous", func(t *testing.T) {
		got, err := host.FindProviders(ctx, d)
		if err != nil {
			t.Fatal(err)
		}

		slices.SortFunc(got, compare)

		if !slices.Equal(got, want) {
			t.Fatalf("FindProviders = %+v, want %+v", got, want)
		}
	})

	t.Run("streaming", func(t *testing.T) {
		providers, lookupErr := host.StreamProviders(ctx, d, time.Second)

		var got []ifaces.Provider
		for provider := range providers {
			got = append(got, provider)
		}

		if err := lookupErr(); err != nil {
			t.Fatal(err)
		}

		slices.SortFunc(got, compare)

		if !slices.Equal(got, want) {
			t.Fatalf("StreamProviders = %+v, want %+v", got, want)
		}
	})

	t.Run("unusable record", func(t *testing.T) {
		host.h.Peerstore().ClearAddrs(id)

		unusable := peer.AddrInfo{ID: id, Addrs: info.Addrs[:1]}
		if got := host.transferProviders(unusable); len(got) != 0 {
			t.Fatalf("loopback-only record yielded %+v", got)
		}

		if got := host.h.Peerstore().Addrs(id); len(got) != 0 {
			t.Fatalf("unusable record cached %d addresses", len(got))
		}
	})
}
