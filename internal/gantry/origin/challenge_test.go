// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestAuthenticationChallengeParallelCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})

	var hits atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(entered)
		}

		if r.Header.Get("Authorization") != "" {
			t.Error("credential leaked to challenge probe")
		}

		<-release
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		w.WriteHeader(401)
	}))
	defer srv.Close()
	defer close(release)

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	c.registries["reg"].hc = srv.Client()
	leader := make(chan error, 1)

	go func() { _, _, err := c.AuthenticationChallenge(context.Background(), "reg"); leader <- err }()

	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)

	go func() { _, _, err := c.AuthenticationChallenge(ctx, "reg"); waiter <- err }()

	cancel()

	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter blocked behind network I/O")
	}
	// Resource challenges can be remembered while the probe is in flight.
	remembered := make(chan struct{})

	go func() { c.registries["reg"].rememberAuthenticationChallenge(`Basic realm="new"`); close(remembered) }()

	select {
	case <-remembered:
	case <-time.After(time.Second):
		t.Fatal("challenge mutex held during network I/O")
	}

	challenge, _, err := c.AuthenticationChallenge(context.Background(), "reg")
	if err != nil || challenge != `Basic realm="new"` || hits.Load() != 1 {
		t.Fatalf("challenge=%q err=%v hits=%d", challenge, err, hits.Load())
	}
}

func TestAuthenticationChallengeFailureBackoffAndRefresh(t *testing.T) {
	var (
		hits   atomic.Int64
		status atomic.Int64
	)
	status.Store(429)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	r := c.registries["reg"]
	r.hc = srv.Client()

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			_, _, err := c.AuthenticationChallenge(context.Background(), "reg")

			var oe *ifaces.OriginError
			if !errors.As(err, &oe) || oe.StatusCode != 429 || oe.Class != ifaces.FailureRateLimited {
				t.Errorf("failure classification: %v", err)
			}
		}()
	}

	wg.Wait()

	if hits.Load() != 1 {
		t.Fatalf("failure stampede: %d probes", hits.Load())
	}

	status.Store(200)
	r.challengeMu.Lock()
	if delay := time.Until(r.challengeRetry); delay > authenticationFailureBackoff {
		t.Error("unbounded failure backoff")
	}

	r.challengeRetry = time.Now().Add(-time.Second)
	r.challengeMu.Unlock()

	challenge, required, err := c.AuthenticationChallenge(context.Background(), "reg")
	if err != nil || required || challenge != "" || hits.Load() != 2 {
		t.Fatalf("refresh=%q %v %v hits=%d", challenge, required, err, hits.Load())
	}
}

func TestAuthenticationChallengeCanceledLeaderRetries(t *testing.T) {
	var hits atomic.Int64

	entered := make(chan struct{})

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(entered)
			<-r.Context().Done()

			return
		}

		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		w.WriteHeader(401)
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	c.registries["reg"].hc = srv.Client()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { _, _, err := c.AuthenticationChallenge(ctx, "reg"); done <- err }()

	<-entered
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader=%v", err)
	}

	challenge, required, err := c.AuthenticationChallenge(context.Background(), "reg")
	if err != nil || !required || !strings.HasPrefix(challenge, "Basic ") || hits.Load() != 2 {
		t.Fatalf("retry=%q %v %v", challenge, required, err)
	}
}

func TestAuthenticationChallengeParallelSuccess(t *testing.T) {
	var hits atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		w.WriteHeader(401)
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	c.registries["reg"].hc = srv.Client()
	start := make(chan struct{})

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			<-start

			value, required, err := c.AuthenticationChallenge(context.Background(), "reg")
			if err != nil || !required || value != `Basic realm="registry"` {
				t.Errorf("challenge=%q %v %v", value, required, err)
			}
		}()
	}

	close(start)
	wg.Wait()

	if hits.Load() != 1 {
		t.Fatalf("parallel discovery made %d requests", hits.Load())
	}
}

func TestAuthenticationChallengeCanceledLeaderKeepsConcurrentProbe(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})

	var hits atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(entered)
		}

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		w.WriteHeader(401)
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	r := c.registries["reg"]
	r.hc = srv.Client()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	leader, waiter := make(chan error, 1), make(chan error, 1)

	go func() { _, _, err := c.AuthenticationChallenge(ctx, "reg"); leader <- err }()

	<-entered

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()

	go func() {
		value, required, err := c.AuthenticationChallenge(waitCtx, "reg")
		if err == nil && (!required || value != `Basic realm="registry"`) {
			err = errors.New("lost shared probe result")
		}

		waiter <- err
	}()
	// Confirm the second caller joined before canceling the first caller.
	for {
		r.challengeMu.Lock()
		joined := r.challengeFlight != nil && r.challengeFlight.waiters == 2
		r.challengeMu.Unlock()

		if joined {
			break
		}

		select {
		case <-waitCtx.Done():
			close(release)
			t.Fatal("waiter did not join")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()

	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader=%v", err)
	}

	close(release)

	if err := <-waiter; err != nil {
		t.Fatal(err)
	}

	if hits.Load() != 1 {
		t.Fatalf("leader cancellation restarted shared probe: hits=%d", hits.Load())
	}
}
