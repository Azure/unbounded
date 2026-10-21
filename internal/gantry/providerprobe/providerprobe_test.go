// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package providerprobe_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/providerprobe"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

type metadataDialer struct {
	mu     sync.Mutex
	errors map[string]error
	calls  map[string]int
	auth   []string
}

func (d *metadataDialer) HeadFromPeer(ctx context.Context, addr string, _ ifaces.OriginRef) (int64, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.calls[addr]++
	d.auth = append(d.auth, registryauth.Authorization(ctx))

	return 1, "application/octet-stream", d.errors[addr]
}

func TestFirstDoesNotForwardRegistryAuthorization(t *testing.T) {
	dialer := &metadataDialer{calls: map[string]int{}}
	ctx := registryauth.WithAuthorization(context.Background(), "Bearer secret")

	_, ok := providerprobe.First(ctx, dialer, []ifaces.Provider{{NodeID: "peer", Addr: "peer:5001"}}, ifaces.OriginRef{}, providerprobe.Attempted{}, 1)
	if !ok {
		t.Fatal("First did not find usable provider")
	}

	if len(dialer.auth) != 1 || dialer.auth[0] != "" {
		t.Fatalf("probe authorization = %q, want empty", dialer.auth)
	}
}

func TestFirstSkipsAttemptedAndReturnsUsableProvider(t *testing.T) {
	stale := ifaces.Provider{NodeID: "stale", Addr: "stale:5001"}
	fresh := ifaces.Provider{NodeID: "fresh", Addr: "fresh:5001"}
	dialer := &metadataDialer{
		errors: map[string]error{stale.Addr: errors.New("unreachable")},
		calls:  map[string]int{},
	}
	attempted := providerprobe.Attempted{}
	ref := ifaces.OriginRef{
		Registry:   "registry.example.com",
		Repository: "repo/image",
		Digest:     digest.MustParse("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Kind:       ifaces.KindBlob,
	}

	provider, ok := providerprobe.First(context.Background(), dialer, []ifaces.Provider{stale, fresh}, ref, attempted, 1)
	if !ok || provider != fresh {
		t.Fatalf("First = (%+v, %v), want fresh provider", provider, ok)
	}

	if len(attempted) != 2 {
		t.Fatalf("attempted providers = %d, want 2", len(attempted))
	}

	_, ok = providerprobe.First(context.Background(), dialer, []ifaces.Provider{stale, fresh}, ref, attempted, 1)
	if ok {
		t.Fatal("second First found an already-attempted provider")
	}

	if dialer.calls[stale.Addr] != 1 || dialer.calls[fresh.Addr] != 1 {
		t.Fatalf("calls = %+v, want one probe per provider", dialer.calls)
	}
}

func TestFirstReturnsFalseWhenEveryProviderFails(t *testing.T) {
	providers := []ifaces.Provider{
		{NodeID: "a", Addr: "a:5001"},
		{NodeID: "b", Addr: "b:5001"},
	}
	dialer := &metadataDialer{
		errors: map[string]error{
			providers[0].Addr: errors.New("unreachable"),
			providers[1].Addr: errors.New("not found"),
		},
		calls: map[string]int{},
	}

	_, ok := providerprobe.First(context.Background(), dialer, providers, ifaces.OriginRef{}, providerprobe.Attempted{}, 2)
	if ok {
		t.Fatal("First found an unusable provider")
	}
}

func TestFirstProbesDuplicateProviderOnce(t *testing.T) {
	provider := ifaces.Provider{NodeID: "peer", Addr: "peer:5001"}
	dialer := &metadataDialer{calls: map[string]int{}}

	_, ok := providerprobe.First(context.Background(), dialer, []ifaces.Provider{provider, provider}, ifaces.OriginRef{}, providerprobe.Attempted{}, 2)
	if !ok {
		t.Fatal("First did not find usable provider")
	}

	if calls := dialer.calls[provider.Addr]; calls != 1 {
		t.Fatalf("HEAD calls = %d, want 1", calls)
	}
}

type blockingMetadataDialer struct{}

func (blockingMetadataDialer) HeadFromPeer(ctx context.Context, _ string, _ ifaces.OriginRef) (int64, string, error) {
	<-ctx.Done()

	return 0, "", ctx.Err()
}

func TestFirstDoesNotMarkUnstartedCandidatesAfterTimeout(t *testing.T) {
	first := ifaces.Provider{NodeID: "first", Addr: "first:5001"}
	second := ifaces.Provider{NodeID: "second", Addr: "second:5001"}
	attempted := providerprobe.Attempted{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)

	defer cancel()

	_, ok := providerprobe.First(ctx, blockingMetadataDialer{}, []ifaces.Provider{first, second}, ifaces.OriginRef{}, attempted, 1)
	if ok {
		t.Fatal("First found a provider after timeout")
	}

	if _, found := attempted[first]; !found {
		t.Fatal("started provider missing from attempted set")
	}

	if _, found := attempted[second]; found {
		t.Fatal("unstarted provider was added to attempted set")
	}
}
