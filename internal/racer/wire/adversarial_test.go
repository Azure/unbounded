// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func vectorRoundTrip(name string, b []byte) ([]byte, error) {
	r := bytes.NewReader(b)

	switch name {
	case "publication.json":
		v, err := DecodePublication(r)
		if err != nil {
			return nil, err
		}

		return EncodePublication(v)
	case "bootstrap-request.json":
		v, err := DecodeBootstrap(r)
		if err != nil {
			return nil, err
		}

		return EncodeBootstrapRequest(v)
	case "bootstrap-response.json":
		v, err := DecodeBootstrapResponse(r)
		if err != nil {
			return nil, err
		}

		return EncodeBootstrap(v)
	case "bundle.json":
		v, err := DecodeBundle(r)
		if err != nil {
			return nil, err
		}

		return EncodeBundle(v)
	default:
		return nil, InvalidRequest
	}
}

func TestSharedVectors(t *testing.T) {
	for _, name := range []string{"bootstrap-request.json", "bootstrap-response.json", "bundle.json"} {
		t.Run(name, func(t *testing.T) {
			b := fixture(t, name)

			encoded, err := vectorRoundTrip(name, b)
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(b, encoded) {
				t.Fatal("wire bytes changed")
			}
		})
	}

	var cases []struct {
		Name, File, Old, New string
		Code                 ErrorCode
	}
	if err := json.Unmarshal(fixture(t, "rejections.json"), &cases); err != nil {
		t.Fatal(err)
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			b := fixture(t, tc.File)

			changed := bytes.Replace(b, []byte(tc.Old), []byte(tc.New), 1)
			if bytes.Equal(b, changed) {
				t.Fatal("mutation did not match")
			}

			if _, err := vectorRoundTrip(tc.File, changed); !errors.Is(err, tc.Code) {
				t.Fatalf("got %v, want %v", err, tc.Code)
			}
		})
	}
}

func TestCanonicalHashSemantics(t *testing.T) {
	v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	if err != nil {
		t.Fatal(err)
	}

	ph, mh, err := ContentHashes(v)
	if err != nil {
		t.Fatal(err)
	}

	v.Sequence, v.MembershipVersion = 0, 0
	// Reorder all three collections. Hashing must not mutate caller-owned slices.
	v.Members[0], v.Members[1] = v.Members[1], v.Members[0]
	v.Members[0].Rails[0], v.Members[0].Rails[1] = v.Members[0].Rails[1], v.Members[0].Rails[0]

	before, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	p2, m2, err := ContentHashes(v)
	if err != nil || ph != p2 || mh != m2 {
		t.Fatal("ordering or counters changed hash", err)
	}

	after, err := json.Marshal(v)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("hash mutated input", err)
	}

	v.Caches[0].SocketMode = 0o600

	p2, m2, err = ContentHashes(v)
	if err != nil || ph == p2 || mh != m2 {
		t.Fatal("cache-only hash semantics", err)
	}

	v.Members[0].PeerEndpoint = "[2001:db8::2]:7443"

	p3, m3, err := ContentHashes(v)
	if err != nil || p2 == p3 || m2 == m3 {
		t.Fatal("endpoint hash semantics", err)
	}

	for _, mutate := range []func(*Publication){
		func(p *Publication) { p.Members[0].Shares-- },
		func(p *Publication) { p.Members[0].AlignmentEnabled = !p.Members[0].AlignmentEnabled },
		func(p *Publication) { p.Members[0].Rails[0].Fabric += "-new" },
		func(p *Publication) { p.Cluster = "aaaaaaaa-1111-4111-8111-111111111111" },
	} {
		mutate(&v)

		nextP, nextM, err := ContentHashes(v)
		if err != nil || p3 == nextP || m3 == nextM {
			t.Fatal("member/cluster content not hashed", err)
		}

		p3, m3 = nextP, nextM
	}
}

type repeatedReader struct{ remaining, read int }

func (r *repeatedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}

	n := min(len(p), r.remaining)
	clear(p[:n])
	r.remaining -= n
	r.read += n

	return n, nil
}

func TestByteBoundsAndMalformedDocuments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
	}{{"bootstrap-request.json", MaxBootstrapBytes}, {"bootstrap-response.json", MaxBootstrapBytes}, {"bundle.json", MaxBundleBytes}, {"publication.json", MaxPublicationBytes}} {
		t.Run(tc.name, func(t *testing.T) {
			b := fixture(t, tc.name)

			padded := append(bytes.Clone(b), bytes.Repeat([]byte{' '}, tc.limit-len(b))...)
			if _, err := vectorRoundTrip(tc.name, padded); err != nil {
				t.Fatalf("exact bound: %v", err)
			}

			if _, err := vectorRoundTrip(tc.name, append(padded, ' ')); !errors.Is(err, TooLarge) {
				t.Fatalf("over bound: %v", err)
			}

			for _, bad := range [][]byte{nil, []byte("null"), []byte("[]"), b[:len(b)-1], append(bytes.Clone(b), []byte("{}")...), append(bytes.Clone(b), 0xff), []byte(strings.Repeat("[", 1000) + strings.Repeat("]", 1000))} {
				if _, err := vectorRoundTrip(tc.name, bad); !errors.Is(err, InvalidRequest) {
					t.Fatalf("malformed: %v", err)
				}
			}
		})
	}

	r := &repeatedReader{remaining: MaxBootstrapBytes * 10}
	if _, err := DecodeBootstrap(r); !errors.Is(err, TooLarge) || r.read != MaxBootstrapBytes+1 {
		t.Fatalf("unbounded read: %d, %v", r.read, err)
	}
}

func TestPublicationValidationAndLimits(t *testing.T) {
	v := Publication{SchemaVersion: 1, Cluster: "11111111-1111-4111-8111-111111111111", Sequence: 1, MembershipVersion: 1}

	b, err := EncodePublication(v)
	if err != nil || !bytes.Contains(b, []byte(`"members":[],"caches":[]`)) {
		t.Fatal("nil collections not normalized", err)
	}

	v.Members = make([]Member, MaxMembers+1)
	if _, err := EncodePublication(v); !errors.Is(err, TooLarge) {
		t.Fatal("member limit", err)
	}

	for _, name := range []string{".", "..", "a/b", "a\\b", "a\x00b", "A", "-a", "a-", "a..b", strings.Repeat("a", 64)} {
		if _, _, err := CanonicalSocketPaths(name); err == nil {
			t.Fatalf("accepted unsafe name %q", name)
		}
	}

	name := strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)

	client, _, err := CanonicalSocketPaths(name)
	if err != nil || len(client) != 107 {
		t.Fatalf("UDS boundary: %d %v", len(client), err)
	}

	if _, _, err := CanonicalSocketPaths(name + "b"); err == nil {
		t.Fatal("UDS overflow")
	}

	v, err = DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	if err != nil {
		t.Fatal(err)
	}

	v.Caches = append(v.Caches, v.Caches[0])
	if _, err := EncodePublication(v); err == nil {
		t.Fatal("duplicate cache")
	}

	v.Caches[1].ID = "66666666-6666-4666-8666-666666666666"
	if _, err := EncodePublication(v); err == nil {
		t.Fatal("duplicate cache name")
	}

	v.Caches = nil

	v.Members[0].Rails = []Rail{{Fabric: strings.Repeat("x", MaxPublicationBytes)}}
	if _, err := EncodePublication(v); !errors.Is(err, TooLarge) {
		t.Fatal("encoded byte limit", err)
	}
}

func TestUnknownFieldsAndExactNames(t *testing.T) {
	b := fixture(t, "publication.json")

	b = bytes.Replace(b, []byte(`"schema_version":1`), []byte(`"schema_version":1,"SCHEMA_VERSION":42`), 1)
	if _, err := DecodePublication(bytes.NewReader(b)); err != nil {
		t.Fatal("unknown case variant was not ignored", err)
	}

	b = bytes.Replace(b, []byte(`"schema_version":1,`), nil, 1)
	if _, err := DecodePublication(bytes.NewReader(b)); err == nil {
		t.Fatal("case variant supplied required field")
	}
}

func TestBundleValidationAndKeyIsolation(t *testing.T) {
	b, err := DecodeBundle(bytes.NewReader(fixture(t, "bundle.json")))
	if err != nil {
		t.Fatal(err)
	}

	ref := b.CacheKeys[0].Key

	key, err := NewCacheKey(ref, ActiveKey, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}

	ref.ID[0] = 123
	if key.Key.ID[0] != 0 {
		t.Fatal("material ingress retained mutable id")
	}

	if !key.EqualMaterial(b.CacheKeys[0]) || key.EqualMaterial(b.CacheKeys[1]) {
		t.Fatal("material equality")
	}

	b.PeerTrustRoots = append(b.PeerTrustRoots, b.PeerTrustRoots[0])
	if _, err := EncodeBundle(b); err == nil {
		t.Fatal("duplicate trust root")
	}

	for _, purpose := range []KeyPurpose{"", "future"} {
		ref.Purpose = purpose
		if _, err := NewCacheKey(ref, ActiveKey, [32]byte{}); err == nil {
			t.Fatal("invalid purpose")
		}
	}

	for _, code := range []ErrorCode{InvalidRequest, Unauthenticated, Forbidden, Conflict, TooLarge, UnsupportedVersion, Overloaded, Unavailable} {
		encoded, err := EncodeError(ErrorResponse{Code: code})
		if err != nil {
			t.Fatal(err)
		}

		decoded, err := DecodeError(bytes.NewReader(encoded))
		if err != nil || decoded.Code != code {
			t.Fatal("error codec", err)
		}
	}

	if _, err := DecodeError(strings.NewReader(`{"code":"future"}`)); err == nil {
		t.Fatal("unknown error enum")
	}
}

func FuzzDecodePublication(f *testing.F) {
	f.Add([]byte(`{"schema_version":1,"cluster":"11111111-1111-4111-8111-111111111111","sequence":"1","membership_version":"1","members":[],"caches":[]}`))
	f.Add([]byte(`{"x":1,"x":2}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := DecodePublication(bytes.NewReader(b))
		if err != nil {
			if !reflect.DeepEqual(v, Publication{}) {
				t.Fatal("partial result on error")
			}

			return
		}

		encoded, err := EncodePublication(v)
		if err != nil {
			t.Fatal("decoded invalid publication", err)
		}

		if _, err := DecodePublication(bytes.NewReader(encoded)); err != nil {
			t.Fatal("invalid re-encoding", err)
		}
	})
}

func TestMaximumMembership(t *testing.T) {
	v := Publication{SchemaVersion: 1, Cluster: "11111111-1111-4111-8111-111111111111", Sequence: 1, MembershipVersion: 1, Members: make([]Member, MaxMembers)}
	for i := range v.Members {
		v.Members[i] = Member{Node: NodeID(fmt.Sprintf("%08x-1111-4111-8111-111111111111", i)), Shares: 1, PeerEndpoint: "192.0.2.1:1"}
	}

	b, err := EncodePublication(v)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := DecodePublication(bytes.NewReader(b))
	if err != nil || len(decoded.Members) != MaxMembers {
		t.Fatal("exact member limit", err)
	}
	// Input remains below the byte bound but exceeds the independent member cap.
	start := bytes.Index(b, []byte(`"members":[`)) + len(`"members":[`)
	end := bytes.IndexByte(b[start:], '}') + start + 1
	tooMany := append(bytes.Clone(b[:start]), append(bytes.Clone(b[start:end]), ',')...)

	tooMany = append(tooMany, b[start:]...)
	if _, err := DecodePublication(bytes.NewReader(tooMany)); !errors.Is(err, TooLarge) {
		t.Fatal("decoded member cap", err)
	}
}

func FuzzDecodeSecretAndEnrollment(f *testing.F) {
	f.Add([]byte(`{"schema_version":1}`))
	f.Add([]byte(`{"cache_keys":[{"material":"AA=="}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		if v, err := DecodeBundle(bytes.NewReader(b)); err == nil {
			encoded, err := EncodeBundle(v)
			if err != nil {
				t.Fatal("decoded invalid bundle", err)
			}

			if _, err := DecodeBundle(bytes.NewReader(encoded)); err != nil {
				t.Fatal("bundle re-encoding", err)
			}
		}

		if v, err := DecodeBootstrap(bytes.NewReader(b)); err == nil {
			if _, err := EncodeBootstrapRequest(v); err != nil {
				t.Fatal("decoded invalid request", err)
			}
		}

		if v, err := DecodeBootstrapResponse(bytes.NewReader(b)); err == nil {
			if _, err := EncodeBootstrap(v); err != nil {
				t.Fatal("decoded invalid response", err)
			}
		}
	})
}
