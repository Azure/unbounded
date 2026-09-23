// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
)

type reviewTestClient struct {
	client.Client
	calls  atomic.Int32
	review func(context.Context, *authenticationv1.TokenReview) error
}

func (c *reviewTestClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if r, ok := obj.(*authenticationv1.TokenReview); ok {
		c.calls.Add(1)
		return c.review(ctx, r)
	}

	return c.Client.Create(ctx, obj, opts...)
}

func validReview(_ context.Context, r *authenticationv1.TokenReview) error {
	r.Status = authenticationv1.TokenReviewStatus{
		Authenticated: true, Audiences: r.Spec.Audiences,
		User: authenticationv1.UserInfo{Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-uid": {"pod-uid"}}},
	}

	return nil
}

func testCredential(exp time.Time, identity string) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"jti":%q}`, exp.Unix(), identity))) + ".signature"
}

func TestCredentialReuseExpiryRotationAudienceAndRevocation(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	c := &credentialCache{now: func() time.Time { return now }}
	kube := &reviewTestClient{review: validReview}
	token := testCredential(now.Add(time.Hour), "one")
	auth := func(token, audience string, want error) {
		t.Helper()

		uid, err := c.authenticate(context.Background(), kube, token, audience)
		if err != want || (err == nil && uid != "pod-uid") {
			t.Fatalf("uid=%q err=%v want=%v", uid, err, want)
		}
	}
	auth(token, controlAudience, nil)

	now = now.Add(credentialTTL - time.Nanosecond)

	auth(token, controlAudience, nil)

	if kube.calls.Load() != 1 {
		t.Fatal("cache hit refreshed/reviewed")
	}

	now = now.Add(time.Nanosecond)

	auth(token, controlAudience, nil)

	if kube.calls.Load() != 2 {
		t.Fatal("TTL boundary was not revalidated")
	}

	auth(testCredential(now.Add(time.Hour), "rotated"), controlAudience, nil)
	auth(token, "different-audience", nil)

	if kube.calls.Load() != 4 {
		t.Fatal("token/audience identities aliased")
	}
	// Revocation is visible after the original TTL, not a sliding hit TTL.
	kube.review = func(_ context.Context, _ *authenticationv1.TokenReview) error { return nil }

	auth(token, controlAudience, nil)

	now = now.Add(credentialTTL)

	auth(token, controlAudience, errInvalidCredential)
	auth(token, controlAudience, errInvalidCredential)

	if kube.calls.Load() != 6 {
		t.Fatal("invalid credential cached")
	}
	// The API is the authentication authority; even a forged future exp is denied.
	auth(testCredential(now.Add(time.Hour), "forged"), controlAudience, errInvalidCredential)

	kube.review = validReview
	short := testCredential(now.Add(time.Second), "short")
	auth(short, controlAudience, nil)

	now = now.Add(time.Second)

	auth(short, controlAudience, errInvalidCredential)
	// Even an erroneously positive reviewer cannot extend a known token expiry.
	auth(testCredential(now, "expired"), controlAudience, errInvalidCredential)
	auth(testCredential(time.Unix(0, 0), "zero"), controlAudience, errInvalidCredential)
}

func TestCredentialUnknownExpiryAndErrorsNeverCached(t *testing.T) {
	for _, payload := range []string{
		"opaque", "h.bad.s", "h." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".s",
		"h." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":"invalid"}`)) + ".s",
	} {
		c := new(credentialCache)

		kube := &reviewTestClient{review: validReview}
		for i := 0; i < 2; i++ {
			if _, err := c.authenticate(context.Background(), kube, payload, controlAudience); err != nil {
				t.Fatal(err)
			}
		}

		if kube.calls.Load() != 2 || c.lru.Len() != 0 {
			t.Fatal("unknown expiry was cached")
		}
	}

	for _, mode := range []string{"api", "status", "invalid", "audience", "uid", "multiple-uid", "large-uid"} {
		t.Run(mode, func(t *testing.T) {
			c := new(credentialCache)
			kube := &reviewTestClient{review: func(ctx context.Context, r *authenticationv1.TokenReview) error {
				validReview(ctx, r)

				switch mode {
				case "api":
					return errors.New("API transport failure")
				case "status":
					r.Status.Error = "review backend unavailable"
				case "invalid":
					r.Status.Authenticated = false
				case "audience":
					r.Status.Audiences = []string{"another"}
				case "uid":
					r.Status.User.Extra = nil
				case "multiple-uid":
					r.Status.User.Extra["authentication.kubernetes.io/pod-uid"] = []string{"one", "two"}
				case "large-uid":
					r.Status.User.Extra["authentication.kubernetes.io/pod-uid"] = []string{strings.Repeat("x", 257)}
				}

				return nil
			}}

			want := errInvalidCredential
			if mode == "api" || mode == "status" {
				want = errCredentialUnavailable
			}

			token := testCredential(time.Now().Add(time.Hour), mode)
			for i := 0; i < 2; i++ {
				if _, err := c.authenticate(context.Background(), kube, token, controlAudience); err != want {
					t.Fatalf("%v", err)
				}
			}

			if kube.calls.Load() != 2 || c.lru.Len() != 0 {
				t.Fatal("failure cached")
			}

			kube.review = validReview
			if _, err := c.authenticate(context.Background(), kube, token, controlAudience); err != nil {
				t.Fatal("recovery", err)
			}
		})
	}
}

func waitCredentialState(t *testing.T, c *credentialCache, predicate func() bool) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		ok := predicate()
		c.mu.Unlock()

		if ok {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal("credential state deadline")
}

func TestCredentialSingleflightWaiterBoundAndCancellation(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			c := new(credentialCache)

			gate := make(chan struct{})
			defer close(gate)

			kube := &reviewTestClient{review: func(ctx context.Context, r *authenticationv1.TokenReview) error {
				select {
				case <-gate:
				case <-ctx.Done():
					return ctx.Err()
				}

				if failure {
					return errors.New("API down")
				}

				return validReview(ctx, r)
			}}
			token := testCredential(time.Now().Add(time.Hour), "shared")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			results := make(chan error, credentialWaiters+1)

			go func() { _, err := c.authenticate(ctx, kube, token, controlAudience); results <- err }()

			waitCredentialState(t, c, func() bool { return len(c.flights) == 1 })

			for i := 0; i < credentialWaiters; i++ {
				go func() { _, err := c.authenticate(context.Background(), kube, token, controlAudience); results <- err }()
			}

			waitCredentialState(t, c, func() bool {
				for _, f := range c.flights {
					return f.waiters == credentialWaiters
				}

				return false
			})

			if _, err := c.authenticate(context.Background(), kube, token, controlAudience); err != errCredentialUnavailable {
				t.Fatal("waiter cap", err)
			}

			if failure {
				cancel()
			} else {
				gate <- struct{}{}
			}

			for i := 0; i < credentialWaiters+1; i++ {
				err := <-results
				if (!failure && err != nil) || (failure && err != errCredentialUnavailable) {
					t.Fatal(err)
				}
			}

			if kube.calls.Load() != 1 || len(c.flights) != 0 {
				t.Fatal("duplicate flight/leaked state")
			}

			if failure && c.lru.Len() != 0 {
				t.Fatal("canceled flight cached")
			}
		})
	}
}

func TestCredentialCapacityAndDistinctFlightBound(t *testing.T) {
	c := new(credentialCache)
	kube := &reviewTestClient{review: validReview}

	exp := time.Now().Add(time.Hour)
	for i := 0; i < credentialCapacity+100; i++ {
		if _, err := c.authenticate(context.Background(), kube, testCredential(exp, fmt.Sprint(i)), controlAudience); err != nil {
			t.Fatal(err)
		}
	}

	if len(c.entries) != credentialCapacity || c.lru.Len() != credentialCapacity {
		t.Fatal("unbounded cache")
	}

	before := kube.calls.Load()
	c.authenticate(context.Background(), kube, testCredential(exp, "0"), controlAudience)

	if kube.calls.Load() != before+1 {
		t.Fatal("LRU did not evict oldest")
	}

	gate := make(chan struct{})
	defer close(gate)

	kube.review = func(ctx context.Context, r *authenticationv1.TokenReview) error {
		select {
		case <-gate:
			return validReview(ctx, r)
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < credentialFlights; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			c.authenticate(ctx, kube, testCredential(exp, fmt.Sprint("flight", i)), controlAudience)
		}(i)
	}

	waitCredentialState(t, c, func() bool { return len(c.flights) == credentialFlights })

	if _, err := c.authenticate(ctx, kube, testCredential(exp, "overflow"), controlAudience); err != errCredentialUnavailable {
		t.Fatal("flight cap", err)
	}
	// An unrelated positive entry is still usable under miss pressure.
	if _, err := c.authenticate(ctx, kube, testCredential(exp, "0"), controlAudience); err != nil {
		t.Fatal("hit blocked", err)
	}

	cancel()
	wg.Wait()

	if len(c.flights) != 0 {
		t.Fatal("flight leak")
	}
}

func TestCredentialFollowerCancellationAndReviewDeadline(t *testing.T) {
	c := new(credentialCache)
	started, release := make(chan struct{}), make(chan struct{})
	kube := &reviewTestClient{review: func(ctx context.Context, r *authenticationv1.TokenReview) error {
		close(started)

		select {
		case <-release:
			return validReview(ctx, r)
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	token := testCredential(time.Now().Add(time.Hour), "cancel-follower")
	result := make(chan error, 1)

	go func() { _, err := c.authenticate(context.Background(), kube, token, controlAudience); result <- err }()

	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.authenticate(ctx, kube, token, controlAudience); err != errCredentialUnavailable {
		t.Fatal("canceled follower", err)
	}

	close(release)

	if err := <-result; err != nil {
		t.Fatal("follower canceled leader", err)
	}

	if kube.calls.Load() != 1 {
		t.Fatal("duplicate review")
	}

	kube.review = func(ctx context.Context, _ *authenticationv1.TokenReview) error {
		<-ctx.Done()
		return ctx.Err()
	}

	start := time.Now()
	if _, err := c.authenticate(context.Background(), kube, testCredential(time.Now().Add(time.Hour), "timeout"), controlAudience); err != errCredentialUnavailable {
		t.Fatal("review deadline", err)
	}

	if elapsed := time.Since(start); elapsed < credentialTimeout || elapsed >= 2*time.Second {
		t.Fatal("review did not finish within first-byte budget", elapsed)
	}
}

func TestCredentialSlowReviewDoesNotExtendTTL(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	c := &credentialCache{now: func() time.Time { return now }}
	kube := &reviewTestClient{review: func(ctx context.Context, r *authenticationv1.TokenReview) error {
		now = now.Add(4 * time.Second)
		return validReview(ctx, r)
	}}

	token := testCredential(now.Add(time.Hour), "slow")
	if _, err := c.authenticate(context.Background(), kube, token, controlAudience); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Second)

	if _, err := c.authenticate(context.Background(), kube, token, controlAudience); err != nil {
		t.Fatal(err)
	}

	if kube.calls.Load() != 2 {
		t.Fatal("API latency extended lifetime")
	}

	now = now.Add(time.Second)

	kube.review = func(context.Context, *authenticationv1.TokenReview) error { return errors.New("API unavailable") }
	if _, err := c.authenticate(context.Background(), kube, token, controlAudience); err != errCredentialUnavailable {
		t.Fatal("expired positive entry reused during API failure", err)
	}
}

func TestTokenReviewConfiguration(t *testing.T) {
	base := &rest.Config{QPS: 2, Burst: 3}

	cfg, err := tokenReviewConfig(base, 50, 80)
	if err != nil || cfg.QPS != 50 || cfg.Burst != 80 || cfg.Timeout != credentialTimeout || base.QPS != 2 {
		t.Fatal(cfg, err)
	}

	for _, qps := range []float64{-1, 0, math.SmallestNonzeroFloat64, math.NaN(), math.Inf(1), 100001} {
		if _, err := tokenReviewConfig(base, qps, 30); err == nil {
			t.Fatal("invalid QPS accepted")
		}
	}

	for _, burst := range []int{-1, 0, 100001} {
		if _, err := tokenReviewConfig(base, 20, burst); err == nil {
			t.Fatal("invalid burst accepted")
		}
	}
}

// Count actual TokenReview HTTP POSTs through the real client and its 20/30
// limiter. TLS-authenticated heartbeats must never reach that API, even when
// bearer credentials change or the selected Pod is replaced.
func TestPhase4TLSAvoidsTokenReviewAndChecksSelection(t *testing.T) {
	var (
		posts               atomic.Int32
		apiFailure, invalid atomic.Bool
	)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" {
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			http.Error(w, "bad route", 500)

			return
		}

		posts.Add(1)

		if apiFailure.Load() {
			http.Error(w, "unavailable", 500)
			return
		}

		var review authenticationv1.TokenReview
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
			t.Error(err)
			return
		}

		validReview(r.Context(), &review)
		review.Status.Authenticated = !invalid.Load()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(&review)
	}))
	defer api.Close()

	scheme := runtime.NewScheme()
	authenticationv1.AddToScheme(scheme)

	cfg, err := tokenReviewConfig(&rest.Config{Host: api.URL}, 20, 30)
	if err != nil {
		t.Fatal(err)
	}

	cfg.ContentType = "application/json"

	reviewer, err := newTokenReviewClient(cfg, scheme)
	if err != nil {
		t.Fatal(err)
	}

	n, p, svc := fixtures()
	p.UID = "pod-uid"

	g, _, err := buildGeneration("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, []corev1.Service{*svc})
	if err != nil {
		t.Fatal(err)
	}

	g.Revision = 1

	store := stateStore{client: fakeKube(n, p, svc), namespace: "state"}
	if err := store.commit(context.Background(), g, nil); err != nil {
		t.Fatal(err)
	}

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{controlStore: store, reviewClient: reviewer}
	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Truncate(time.Second)
	s.credentials.now = func() time.Time { return now }
	token := testCredential(now.Add(time.Hour), "initial")
	node := g.Nodes[n.Name].ID
	digest := ""

	call := func(phase uint32, want int) {
		t.Helper()

		req := httptest.NewRequest("GET", "/v3/"+identity("universe", "default")+"/"+node, nil)
		req.SetPathValue("universe", identity("universe", "default"))
		req.SetPathValue("node", node)
		req.Header.Set("Authorization", "Bearer "+token)
		controlTLS(req, "pod-uid")
		req.Header.Set("X-Racer-Boot", strings.Repeat("ab", 32))
		req.Header.Set("X-Racer-Profile", "1")
		req.Header.Set("X-Racer-Phase", fmt.Sprint(phase))
		req.Header.Set("X-Racer-Digest", digest)

		w := httptest.NewRecorder()
		s.control(w, req)

		if w.Code != want {
			t.Fatalf("status=%d want=%d: %s", w.Code, want, w.Body.String())
		}

		if want == 200 {
			var command pb.ControlCommand

			if err := proto.Unmarshal(w.Body.Bytes(), &command); err != nil {
				t.Fatal(err)
			}

			if phase == 4 && command.Phase != 4 {
				t.Fatal("lost terminal phase")
			}

			digest = hex.EncodeToString(command.SnapshotDigest)
		}
	}
	for phase := uint32(0); phase < 4; phase++ {
		call(phase, 200)
	}

	for i := 0; i < 80; i++ {
		call(4, 200)

		now = now.Add(250 * time.Millisecond)
	}

	if posts.Load() != 0 {
		t.Fatalf("84 mTLS heartbeats made %d TokenReview POSTs, want 0", posts.Load())
	}

	if ack := s.rollouts["default"].acks[node]; ack.phase != 4 || time.Since(ack.seen) >= 15*time.Second {
		t.Fatal("phase heartbeat lost")
	}
	// Refresh the heartbeat, then commit a replacement Pod selection.
	call(4, 200)

	oldPosts := posts.Load()
	g.Revision++
	m := g.Nodes[n.Name]
	m.PodUID = "replacement"
	g.Nodes[n.Name] = m

	index, err = indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	call(4, 403)

	if posts.Load() != oldPosts {
		t.Fatal("selection check did not use TLS identity")
	}
	// Bearer credential changes and review failures cannot affect TLS selection.
	token = testCredential(now.Add(time.Hour), "rotated")

	apiFailure.Store(true)
	call(4, 403)
	call(4, 403)
	apiFailure.Store(false)
	invalid.Store(true)
	call(4, 403)
	call(4, 403)

	if posts.Load() != oldPosts {
		t.Fatal("mTLS heartbeat consulted TokenReview")
	}
}
