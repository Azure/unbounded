// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type oidcTestRefreshGate struct {
	started chan struct{}
	release chan struct{}
}

type oidcTestWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *oidcTestWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })

	return c.Context.Done()
}

type oidcTestIssuer struct {
	server   *httptest.Server
	key      *ecdsa.PrivateKey
	verifier *KubernetesOIDCVerifier
	clock    atomic.Int64
	requests atomic.Int64
	status   atomic.Int64
	keys     atomic.Value
	gate     atomic.Pointer[oidcTestRefreshGate]
}

func newOIDCTestIssuer(t *testing.T) *oidcTestIssuer {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	issuer := &oidcTestIssuer{key: key}
	issuer.clock.Store(time.Now().UnixNano())
	issuer.status.Store(http.StatusOK)
	issuer.setKeys("original")
	issuer.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(oidcDiscoveryDocument{
				Issuer: issuer.server.URL, JWKSURL: issuer.server.URL + "/keys",
			})
		case "/keys":
			issuer.requests.Add(1)

			if gate := issuer.gate.Swap(nil); gate != nil {
				close(gate.started)

				select {
				case <-gate.release:
				case <-r.Context().Done():
					return
				}
			}

			w.WriteHeader(int(issuer.status.Load()))
			_ = json.NewEncoder(w).Encode(issuer.keys.Load())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(issuer.server.Close)

	issuer.verifier, err = newKubernetesOIDCVerifier(
		t.Context(), issuer.server.URL, "api", issuer.server.Client(),
		func() time.Time { return time.Unix(0, issuer.clock.Load()) },
	)
	if err != nil {
		t.Fatalf("create verifier: %v", err)
	}

	return issuer
}

func (s *oidcTestIssuer) setKeys(ids ...string) {
	keys := jsonWebKeySet{}
	for _, id := range ids {
		keys.Keys = append(keys.Keys, jsonWebKey{
			KeyID: id, KeyType: "EC", Algorithm: "ES256", Use: "sig", Curve: "P-256",
			X: base64.RawURLEncoding.EncodeToString(s.key.X.Bytes()),
			Y: base64.RawURLEncoding.EncodeToString(s.key.Y.Bytes()),
		})
	}

	s.keys.Store(keys)
}

func (s *oidcTestIssuer) token(t *testing.T, kid string) string {
	t.Helper()

	claims := &kubernetesServiceAccountClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: s.server.URL, Subject: "system:serviceaccount:system:node",
			Audience: jwt.ClaimStrings{"api"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	claims.Kubernetes.Namespace = "system"
	claims.Kubernetes.ServiceAccount.Name = "node"
	claims.Kubernetes.Node.Name = "node-a"
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = kid

	signed, err := token.SignedString(s.key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	return signed
}

func (s *oidcTestIssuer) assertRequests(t *testing.T, want int64) {
	t.Helper()

	if got := s.requests.Load(); got != want {
		t.Fatalf("JWKS requests = %d, want %d", got, want)
	}
}

func (s *oidcTestIssuer) blockRefresh(t *testing.T) *oidcTestRefreshGate {
	t.Helper()

	gate := &oidcTestRefreshGate{started: make(chan struct{}), release: make(chan struct{})}
	s.gate.Store(gate)
	t.Cleanup(func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	})

	return gate
}

func awaitOIDCTestResult(t *testing.T, result <-chan error) error {
	t.Helper()

	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("verification did not complete")
		return nil
	}
}

func TestOIDCUnknownKeySequentialCooldownAndRotation(t *testing.T) {
	issuer := newOIDCTestIssuer(t)

	for burst := range 2 {
		for i := range 32 {
			_, err := issuer.verifier.Verify(t.Context(), issuer.token(t, fmt.Sprintf("unknown-%d-%d", burst, i)))
			if err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("unknown key error = %v", err)
			}
		}

		issuer.assertRequests(t, int64(burst+1))
		issuer.clock.Add(int64(oidcKeyRefreshCooldown))
	}

	issuer.setKeys("original", "rotated")

	if _, err := issuer.verifier.Verify(t.Context(), issuer.token(t, "rotated")); err != nil {
		t.Fatalf("rotated key after cooldown: %v", err)
	}

	issuer.assertRequests(t, 3)

	issuer.setKeys("original", "rotated", "next")

	if _, err := issuer.verifier.Verify(t.Context(), issuer.token(t, "next")); err == nil {
		t.Fatal("rotation within cooldown should require retry")
	}

	issuer.clock.Add(int64(oidcKeyRefreshCooldown - time.Nanosecond))

	if _, err := issuer.verifier.Verify(t.Context(), issuer.token(t, "next")); err == nil {
		t.Fatal("rotation before cooldown boundary should require retry")
	}

	issuer.assertRequests(t, 3)
	issuer.clock.Add(1)

	if _, err := issuer.verifier.Verify(t.Context(), issuer.token(t, "next")); err != nil {
		t.Fatalf("rotation retry at cooldown boundary: %v", err)
	}

	issuer.assertRequests(t, 4)
}

func TestOIDCUnknownKeyConcurrentRefresh(t *testing.T) {
	issuer := newOIDCTestIssuer(t)
	issuer.clock.Add(int64(oidcKeyRefreshCooldown))
	issuer.setKeys("original", "rotated")
	gate := issuer.blockRefresh(t)
	rotated := issuer.token(t, "rotated")
	leader := make(chan error, 1)

	go func() {
		_, err := issuer.verifier.Verify(t.Context(), rotated)
		leader <- err
	}()

	<-gate.started

	const count = 32

	results := make(chan error, count)

	waiters := make([]*oidcTestWaitContext, 0, count)

	for i := range count {
		token := issuer.token(t, fmt.Sprintf("unknown-%d", i))
		ctx := &oidcTestWaitContext{Context: t.Context(), waiting: make(chan struct{})}

		waiters = append(waiters, ctx)
		go func() {
			_, err := issuer.verifier.Verify(ctx, token)
			results <- err
		}()
	}

	for _, waiter := range waiters {
		<-waiter.waiting
	}

	rotatedContext := &oidcTestWaitContext{Context: t.Context(), waiting: make(chan struct{})}
	rotatedResult := make(chan error, 1)

	go func() {
		_, err := issuer.verifier.Verify(rotatedContext, rotated)
		rotatedResult <- err
	}()

	<-rotatedContext.waiting

	known := issuer.token(t, "original")
	knownResult := make(chan error, 1)

	go func() {
		_, err := issuer.verifier.Verify(t.Context(), known)
		knownResult <- err
	}()

	if err := awaitOIDCTestResult(t, knownResult); err != nil {
		t.Fatalf("cached key during refresh: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancelContext := &oidcTestWaitContext{Context: ctx, waiting: make(chan struct{})}
	cancelResult := make(chan error, 1)

	go func() {
		_, err := issuer.verifier.Verify(cancelContext, rotated)
		cancelResult <- err
	}()

	<-cancelContext.waiting
	cancel()

	if err := awaitOIDCTestResult(t, cancelResult); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v", err)
	}

	close(gate.release)

	if err := awaitOIDCTestResult(t, leader); err != nil {
		t.Fatalf("rotated key: %v", err)
	}

	if err := awaitOIDCTestResult(t, rotatedResult); err != nil {
		t.Fatalf("rotated key waiter: %v", err)
	}

	for range count {
		if err := awaitOIDCTestResult(t, results); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("unknown key error = %v", err)
		}
	}

	issuer.assertRequests(t, 2)
}

func TestOIDCConcurrentRefreshFailure(t *testing.T) {
	issuer := newOIDCTestIssuer(t)
	issuer.clock.Add(int64(oidcKeyRefreshCooldown))
	issuer.status.Store(http.StatusServiceUnavailable)
	gate := issuer.blockRefresh(t)
	results := make(chan error, 17)

	unknown := issuer.token(t, "unknown")
	go func() {
		_, err := issuer.verifier.Verify(t.Context(), unknown)
		results <- err
	}()

	<-gate.started

	for i := range 16 {
		ctx := &oidcTestWaitContext{Context: t.Context(), waiting: make(chan struct{})}

		token := issuer.token(t, fmt.Sprintf("unknown-%d", i))
		go func() {
			_, err := issuer.verifier.Verify(ctx, token)
			results <- err
		}()

		<-ctx.waiting
	}

	// Even a long-running failed request gets a full cooldown on completion.
	issuer.clock.Add(int64(2 * oidcKeyRefreshCooldown))
	close(gate.release)

	for range 17 {
		if err := awaitOIDCTestResult(t, results); err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("coalesced refresh failure = %v", err)
		}
	}

	issuer.assertRequests(t, 2)
	issuer.status.Store(http.StatusOK)
	issuer.setKeys("original", "rotated")
	issuer.clock.Add(int64(oidcKeyRefreshCooldown - time.Nanosecond))

	for i := range 16 {
		if _, err := issuer.verifier.Verify(t.Context(), issuer.token(t, fmt.Sprintf("unknown-retry-%d", i))); err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("failure cooldown error = %v", err)
		}
	}

	issuer.assertRequests(t, 2)
	issuer.clock.Add(1)

	if _, err := issuer.verifier.Verify(t.Context(), issuer.token(t, "rotated")); err != nil {
		t.Fatalf("rotation after failure cooldown: %v", err)
	}

	issuer.assertRequests(t, 3)
}

func TestOIDCRefreshFailureCooldownAndRetry(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%t", stale), func(t *testing.T) {
			issuer := newOIDCTestIssuer(t)
			advance := oidcKeyRefreshCooldown
			kid := "rotated"

			if stale {
				advance = oidcKeyRefreshPeriod
				kid = "original"
			}

			issuer.clock.Add(int64(advance))
			issuer.status.Store(http.StatusServiceUnavailable)

			for i := range 32 {
				tokenKid := kid
				if !stale {
					tokenKid = fmt.Sprintf("unknown-%d", i)
				}

				if _, err := issuer.verifier.Verify(t.Context(), issuer.token(t, tokenKid)); err == nil || !strings.Contains(err.Error(), "503") {
					t.Fatalf("refresh failure error = %v", err)
				}
			}

			issuer.assertRequests(t, 2)
			issuer.status.Store(http.StatusOK)
			issuer.setKeys("original", "rotated")

			if _, err := issuer.verifier.Verify(t.Context(), issuer.token(t, kid)); err == nil {
				t.Fatal("failed refresh should remain in cooldown")
			}

			if !stale {
				if _, err := issuer.verifier.Verify(t.Context(), issuer.token(t, "original")); err != nil {
					t.Fatalf("known key after failed forced refresh: %v", err)
				}
			}

			issuer.clock.Add(int64(oidcKeyRefreshCooldown))

			if _, err := issuer.verifier.Verify(t.Context(), issuer.token(t, kid)); err != nil {
				t.Fatalf("refresh retry: %v", err)
			}

			issuer.assertRequests(t, 3)

			if issuer.verifier.keysStale() {
				t.Fatal("successful retry left keys stale")
			}
		})
	}
}

func TestOIDCStalePeriodicRefresh(t *testing.T) {
	issuer := newOIDCTestIssuer(t)
	token := issuer.token(t, "original")
	issuer.clock.Add(int64(oidcKeyRefreshPeriod - time.Nanosecond))

	if _, err := issuer.verifier.Verify(t.Context(), token); err != nil {
		t.Fatalf("fresh keys: %v", err)
	}

	issuer.assertRequests(t, 1)
	issuer.clock.Add(1)
	gate := issuer.blockRefresh(t)
	result := make(chan error, 1)

	go func() {
		_, err := issuer.verifier.Verify(t.Context(), token)
		result <- err
	}()

	<-gate.started

	cached := make(chan error, 1)

	go func() {
		_, err := issuer.verifier.Verify(t.Context(), token)
		cached <- err
	}()

	if err := awaitOIDCTestResult(t, cached); err != nil {
		t.Fatalf("cached key during periodic refresh: %v", err)
	}

	close(gate.release)

	if err := awaitOIDCTestResult(t, result); err != nil {
		t.Fatalf("periodic refresh: %v", err)
	}

	issuer.assertRequests(t, 2)

	if issuer.verifier.keysStale() {
		t.Fatal("periodic refresh left keys stale")
	}
}

func TestOIDCRefreshSurvivesInitiatingCallerCancellation(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%t", stale), func(t *testing.T) {
			issuer := newOIDCTestIssuer(t)
			advance := oidcKeyRefreshCooldown
			kid := "rotated"

			if stale {
				advance = oidcKeyRefreshPeriod
				kid = "original"
			}

			issuer.clock.Add(int64(advance))
			issuer.setKeys("original", "rotated")
			gate := issuer.blockRefresh(t)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			leader := make(chan error, 1)

			token := issuer.token(t, kid)
			go func() {
				_, err := issuer.verifier.Verify(ctx, token)
				leader <- err
			}()

			<-gate.started
			cancel()

			if err := awaitOIDCTestResult(t, leader); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled initiating caller: %v", err)
			}

			liveCtx := &oidcTestWaitContext{Context: t.Context(), waiting: make(chan struct{})}
			live := make(chan error, 1)

			rotated := issuer.token(t, "rotated")
			go func() {
				_, err := issuer.verifier.Verify(liveCtx, rotated)
				live <- err
			}()

			select {
			case <-liveCtx.waiting:
			case err := <-live:
				t.Fatalf("live caller did not join shared refresh: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("live caller did not reach refresh wait")
			}

			close(gate.release)

			if err := awaitOIDCTestResult(t, live); err != nil {
				t.Fatalf("live waiter after initiating caller canceled: %v", err)
			}

			if _, err := issuer.verifier.Verify(t.Context(), rotated); err != nil {
				t.Fatalf("cached rotated key: %v", err)
			}

			if _, err := issuer.verifier.Verify(t.Context(), issuer.token(t, "unknown")); err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("cooldown poisoned by initiating caller: %v", err)
			}

			issuer.assertRequests(t, 2)
		})
	}
}

func TestOIDCSharedRefreshTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int64

		verifier := &KubernetesOIDCVerifier{
			jwksURL: "https://issuer.example/keys",
			now:     time.Now,
			client: &http.Client{
				Transport: oidcTestTransport(func(req *http.Request) (*http.Response, error) {
					requests.Add(1)

					deadline, ok := req.Context().Deadline()
					if !ok || !deadline.Equal(time.Now().Add(oidcHTTPTimeout)) {
						t.Errorf("shared refresh deadline = %v, present = %t", deadline, ok)
					}

					<-req.Context().Done()

					return nil, req.Context().Err()
				}),
			},
		}
		for attempt := range 2 {
			started := time.Now()

			results := make(chan error, 1)
			go func() {
				results <- verifier.refreshKeys(t.Context(), true)
			}()

			synctest.Wait()

			select {
			case err := <-results:
				t.Fatalf("refresh completed before timeout: %v", err)
			default:
			}

			time.Sleep(oidcHTTPTimeout)
			synctest.Wait()

			if err := <-results; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("shared refresh timeout: %v", err)
			}

			if elapsed := time.Since(started); elapsed != oidcHTTPTimeout {
				t.Fatalf("shared refresh duration = %v, want %v", elapsed, oidcHTTPTimeout)
			}

			if err := verifier.refreshKeys(t.Context(), true); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout cooldown error = %v", err)
			}

			if got := requests.Load(); got != int64(attempt+1) {
				t.Fatalf("requests = %d, want %d", got, attempt+1)
			}

			time.Sleep(oidcKeyRefreshCooldown)
		}
	})
}

func TestOIDCConstructorCancellation(t *testing.T) {
	issuer := newOIDCTestIssuer(t)
	gate := issuer.blockRefresh(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	result := make(chan error, 1)

	go func() {
		_, err := newKubernetesOIDCVerifier(ctx, issuer.server.URL, "api", issuer.server.Client(), time.Now)
		result <- err
	}()

	<-gate.started
	cancel()

	if err := awaitOIDCTestResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("constructor cancellation: %v", err)
	}

	issuer.assertRequests(t, 2)
}

func TestOIDCRefreshOutlivesInitializationContext(t *testing.T) {
	issuer := newOIDCTestIssuer(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	verifier, err := newKubernetesOIDCVerifier(
		ctx, issuer.server.URL, "api", issuer.server.Client(),
		func() time.Time { return time.Unix(0, issuer.clock.Load()) },
	)
	if err != nil {
		t.Fatalf("create verifier: %v", err)
	}

	cancel()
	issuer.clock.Add(int64(oidcKeyRefreshCooldown))
	issuer.setKeys("original", "rotated")

	if _, err := verifier.Verify(t.Context(), issuer.token(t, "rotated")); err != nil {
		t.Fatalf("refresh after initialization context canceled: %v", err)
	}

	issuer.assertRequests(t, 3)
}
