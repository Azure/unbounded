// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/mirror"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
)

// This dependency deliberately exposes no registry content methods.
type authenticationChallengeFunc func(context.Context, string) (string, bool, error)

func (f authenticationChallengeFunc) AuthenticationChallenge(ctx context.Context, registry string) (string, bool, error) {
	return f(ctx, registry)
}

func TestRacerExplicitAuthenticationChallenge(t *testing.T) {
	for _, tc := range []struct {
		name, authorization, challenge string
		required                       bool
		err                            error
		status, challenges, requests   int
	}{
		{name: "public", status: 200, challenges: 1, requests: 1},
		{name: "basic", challenge: `Basic realm="private"`, required: true, status: 401, challenges: 1},
		{name: "bearer", challenge: `Bearer realm="https://registry/token"`, required: true, status: 401, challenges: 1},
		{name: "unavailable", err: errors.New("challenge unavailable"), status: 503, challenges: 1},
		{name: "delegated-basic", authorization: "Basic dXNlcjpwYXNz", required: true, status: 200, requests: 1},
		{name: "delegated-bearer", authorization: "Bearer delegated", required: true, status: 200, requests: 1},
		{name: "unsupported", authorization: "Digest unsupported", status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := digestOf(nil)

			var requests atomic.Int64

			client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("ETag", `"`+d.Hex()+`"`)
				w.Header().Set("Content-Length", "0")
			}))
			challenges := 0
			auth := authenticationChallengeFunc(func(ctx context.Context, registry string) (string, bool, error) {
				challenges++

				if registry != "registry.example" {
					t.Errorf("challenge registry = %q", registry)
				}

				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 2*time.Second {
					t.Error("challenge is not bounded")
				}

				return tc.challenge, tc.required, tc.err
			})
			server := mirror.NewRacer(reviewConfig(), auth, &gantryracer.Backend{Client: client})
			r := httptest.NewRequest(http.MethodHead, "/v2/repo/blobs/"+d.String(), nil)
			r.Header.Set("Authorization", tc.authorization)

			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, r)

			if w.Code != tc.status || challenges != tc.challenges || requests.Load() != int64(tc.requests) {
				t.Fatal("incorrect authentication routing", w.Code, challenges, requests.Load())
			}

			if got := w.Header().Get("WWW-Authenticate"); got != tc.challenge {
				t.Fatalf("challenge = %q, want %q", got, tc.challenge)
			}
		})
	}
}
