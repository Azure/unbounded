// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
)

var (
	testKey      = []byte(strings.Repeat("k", MinPeerKeyLen))
	testOtherKey = []byte(strings.Repeat("o", MinPeerKeyLen))
	testAuth     = mustAuth(testKey)
)

func mustAuth(keys ...[]byte) *PeerAuth {
	a, err := NewPeerAuth(keys)
	if err != nil {
		panic(err)
	}

	return a
}

// signedGet returns a server-side GET request to target signed by testAuth.
func signedGet(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	testAuth.Sign(r, nil)

	return r
}

func TestParsePeerKeys(t *testing.T) {
	long := strings.Repeat("a", MinPeerKeyLen)
	other := strings.Repeat("b", MinPeerKeyLen)

	for _, tc := range []struct {
		name string
		data string
		keys int
		ok   bool
	}{
		{"one", long + "\n", 1, true},
		{"two with blanks and spaces", "\n  " + long + " \r\n\n" + other + "\n", 2, true},
		{"empty", "", 0, false},
		{"only blanks", "\n \n\t\n", 0, false},
		{"short", strings.Repeat("a", MinPeerKeyLen-1), 0, false},
		{"one short of two", long + "\nshort\n", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParsePeerKeys([]byte(tc.data))
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}

			if tc.ok && len(a.keys) != tc.keys {
				t.Fatalf("keys = %d, want %d", len(a.keys), tc.keys)
			}
		})
	}
}

func TestPeerAuthVerify(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	at := func(a *PeerAuth, ts time.Time) *PeerAuth {
		c := *a
		c.now = func() time.Time { return ts }

		return &c
	}
	server := at(testAuth, now)
	req := func(method, target string) *http.Request {
		return httptest.NewRequest(method, target, nil)
	}
	body := []byte("control message")

	for _, tc := range []struct {
		name   string
		signer *PeerAuth
		mutate func(r *http.Request) []byte
		ok     bool
	}{
		{"valid", server, nil, true},
		{"missing header", nil, nil, false},
		{"wrong key", at(mustAuth(testOtherKey), now), nil, false},
		{"past", at(testAuth, now.Add(-MaxAuthSkew-time.Second)), nil, false},
		{"future", at(testAuth, now.Add(MaxAuthSkew+time.Second)), nil, false},
		{"within skew", at(testAuth, now.Add(-MaxAuthSkew+time.Second)), nil, true},
		{"tampered query", server, func(r *http.Request) []byte { r.URL.RawQuery = "a=2"; return body }, false},
		{"tampered path", server, func(r *http.Request) []byte { r.URL.Path = ChunkPath; return body }, false},
		{"tampered method", server, func(r *http.Request) []byte { r.Method = http.MethodGet; return body }, false},
		{"tampered body", server, func(*http.Request) []byte { return []byte("forged leader snapshot") }, false},
		{"bad signature", server, func(r *http.Request) []byte {
			r.Header.Set(headerAuth, strings.Replace(r.Header.Get(headerAuth), "s=", "s=AA", 1))
			return body
		}, false},
		{"malformed", server, func(r *http.Request) []byte { r.Header.Set(headerAuth, "v1 t=x s=y"); return body }, false},
		{"wrong version", server, func(r *http.Request) []byte {
			r.Header.Set(headerAuth, strings.Replace(r.Header.Get(headerAuth), "v1", "v2", 1))
			return body
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := req(http.MethodPost, ControlPath+"?a=1")
			if tc.signer != nil {
				tc.signer.Sign(r, body)
			}

			b := body
			if tc.mutate != nil {
				b = tc.mutate(r)
			}

			err := server.Verify(r, b)
			if (err == nil) != tc.ok {
				t.Fatalf("Verify = %v, want ok=%v", err, tc.ok)
			}

			if err != nil && !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("Verify = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestPeerAuthRotation(t *testing.T) {
	r := req0()
	mustAuth(testOtherKey).Sign(r, nil)

	// The receiver signs with the new key and still accepts the old one.
	if err := mustAuth(testKey, testOtherKey).Verify(r, nil); err != nil {
		t.Fatalf("previous key rejected: %v", err)
	}

	if err := mustAuth(testKey).Verify(r, nil); err == nil {
		t.Fatal("retired key accepted")
	}

	var nilAuth *PeerAuth
	if err := nilAuth.Verify(r, nil); err == nil {
		t.Fatal("nil PeerAuth accepted a request")
	}
}

func req0() *http.Request {
	return httptest.NewRequest(http.MethodGet, ChunkPath+"?d=x", nil)
}

func TestHandlersRejectUnauthenticated(t *testing.T) {
	c := newCluster(t, clusterOpts{n: 1})
	n := c.nodes[0]

	var meshPtr atomic.Pointer[cachemesh.Mesh]

	mux := http.NewServeMux()
	n.node.Register(mux)
	mux.Handle(ControlPath, ControlHandler(&meshPtr, testAuth))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{ChunkPath + "?d=x", MetaPath + "?r=reg"} {
		for name, auth := range map[string]*PeerAuth{"unsigned": nil, "wrong key": mustAuth(testOtherKey)} {
			r, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}

			auth.Sign(r, nil)

			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}

			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s: status %d, want 401", name, path, resp.StatusCode)
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addr := strings.TrimPrefix(srv.URL, "http://")

	// A signed control message passes authentication and reaches the
	// (absent) mesh: 503, not 401.
	_, err := NewControlTransport(testAuth).Send(ctx, addr, []byte("msg"))
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("signed control: %v, want 503", err)
	}

	_, err = NewControlTransport(mustAuth(testOtherKey)).Send(ctx, addr, []byte("msg"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong-key control: %v, want 401", err)
	}

	// A signature over another body is rejected.
	r, err := http.NewRequest(http.MethodPost, srv.URL+ControlPath, bytes.NewReader([]byte("forged")))
	if err != nil {
		t.Fatal(err)
	}

	testAuth.Sign(r, []byte("msg"))

	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tampered control body: status %d, want 401", resp.StatusCode)
	}
}
