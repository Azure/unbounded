// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Peer authentication.
//
// Every request a member sends to another member's /cachemesh/* endpoint
// carries headerAuth:
//
//	Gantry-Cachemesh-Auth: v1 t=<unix seconds> s=<base64url HMAC-SHA256>
//
// The MAC covers authDomain, the method, the escaped URL path, the raw
// query, the timestamp, and the SHA-256 of the body (of the empty body for
// GET). The first key signs; every key verifies, so keys rotate by adding
// the new key second, then moving it first, then dropping the old one.
const (
	headerAuth = "Gantry-Cachemesh-Auth"
	authDomain = "gantry-cachemesh-auth"
	authVer    = "v1"

	// MinPeerKeyLen is the minimum length of a peer key in bytes.
	MinPeerKeyLen = 32
	// MaxAuthSkew bounds the difference between a request's timestamp and
	// the receiver's clock.
	MaxAuthSkew = 2 * time.Minute
)

// ErrUnauthenticated reports a request without a valid signature.
var ErrUnauthenticated = errors.New("meshfetch: unauthenticated peer request")

// PeerAuth signs and verifies requests between members with shared keys.
type PeerAuth struct {
	keys [][]byte
	now  func() time.Time
}

// NewPeerAuth returns a PeerAuth that signs with keys[0] and verifies with
// every key. Each key must be at least MinPeerKeyLen bytes.
func NewPeerAuth(keys [][]byte) (*PeerAuth, error) {
	if len(keys) == 0 {
		return nil, errors.New("meshfetch: no peer keys")
	}

	cp := make([][]byte, len(keys))

	for i, k := range keys {
		if len(k) < MinPeerKeyLen {
			return nil, fmt.Errorf("meshfetch: peer key %d is %d bytes, need at least %d", i+1, len(k), MinPeerKeyLen)
		}

		cp[i] = bytes.Clone(k)
	}

	return &PeerAuth{keys: cp, now: time.Now}, nil
}

// ParsePeerKeys parses a key file: one key per line, surrounding
// whitespace trimmed, blank lines ignored.
func ParsePeerKeys(data []byte) (*PeerAuth, error) {
	var keys [][]byte

	for line := range strings.SplitSeq(string(data), "\n") {
		if k := strings.TrimSpace(line); k != "" {
			keys = append(keys, []byte(k))
		}
	}

	return NewPeerAuth(keys)
}

// Sign sets the auth header on r, whose body is body (nil for none). A nil
// PeerAuth sends the request unsigned, which peers reject.
func (a *PeerAuth) Sign(r *http.Request, body []byte) {
	if a == nil {
		return
	}

	ts := a.now().Unix()
	mac := a.mac(a.keys[0], r, ts, body)
	r.Header.Set(headerAuth, authVer+" t="+strconv.FormatInt(ts, 10)+" s="+base64.RawURLEncoding.EncodeToString(mac))
}

// Verify checks the auth header of r against body. It returns an error
// wrapping ErrUnauthenticated when the header is missing, malformed, stale,
// or signed with no known key. A nil PeerAuth rejects every request.
func (a *PeerAuth) Verify(r *http.Request, body []byte) error {
	if a == nil {
		return fmt.Errorf("%w: no peer keys configured", ErrUnauthenticated)
	}

	v := r.Header.Get(headerAuth)
	if v == "" {
		return fmt.Errorf("%w: missing %s", ErrUnauthenticated, headerAuth)
	}

	f := strings.Fields(v)
	if len(f) != 3 || f[0] != authVer || !strings.HasPrefix(f[1], "t=") || !strings.HasPrefix(f[2], "s=") {
		return fmt.Errorf("%w: malformed header", ErrUnauthenticated)
	}

	ts, err := strconv.ParseInt(f[1][2:], 10, 64)
	if err != nil {
		return fmt.Errorf("%w: malformed timestamp", ErrUnauthenticated)
	}

	sig, err := base64.RawURLEncoding.DecodeString(f[2][2:])
	if err != nil || len(sig) != sha256.Size {
		return fmt.Errorf("%w: malformed signature", ErrUnauthenticated)
	}

	skew := a.now().Sub(time.Unix(ts, 0))
	if skew > MaxAuthSkew || skew < -MaxAuthSkew {
		return fmt.Errorf("%w: timestamp outside the %s window", ErrUnauthenticated, MaxAuthSkew)
	}

	ok := false

	for _, k := range a.keys {
		// Check every key so the time taken does not tell which matched.
		if hmac.Equal(sig, a.mac(k, r, ts, body)) {
			ok = true
		}
	}

	if !ok {
		return fmt.Errorf("%w: bad signature", ErrUnauthenticated)
	}

	return nil
}

func (a *PeerAuth) mac(key []byte, r *http.Request, ts int64, body []byte) []byte {
	bh := sha256.Sum256(body)
	m := hmac.New(sha256.New, key)

	// Fields are newline-separated; none of them can contain a newline
	// (method token, escaped path, raw query, decimal, hex).
	for _, s := range []string{authDomain + "/" + authVer, r.Method, r.URL.EscapedPath(), r.URL.RawQuery, strconv.FormatInt(ts, 10)} {
		m.Write([]byte(s))
		m.Write([]byte{'\n'})
	}

	m.Write(bh[:])

	return m.Sum(nil)
}

// authorize verifies r and answers 401 when it fails.
func authorize(w http.ResponseWriter, r *http.Request, a *PeerAuth, body []byte) bool {
	if err := a.Verify(r, body); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}

	return true
}
