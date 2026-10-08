// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

func TestBootstrapBlockDevicesVectors(t *testing.T) {
	var vectors []struct {
		Name, Fields, Pattern string
		Code                  ErrorCode
	}
	require.NoError(t, json.Unmarshal(fixture(t, "bootstrap-block-devices.json"), &vectors))

	base := fixture(t, "bootstrap-response.json")
	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			raw := string(base[:len(base)-1]) + vector.Fields + "}"

			response, err := DecodeBootstrapResponse(strings.NewReader(raw))
			if vector.Code != "" {
				require.ErrorIs(t, err, vector.Code)
				return
			}

			require.NoError(t, err)
			require.Equal(t, vector.Pattern, response.BlockDevices)
			encoded, err := EncodeBootstrap(response)
			require.NoError(t, err)

			if vector.Pattern == "" {
				require.Equal(t, base, encoded)
			} else {
				require.Equal(t, raw, string(encoded))
			}
		})
	}
}

func TestCanonicalHashSemantics(t *testing.T) {
	v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	require.NoError(t, err)

	ph, mh, err := ContentHashes(v)
	require.NoError(t, err)

	v.Sequence, v.MembershipVersion = 0, 0
	// Reorder all three collections. Hashing must not mutate caller-owned slices.
	v.Members[0], v.Members[1] = v.Members[1], v.Members[0]
	v.Members[0].RDMANICs[0], v.Members[0].RDMANICs[1] = v.Members[0].RDMANICs[1], v.Members[0].RDMANICs[0]

	before, err := json.Marshal(v)
	require.NoError(t, err)

	p2, m2, err := ContentHashes(v)
	require.NoError(t, err)
	require.Equal(t, ph, p2, "ordering or counters changed content hash")
	require.Equal(t, mh, m2, "ordering or counters changed membership hash")

	after, err := json.Marshal(v)
	require.NoError(t, err)
	require.Equal(t, before, after, "hash mutated input")

	v.Caches[0].ID = "66666666-6666-4666-8666-666666666666"

	p2, m2, err = ContentHashes(v)
	require.NoError(t, err)
	require.NotEqual(t, ph, p2, "cache changes content hash")
	require.Equal(t, mh, m2, "cache does not change membership hash")

	v.Members[0].PeerEndpoint = "[2001:db8::2]:7443"

	p3, m3, err := ContentHashes(v)
	require.NoError(t, err)
	require.NotEqual(t, p2, p3, "endpoint changes content hash")
	require.NotEqual(t, m2, m3, "endpoint changes membership hash")

	for _, mutate := range []func(*Publication){
		func(p *Publication) { p.Members[0].Shares-- },
		func(p *Publication) { p.Members[0].RDMANICs[0].Port++ },
		func(p *Publication) { p.Members[0].RDMANICs[0].Device += "-new" },
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

	v.Members[0].RDMANICs = []RDMANIC{{Device: strings.Repeat("x", MaxPublicationBytes), Port: 1}}
	if _, err := EncodePublication(v); !errors.Is(err, TooLarge) {
		t.Fatal("encoded byte limit", err)
	}
}

func TestUnknownFieldsAndExactNames(t *testing.T) {
	b := fixture(t, "publication.json")

	b = bytes.Replace(b, []byte(`"schema_version":1`), []byte(`"schema_version":1,"SCHEMA_VERSION":42`), 1)
	if _, err := DecodePublication(bytes.NewReader(b)); err == nil {
		t.Fatal("unknown case variant accepted")
	}

	b = bytes.Replace(b, []byte(`"schema_version":1,`), nil, 1)
	if _, err := DecodePublication(bytes.NewReader(b)); err == nil {
		t.Fatal("case variant supplied required field")
	}
}

func TestValidatedOriginalBytesPreserveEscapes(t *testing.T) {
	for _, name := range []string{"publication.json", "bootstrap-request.json", "bootstrap-response.json", "bundle.json"} {
		t.Run(name, func(t *testing.T) {
			original := fixture(t, name)

			want, err := vectorRoundTrip(name, original)
			if err != nil {
				t.Fatal(err)
			}

			// Escaped exact field names and scalar text are valid, but escaped
			// aliases must still be rejected as duplicates before typed decoding.
			escaped := bytes.ReplaceAll(original, []byte(`"schema_version"`), []byte(`"schema_versi\u006fn"`))
			escaped = bytes.ReplaceAll(escaped, []byte(`"18446744073709551615"`), []byte(`"\u00318446744073709551615"`))
			escaped = bytes.ReplaceAll(escaped, []byte(`11111111-`), []byte(`\u00311111111-`))

			got, err := vectorRoundTrip(name, escaped)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("escaped document changed semantics: %v", err)
			}
		})
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
	if key.Key.ID[0] != 'R' {
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

// Fixtures and public certificate inputs.

func fixture(t *testing.T, name string) []byte {
	t.Helper()

	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)

	return bytes.TrimSuffix(b, []byte{'\n'})
}

func TestSharedPublicationVector(t *testing.T) {
	v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	require.NoError(t, err)
	candidate, err := NewCanonicalCandidate(v)
	require.NoError(t, err)
	p, m, err := candidate.canonicalContent()
	require.NoError(t, err)
	require.Equal(t, fixture(t, "content.json"), p)
	require.Equal(t, fixture(t, "membership.json"), m)

	ph, mh, err := ContentHashes(v)
	require.NoError(t, err)

	var hashes struct{ Content, Membership string }
	require.NoError(t, json.Unmarshal(fixture(t, "hashes.json"), &hashes))
	require.Equal(t, hashes.Content, ph)
	require.Equal(t, hashes.Membership, mh)

	encoded, err := EncodePublication(v)
	require.NoError(t, err)
	replay, err := DecodePublication(bytes.NewReader(encoded))
	require.NoError(t, err)
	again, err := EncodePublication(replay)
	require.NoError(t, err)
	require.Equal(t, encoded, again, "unstable round trip")
}

// The key is ephemeral and never printed or persisted. Wire validation checks
// DER syntax, not trust or CSR authority.
func publicDER(t *testing.T) (cert, csr []byte) {
	t.Helper()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "wire fixture"}, NotBefore: time.Unix(0, 0), NotAfter: time.Unix(2000000000, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	cert, err = x509.CreateCertificate(rand.Reader, template, template, pub, key)
	require.NoError(t, err)
	csr, err = x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: template.Subject}, key)
	require.NoError(t, err)

	return cert, csr
}

func TestPublicDEREncoding(t *testing.T) {
	cert, csr := publicDER(t)
	cluster := ClusterID("11111111-1111-4111-8111-111111111111")
	enrollment := EnrollmentID("55555555-5555-4555-8555-555555555555")
	request, err := EncodeBootstrapRequest(BootstrapRequest{Shares: DefaultShares, SchemaVersion: 1, Cluster: cluster, Enrollment: enrollment, CSRDER: csr})
	require.NoError(t, err)
	_, err = DecodeBootstrap(bytes.NewReader(request))
	require.NoError(t, err)
	response, err := EncodeBootstrap(BootstrapResponse{SchemaVersion: 1, Cluster: cluster, Enrollment: enrollment, Node: "22222222-2222-4222-8222-222222222222", CertificateChain: [][]byte{cert}})
	require.NoError(t, err)
	_, err = DecodeBootstrapResponse(bytes.NewReader(response))
	require.NoError(t, err)
	_, err = json.Marshal(KeyringBundle{})
	require.Error(t, err, "invalid bundle accepted")
}

// Bootstrap bounds include JSON framing and base64 expansion.

func TestBootstrapRequestEncodedBoundary(t *testing.T) {
	request, err := DecodeBootstrap(bytes.NewReader(fixture(t, "bootstrap-request.json")))
	require.NoError(t, err)

	request.CSRDER = []byte{}
	request.RDMANICs = []RDMANIC{{Device: "mlx5_0", Port: 1, Rail: 1, GID: "abcdef0123456789abcdef0123456789"}}
	framing, err := json.Marshal(request)
	require.NoError(t, err)

	maxDER := (MaxBootstrapBytes - len(framing)) / 4 * 3
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	makeCSR := func(padding int) []byte {
		t.Helper()

		der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: strings.Repeat("x", padding)}}, key)
		require.NoError(t, err)

		return der
	}
	padding := maxDER - 256
	padding += maxDER - len(makeCSR(padding))
	// Cover every base64 padding case and the first DER byte that overflows.
	for _, delta := range []int{-2, -1, 0, 1} {
		t.Run(fmt.Sprint(delta), func(t *testing.T) {
			request.CSRDER = makeCSR(padding + delta)
			require.Len(t, request.CSRDER, maxDER+delta)
			raw, err := json.Marshal(request)
			require.NoError(t, err)

			validationErr := ValidateBootstrapRequest(request)
			encoded, encodeErr := EncodeBootstrapRequest(request)

			_, decodeErr := DecodeBootstrap(bytes.NewReader(raw))
			if delta == 1 {
				require.Greater(t, len(raw), MaxBootstrapBytes)
				require.Less(t, len(request.CSRDER), MaxBootstrapBytes)
				require.ErrorIs(t, validationErr, TooLarge)
				require.ErrorIs(t, encodeErr, TooLarge)
				require.ErrorIs(t, decodeErr, TooLarge)
				require.Nil(t, encoded)

				return
			}

			require.Less(t, MaxBootstrapBytes-len(raw), 4)
			require.NoError(t, validationErr)
			require.NoError(t, encodeErr)
			require.NoError(t, decodeErr)
			require.Equal(t, raw, encoded)
		})
	}
}

func TestBootstrapResponseEncodedBoundary(t *testing.T) {
	response, err := DecodeBootstrapResponse(bytes.NewReader(fixture(t, "bootstrap-response.json")))
	require.NoError(t, err)

	cert := response.CertificateChain[0]
	response.CertificateChain = nil

	var previous []byte

	for {
		response.CertificateChain = append(response.CertificateChain, cert)
		raw, err := json.Marshal(response)
		require.NoError(t, err)

		encoded, encodeErr := EncodeBootstrap(response)
		if len(raw) <= MaxBootstrapBytes {
			require.NoError(t, encodeErr)

			previous = encoded

			continue
		}

		require.Less(t, len(response.CertificateChain)*len(cert), MaxBootstrapBytes, "fixture must overflow only after encoding")
		require.ErrorIs(t, encodeErr, TooLarge)
		require.Nil(t, encoded)

		_, err = DecodeBootstrapResponse(bytes.NewReader(raw))
		require.ErrorIs(t, err, TooLarge)
		_, err = DecodeBootstrapResponse(bytes.NewReader(previous))
		require.NoError(t, err, "last fitting response")

		break
	}
}

// Bundle identity, generation, and material isolation.

func TestKeyringDeliveryEncodingBoundsAndGeneration(t *testing.T) {
	bundle, err := DecodeBundle(bytes.NewReader(fixture(t, "bundle.json")))
	require.NoError(t, err)

	bundle.Generation = ^Generation(0)
	encoded, err := EncodeBundle(bundle)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"generation":"18446744073709551615"`)
	decoded, err := DecodeBundle(bytes.NewReader(encoded))
	require.NoError(t, err)
	require.Equal(t, bundle.Generation, decoded.Generation)
	require.True(t, decoded.CacheKeys[0].EqualMaterial(bundle.CacheKeys[0]))

	for n := uint64(1); n <= 4000; n++ {
		ref := bundle.CacheKeys[0].Key
		ref.ID = make([]byte, 16)
		copy(ref.ID, "RKG1")
		binary.BigEndian.PutUint64(ref.ID[4:12], n+10)

		var material [32]byte
		binary.BigEndian.PutUint64(material[:8], n+10)
		key, err := NewCacheKey(ref, PreparedKey, material)
		require.NoError(t, err)

		bundle.CacheKeys = append(bundle.CacheKeys, key)
	}

	encoded, err = EncodeBundle(bundle)
	require.ErrorIs(t, err, TooLarge)
	require.Nil(t, encoded)
}

func TestBundleEncodingCannotBypassCodec(t *testing.T) {
	_, err := json.Marshal(KeyringBundle{})
	require.ErrorIs(t, err, UnsupportedVersion)
}

// Rust rejects material shared by distinct refs regardless of scope or state.
func TestBundleDuplicateMaterialParity(t *testing.T) {
	for _, scope := range []string{"id", "purpose", "cache"} {
		t.Run(scope, func(t *testing.T) {
			bundle, err := DecodeBundle(bytes.NewReader(fixture(t, "bundle.json")))
			require.NoError(t, err)

			first := bundle.CacheKeys[0]
			second := first
			second.Key.ID = bytes.Clone(first.Key.ID)

			switch scope {
			case "id":
				second.Key.ID[15]++
				second.State = PreparedKey
			case "purpose":
				second.Key.Purpose = OriginCredentialsKey
				if first.Key.Purpose == OriginCredentialsKey {
					second.Key.Purpose = PageKey
				}
			case "cache":
				second.Key.Cache = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			}

			second.material[0] ^= 0xff
			bundle.CacheKeys = []CacheKey{first, second}
			valid, err := EncodeBundle(bundle)
			require.NoError(t, err, "distinct material rejected")

			var document bundleJSON
			require.NoError(t, json.Unmarshal(valid, &document))
			document.CacheKeys[1].Material = document.CacheKeys[0].Material
			corrupt, err := json.Marshal(document)
			require.NoError(t, err)
			_, err = DecodeBundle(bytes.NewReader(corrupt))
			require.ErrorIs(t, err, InvalidRequest, "decoder accepted duplicate material")

			bundle.CacheKeys[1].material = first.material
			_, err = EncodeBundle(bundle)
			require.ErrorIs(t, err, InvalidRequest, "encoder accepted duplicate material")
		})
	}
}

func TestRetiringKeyStateIsRejected(t *testing.T) {
	raw := fixture(t, "bundle.json")
	// Leave an active key so this checks the enum, not the active-key count.
	changed := bytes.Replace(raw, []byte(`"state":"prepared"`), []byte(`"state":"retiring"`), 1)
	require.NotEqual(t, raw, changed, "mutation did not match")
	_, err := DecodeBundle(bytes.NewReader(changed))
	require.ErrorIs(t, err, InvalidRequest)
	bundle, err := DecodeBundle(bytes.NewReader(raw))
	require.NoError(t, err)
	_, err = NewCacheKey(bundle.CacheKeys[0].Key, KeyState("retiring"), [32]byte{})
	require.ErrorIs(t, err, InvalidRequest)

	bundle.CacheKeys[1].State = KeyState("retiring")
	_, err = EncodeBundle(bundle)
	require.ErrorIs(t, err, InvalidRequest)
}

func TestKeyIDsRequireCurrentNamespaceAndValidGeneration(t *testing.T) {
	bundle, err := DecodeBundle(bytes.NewReader(fixture(t, "bundle.json")))
	require.NoError(t, err)

	bundle.Generation = 2
	for _, generation := range []uint64{0, 1, 2, 3} {
		ref := bundle.CacheKeys[0].Key
		ref.ID = bytes.Clone(ref.ID)
		binary.BigEndian.PutUint64(ref.ID[4:12], generation)

		key, err := NewCacheKey(ref, ActiveKey, [32]byte{})
		if generation == 0 {
			require.Error(t, err, "zero generation accepted")
			continue
		}

		require.NoError(t, err)

		candidate := bundle
		candidate.CacheKeys = []CacheKey{key}
		_, err = EncodeBundle(candidate)
		require.Equal(t, generation <= 2, err == nil, "generation %d: %v", generation, err)
	}

	for _, id := range [][]byte{make([]byte, 16), []byte("RKG0abcdefgh1234"), []byte("RKG1")} {
		ref := bundle.CacheKeys[0].Key
		ref.ID = id
		_, err := NewCacheKey(ref, ActiveKey, [32]byte{})
		require.Error(t, err, "legacy ID accepted")
	}
}

func TestKeyDiagnosticsRedactMaterial(t *testing.T) {
	key := CacheKey{material: [32]byte{1, 2, 3}}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		require.Equal(t, "<redacted cache key>", fmt.Sprintf(format, key))
	}

	encoded, err := json.Marshal(key)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "material")
}

// Canonical candidates own their input and output independently of callers.

func TestCanonicalCandidateEquivalence(t *testing.T) {
	vector, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	require.NoError(t, err)

	vector.Caches = append(vector.Caches, CacheDefinition{ID: "00000000-0000-4000-8000-000000000000", Name: "cache-b", ClientSocket: "/run/racer/cache-b/client/socket", OriginSocket: "/run/racer/cache-b/origin/socket"})
	slices.Reverse(vector.Members)

	for _, v := range []Publication{
		vector,
		{SchemaVersion: SchemaVersion, Cluster: vector.Cluster},
		{SchemaVersion: SchemaVersion, Cluster: vector.Cluster, Members: []Member{}, Caches: []CacheDefinition{}},
	} {
		before, err := json.Marshal(v)
		require.NoError(t, err)
		candidate, err := NewCanonicalCandidate(v)
		require.NoError(t, err)
		wantContent, wantMembership, err := ContentHashes(v)
		require.NoError(t, err)

		sequence, membershipVersion := v.Sequence, v.MembershipVersion
		for _, counters := range [][2]uint64{{1, 1}, {10, 9}, {1, 2}, {^uint64(0), ^uint64(0)}} {
			v.Sequence, v.MembershipVersion = Sequence(counters[0]), MembershipVersion(counters[1])
			want, err := EncodePublication(v)
			require.NoError(t, err)
			got, err := candidate.EncodePublication(v.Sequence, v.MembershipVersion)
			require.NoError(t, err)
			require.Equal(t, want, got, "counters %v", counters)

			content, membership, err := candidate.ContentHashes()
			require.NoError(t, err)
			require.Equal(t, wantContent, content)
			require.Equal(t, wantMembership, membership)
		}

		v.Sequence, v.MembershipVersion = sequence, membershipVersion
		after, err := json.Marshal(v)
		require.NoError(t, err)
		require.Equal(t, before, after, "candidate mutated input")
	}
}

func TestCanonicalCandidateOwnsNestedStateAndOutput(t *testing.T) {
	v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	require.NoError(t, err)
	want, err := EncodePublication(v)
	require.NoError(t, err)
	candidate, err := NewCanonicalCandidate(v)
	require.NoError(t, err)
	// Mutate all caller collections, including the nested pointer, before use.
	*v.Members[1].RDMANICs[1].NUMANode = 7
	v.Members[1].RDMANICs[0].Device = "changed"
	v.Members[0].Shares = 0
	v.Caches[0].Name = "changed"
	clear(v.Members)
	clear(v.Caches)
	got, err := candidate.EncodePublication(v.Sequence, v.MembershipVersion)
	require.NoError(t, err)
	require.Equal(t, want, got)

	var hashes struct{ Content, Membership string }
	require.NoError(t, json.Unmarshal(fixture(t, "hashes.json"), &hashes))

	content, membership, err := candidate.ContentHashes()
	require.NoError(t, err)
	require.Equal(t, hashes.Content, content)
	require.Equal(t, hashes.Membership, membership)
	clear(got)
	got, err = candidate.EncodePublication(v.Sequence, v.MembershipVersion)
	require.NoError(t, err)
	require.Equal(t, want, got, "encoding retained returned bytes")
}

func TestCanonicalCandidateValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Publication)
		want error
	}{
		{"schema", func(v *Publication) { v.SchemaVersion++ }, UnsupportedVersion},
		{"cluster", func(v *Publication) { v.Cluster = "invalid" }, InvalidRequest},
		{"node", func(v *Publication) { v.Members[0].Node = "invalid" }, InvalidRequest},
		{"duplicate node", func(v *Publication) { v.Members = append(v.Members, v.Members[0]) }, InvalidRequest},
		{"shares", func(v *Publication) { v.Members[0].Shares = 0 }, InvalidRequest},
		{"endpoint", func(v *Publication) { v.Members[0].PeerEndpoint = "192.0.2.1:0" }, InvalidRequest},
		{"duplicate physical NIC", func(v *Publication) { v.Members[1].RDMANICs = append(v.Members[1].RDMANICs, v.Members[1].RDMANICs[0]) }, InvalidRequest},
		{"device", func(v *Publication) { v.Members[1].RDMANICs[0].Device = "\xff" }, InvalidRequest},
		{"duplicate cache", func(v *Publication) { v.Caches = append(v.Caches, v.Caches[0]) }, InvalidRequest},
		{"socket", func(v *Publication) { v.Caches[0].ClientSocket += "x" }, InvalidRequest},
		{"member limit", func(v *Publication) { v.Members = make([]Member, MaxMembers+1) }, TooLarge},
		{"byte lower bound", func(v *Publication) { v.Members[1].RDMANICs[0].Device = strings.Repeat("x", MaxPublicationBytes) }, TooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
			require.NoError(t, err)
			tc.edit(&v)
			candidate, err := NewCanonicalCandidate(v)
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, CanonicalCandidate{}, candidate)

			_, err = EncodePublication(v)
			require.ErrorIs(t, err, tc.want)
		})
	}

	var zero CanonicalCandidate

	p, m, err := zero.ContentHashes()
	require.ErrorIs(t, err, UnsupportedVersion)
	require.Empty(t, p)
	require.Empty(t, m)

	b, err := zero.EncodePublication(1, 1)
	require.ErrorIs(t, err, UnsupportedVersion)
	require.Nil(t, b)

	candidate, err := NewCanonicalCandidate(Publication{SchemaVersion: SchemaVersion, Cluster: "11111111-1111-4111-8111-111111111111"})
	require.NoError(t, err)

	for _, counters := range [][2]uint64{{0, 0}, {0, 1}, {1, 0}} {
		b, err := candidate.EncodePublication(Sequence(counters[0]), MembershipVersion(counters[1]))
		require.ErrorIs(t, err, InvalidRequest)
		require.Nil(t, b)
	}
}

func TestCanonicalCandidateFinalByteBound(t *testing.T) {
	v := Publication{
		SchemaVersion: SchemaVersion, Cluster: "11111111-1111-4111-8111-111111111111", Sequence: 1, MembershipVersion: 1,
		Members: []Member{{Node: "22222222-2222-4222-8222-222222222222", Shares: 1, PeerEndpoint: "192.0.2.1:1", RDMANICs: []RDMANIC{{Device: "x", Port: 1}}}},
	}
	b, err := EncodePublication(v)
	require.NoError(t, err)
	// Tabs expand to two bytes but remain below the cheap input bound.
	padding := MaxPublicationBytes - len(b)
	v.Members[0].RDMANICs[0].Device += strings.Repeat("\t", padding/2) + strings.Repeat("x", padding%2)
	candidate, err := NewCanonicalCandidate(v)
	require.NoError(t, err)
	_, _, err = candidate.ContentHashes()
	require.NoError(t, err, "counter-free content fits")
	b, err = candidate.EncodePublication(1, 1)
	require.NoError(t, err)
	require.Len(t, b, MaxPublicationBytes)

	for _, counters := range [][2]uint64{{10, 1}, {1, 10}, {^uint64(0), ^uint64(0)}} {
		b, err := candidate.EncodePublication(Sequence(counters[0]), MembershipVersion(counters[1]))
		require.ErrorIs(t, err, TooLarge)
		require.Nil(t, b)
	}

	v.Members[0].RDMANICs[0].Device += strings.Repeat("\t", 100)
	candidate, err = NewCanonicalCandidate(v)
	require.NoError(t, err)
	p, m, err := candidate.ContentHashes()
	require.ErrorIs(t, err, TooLarge)
	require.Empty(t, p)
	require.Empty(t, m)
}

// Token validation must reject bad shapes without traversing their contents.

func TestTokenValidationRejectsBeforeDescending(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, suffix string
		typ                  reflect.Type
	}{
		{"unknown field", `{"unknown"`, `:[{"ignored":[]}]}`, reflect.TypeFor[Publication]()},
		{"case variant", `{"Schema_version"`, `:[{}]}`, reflect.TypeFor[Publication]()},
		{"escaped duplicate", `{"schema_version":1,"schema_versi\u006fn"`, `:[{}]}`, reflect.TypeFor[Publication]()},
		{"wrong root", `[`, `{"ignored":[]}]`, reflect.TypeFor[Publication]()},
		{"wrong collection", `{"members":{`, `"ignored":[]}}`, reflect.TypeFor[Publication]()},
		{"wrong element", `{"members":[[`, `{"ignored":[]}]]}`, reflect.TypeFor[Publication]()},
		{"wrong primitive", `{"schema_version":[`, `{"ignored":[]}]}`, reflect.TypeFor[Publication]()},
		{"wrong bytes", `{"csr_der":[`, `{"ignored":[]}]}`, reflect.TypeFor[BootstrapRequest]()},
		{"nested unknown", `[{"unknown"`, `:[{}]}]`, reflect.TypeFor[[]RDMANIC]()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := json.NewDecoder(strings.NewReader(tc.prefix + tc.suffix))
			d.UseNumber()
			require.ErrorIs(t, checkValue(d, tc.typ, false, 0), InvalidRequest)
			// InputOffset counts consumed tokens, not decoder read-ahead.
			require.Equal(t, int64(len(tc.prefix)), d.InputOffset(), "traversed rejected subtree")
		})
	}
}

func TestTokenValidationCollectionLimitsBeforeNextElement(t *testing.T) {
	for _, tc := range []struct {
		name, element string
		typ           reflect.Type
		limit         int
	}{
		{"members", `{"node":"","shares":0,"peer_endpoint":"","rdma_nics":[],"site":""}`, reflect.TypeFor[[]Member](), MaxMembers},
		{"NICs", `{"device":"a","port":1,"rail":0}`, reflect.TypeFor[[]RDMANIC](), MaxRDMANICs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := "[" + strings.Repeat(tc.element+",", tc.limit-1) + tc.element
			for _, suffix := range []string{"]", `,{"unvisited":[{}]}]`} {
				d := json.NewDecoder(strings.NewReader(prefix + suffix))
				d.UseNumber()

				err := checkValue(d, tc.typ, false, 0)
				if suffix == "]" {
					require.NoError(t, err, "exact limit")
					continue
				}

				require.ErrorIs(t, err, TooLarge)
				require.Equal(t, int64(len(prefix)), d.InputOffset(), "consumed excess element")
			}
		})
	}
}

func TestTokenValidationPrimitiveContract(t *testing.T) {
	type primitives struct {
		Count uint64  `json:"count,string"`
		Byte  uint8   `json:"byte"`
		Flag  bool    `json:"flag"`
		Text  string  `json:"text"`
		Bytes []byte  `json:"bytes"`
		Opt   *uint32 `json:"opt,omitempty"`
	}

	const valid = `{"count":"18446744073709551615","byte":255,"flag":true,"text":"\ud83d\ude00","bytes":"AA=="}`
	for _, tc := range []struct{ old, replacement string }{
		{`"count":"18446744073709551615"`, `"count":"18446744073709551616"`},
		{`"count":"18446744073709551615"`, `"count":1`},
		{`"count":"18446744073709551615"`, `"count":"01"`},
		{`"count":"18446744073709551615"`, `"count":"+1"`},
		{`255`, `256`},
		{`255`, `-0`},
		{`255`, `1.0`},
		{`255`, `1e0`},
		{`255`, `"1"`},
		{`true`, `1`},
		{`true`, `null`},
		{`"\ud83d\ude00"`, `"\ud83d"`},
		{`"\ud83d\ude00"`, `"\ude00"`},
		{`"\ud83d\ude00"`, `null`},
		{`"\ud83d\ude00"`, `"` + "\xff" + `"`},
		{`"AA=="`, `"AB=="`},
		{`"AA=="`, `"AA"`},
		{`"AA=="`, `"AA==\n"`},
		{`"AA=="`, `null`},
		{`"AA=="`, `[]`},
		{`"flag":true,`, ``},
		{`"bytes":"AA=="`, `"bytes":"AA==","opt":null`},
		{`"bytes":"AA=="`, `"bytes":"AA==","opt":4294967296`},
	} {
		raw := strings.Replace(valid, tc.old, tc.replacement, 1)

		var v primitives
		require.ErrorIs(t, decode(strings.NewReader(raw), 4096, &v), InvalidRequest, "%s", raw)
	}

	for _, raw := range []string{valid, `{"bytes":"AA==","text":"\ud83d\ude00","flag":true,"byte":255,"count":"18446744073709551615","opt":0}`} {
		var v primitives
		require.NoError(t, decode(strings.NewReader(raw), 4096, &v))
		require.Equal(t, ^uint64(0), v.Count)
		require.Equal(t, "😀", v.Text)

		for _, suffix := range []string{`{}`, `null`, `!`} {
			require.ErrorIs(t, decode(strings.NewReader(raw+suffix), 4096, &v), InvalidRequest, "%s", suffix)
		}
	}
}

func TestTokenValidationDepthBoundary(t *testing.T) {
	for _, depth := range []int{64, 65} {
		typ := reflect.TypeFor[string]()
		for range depth {
			typ = reflect.SliceOf(typ)
		}

		raw := strings.Repeat("[", depth) + `"leaf"` + strings.Repeat("]", depth)

		err := decode(strings.NewReader(raw), 4096, reflect.New(typ).Interface())
		if depth == 64 {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, InvalidRequest)
		}
	}
}

// NIC reports, persisted member migration, and Site boundaries.

func TestRDMANICStrictFieldsAndBounds(t *testing.T) {
	valid := `{"device":"mlx5_0","port":1,"rail":0}`
	for _, bad := range []string{
		`{"port":1,"rail":0}`, `{"device":"a","rail":0}`, `{"device":"a","port":1}`,
		strings.Replace(valid, `"port":1`, `"port":0`, 1),
		strings.Replace(valid, `"port":1`, `"port":256`, 1),
		strings.Replace(valid, `"port":1`, `"port":1.0`, 1),
		strings.Replace(valid, `"port":1`, `"port":"1"`, 1),
		strings.Replace(valid, `"port":1`, `"port":-1`, 1),
		strings.Replace(valid, `"port":1`, `"port":1,"port":2`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":65536`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"gid":""`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"gid":null`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"gid":"ABCDEF0123456789abcdef0123456789"`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"gid":"0123"`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"gid":"gggggggggggggggggggggggggggggggg"`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"fabric":"old"`, 1),
	} {
		t.Run(bad, func(t *testing.T) {
			_, err := DecodeRDMANICs(strings.NewReader("[" + bad + "]"))
			require.ErrorIs(t, err, InvalidRequest)
		})
	}

	nics, err := DecodeRDMANICs(strings.NewReader(`[{"device":"mlx5_0","port":255,"rail":65535,"gid":"abcdef0123456789abcdef0123456789","numa_node":4294967295}]`))
	require.NoError(t, err)
	require.Equal(t, uint8(255), nics[0].Port)

	for _, count := range []int{64, 65} {
		nics := make([]RDMANIC, count)
		for i := range nics {
			nics[i] = RDMANIC{Device: fmt.Sprintf("mlx5_%d", i), Port: 1}
		}

		raw, err := json.Marshal(nics)
		require.NoError(t, err)

		_, err = DecodeRDMANICs(bytes.NewReader(raw))
		if count == 64 {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, TooLarge)
		}
	}
}

func TestBootstrapRDMANICRequiredCanonicalAndBounded(t *testing.T) {
	request, err := DecodeBootstrap(bytes.NewReader(fixture(t, "bootstrap-request.json")))
	require.NoError(t, err)

	request.RDMANICs = []RDMANIC{{Device: "b", Port: 2, Rail: 1}, {Device: "a", Port: 1, Rail: 1}}
	raw, err := EncodeBootstrapRequest(request)
	require.NoError(t, err)
	decoded, err := DecodeBootstrap(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, "a", decoded.RDMANICs[0].Device)
	require.Equal(t, "b", request.RDMANICs[0].Device)

	for _, replacement := range []string{`"rails":`, `"Rdma_nics":`} {
		_, err := DecodeBootstrap(bytes.NewReader(bytes.Replace(raw, []byte(`"rdma_nics":`), []byte(replacement), 1)))
		require.ErrorIs(t, err, InvalidRequest)
	}

	request.RDMANICs = append(request.RDMANICs, request.RDMANICs[0])
	_, err = EncodeBootstrapRequest(request)
	require.ErrorIs(t, err, InvalidRequest)

	request.RDMANICs = []RDMANIC{{Device: strings.Repeat("x", MaxBootstrapBytes), Port: 1}}
	require.ErrorIs(t, ValidateBootstrapRequest(request), TooLarge)
	request.RDMANICs = make([]RDMANIC, 65)
	require.ErrorIs(t, ValidateBootstrapRequest(request), TooLarge)
	request.RDMANICs = nil
	raw, err = EncodeBootstrapRequest(request)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"rdma_nics":[]`)
	_, err = DecodeBootstrap(bytes.NewReader(bytes.Replace(raw, []byte(`"rdma_nics":[],`), nil, 1)))
	require.ErrorIs(t, err, InvalidRequest)
}

func TestAdmittedMemberMigrationAndHardBreak(t *testing.T) {
	legacy := `{"node":"22222222-2222-4222-8222-222222222222","shares":4,"peer_endpoint":"192.0.2.1:7443","rails":[{"rail":0,"fabric":"old"}],"alignment_enabled":false,"site":""}`
	member, err := DecodeAdmittedMember(strings.NewReader(legacy))
	require.ErrorIs(t, err, InvalidRequest)
	require.Zero(t, member)
	member = Member{Node: "22222222-2222-4222-8222-222222222222", Shares: 4, PeerEndpoint: "192.0.2.1:7443", RDMANICs: []RDMANIC{{Device: "a", Port: 1}}}
	raw, err := json.Marshal(member)
	require.NoError(t, err)
	_, err = DecodeAdmittedMember(bytes.NewReader(raw))
	require.NoError(t, err)

	for _, bad := range []string{
		strings.Replace(string(raw), `"port":1`, `"port":0`, 1),
		strings.Replace(string(raw), `"port":1`, `"port":1,"port":2`, 1),
		strings.Replace(string(raw), `"rdma_nics":[{"device":"a","port":1,"rail":0}]`, `"rdma_nics":null`, 1),
		strings.Replace(string(raw), `"device":"a"`, `"device":"a","unknown":1`, 1),
	} {
		_, err = DecodeAdmittedMember(strings.NewReader(bad))
		require.ErrorIs(t, err, InvalidRequest)
	}

	_, err = DecodePublication(strings.NewReader(`{"schema_version":1,"cluster":"11111111-1111-4111-8111-111111111111","sequence":"1","membership_version":"1","members":[` + legacy + `],"caches":[]}`))
	require.ErrorIs(t, err, InvalidRequest)
}

func TestMemberSiteWireValidation(t *testing.T) {
	for _, site := range []string{"", "a", "0", "Site_1.a-b", strings.Repeat("a", 63)} {
		t.Run("valid/"+site, func(t *testing.T) {
			v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
			require.NoError(t, err)

			v.Members[0].Site = site
			encoded, err := EncodePublication(v)
			require.NoError(t, err)

			if site == "" {
				require.Contains(t, string(encoded), `"site":""`)
			} else {
				require.Contains(t, string(encoded), `"rdma_nics":[],"site":"`+site+`"`)
			}

			decoded, err := DecodePublication(bytes.NewReader(encoded))
			require.NoError(t, err)
			require.Equal(t, v, decoded)
		})
	}

	for _, site := range []string{strings.Repeat("a", 64), "-a", "a-", ".a", "a.", "_a", "a_", "a/b", "a b", "a\n", "a\x00", "β", "\xff"} {
		t.Run("invalid/"+site, func(t *testing.T) {
			v, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
			require.NoError(t, err)

			v.Members[0].Site = site
			_, err = EncodePublication(v)
			require.ErrorIs(t, err, InvalidRequest)
			_, err = NewCanonicalCandidate(v)
			require.ErrorIs(t, err, InvalidRequest)
			encoded, err := json.Marshal(v)
			require.NoError(t, err)
			_, err = DecodePublication(bytes.NewReader(encoded))
			require.ErrorIs(t, err, InvalidRequest)
		})
	}
}

func TestMemberSiteJSONShape(t *testing.T) {
	for _, value := range []string{`null`, `1`, `true`, `[]`, `{}`, `"a","site":"b"`} {
		raw := strings.Replace(string(fixture(t, "publication.json")), `"site":""`, `"site":`+value, 1)
		_, err := DecodePublication(strings.NewReader(raw))
		require.ErrorIs(t, err, InvalidRequest, "%s", value)
	}

	raw := string(fixture(t, "publication.json"))
	v, err := DecodePublication(strings.NewReader(raw))
	require.NoError(t, err)
	encoded, err := EncodePublication(v)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"site":""`)
}

func TestMemberSiteDeltaValidation(t *testing.T) {
	base, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	require.NoError(t, err)

	base.Sequence, base.MembershipVersion = 1, 1
	next := canonicalPublication(base)
	next.Sequence, next.MembershipVersion = 2, 2
	next.Members[0].Site = "site-a"
	encoded, err := EncodeDelta(base, next)
	require.NoError(t, err)
	applied, err := ApplyDelta(base, bytes.NewReader(encoded))
	require.NoError(t, err)
	require.Equal(t, next, applied)

	for _, site := range []string{"-invalid", "site-b"} {
		bad := strings.Replace(string(encoded), `"site":"site-a"`, `"site":"`+site+`"`, 1)

		_, err := ApplyDelta(base, strings.NewReader(bad))
		if site == "-invalid" {
			require.ErrorIs(t, err, InvalidRequest)
		} else {
			require.ErrorIs(t, err, Conflict)
		}
	}
}

func TestDeltaAddRemoveUpdateAndRejectedBase(t *testing.T) {
	base := Publication{
		SchemaVersion: 1, Cluster: "11111111-1111-4111-8111-111111111111", Sequence: 1, MembershipVersion: 1,
		Members: []Member{
			{Node: "22222222-2222-4222-8222-222222222222", Shares: 4, PeerEndpoint: "127.0.0.1:7443", RDMANICs: []RDMANIC{}},
			{Node: "33333333-3333-4333-8333-333333333333", Shares: 4, PeerEndpoint: "127.0.0.2:7443", RDMANICs: []RDMANIC{}},
		}, Caches: []CacheDefinition{},
	}
	next := base
	next.Sequence, next.MembershipVersion = 2, 2
	next.Members = []Member{base.Members[0], {Node: "44444444-4444-4444-8444-444444444444", Shares: 7, PeerEndpoint: "127.0.0.4:7443", RDMANICs: []RDMANIC{}}}
	next.Members[0].Shares = 9
	encoded, err := EncodeDelta(base, next)
	require.NoError(t, err)

	if os.Getenv("RACER_UPDATE_SITE_VECTORS") == "1" {
		require.NoError(t, os.WriteFile("testdata/delta.json", append(bytes.Clone(encoded), '\n'), 0o644))
	}

	golden, err := os.ReadFile("testdata/delta.json")
	require.NoError(t, err)
	require.Equal(t, bytes.TrimSpace(golden), encoded)
	got, err := ApplyDelta(base, bytes.NewReader(encoded))
	require.NoError(t, err)
	a, err := EncodePublication(got)
	require.NoError(t, err)
	b, err := EncodePublication(next)
	require.NoError(t, err)
	require.Equal(t, b, a)

	_, err = ApplyDelta(next, bytes.NewReader(encoded))
	require.Error(t, err, "replay accepted")

	bad := strings.Replace(string(encoded), `"shares":9`, `"shares":8`, 1)
	_, err = ApplyDelta(base, strings.NewReader(bad))
	require.Error(t, err, "bad target hash accepted")

	wrong := base
	wrong.Sequence = 3
	_, err = ApplyDelta(wrong, bytes.NewReader(encoded))
	require.Error(t, err, "wrong base accepted")
}

func TestDeltaValidationFailures(t *testing.T) {
	base, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	require.NoError(t, err)

	base.Sequence, base.MembershipVersion = 1, 2
	next := canonicalPublication(base)
	next.Sequence = 2
	encoded, err := EncodeDelta(base, next)
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		edit func(*Delta)
		want error
	}{
		{"version", func(d *Delta) { d.DeltaVersion++ }, Conflict},
		{"cluster", func(d *Delta) { d.Cluster = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" }, Conflict},
		{"sequence", func(d *Delta) { d.Sequence = base.Sequence }, Conflict},
		{"membership rollback", func(d *Delta) { d.MembershipVersion = 1 }, Conflict},
		{"unknown removal", func(d *Delta) { d.RemoveMembers = []NodeID{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"} }, InvalidRequest},
		{"duplicate removal", func(d *Delta) { d.RemoveMembers = []NodeID{base.Members[0].Node, base.Members[0].Node} }, InvalidRequest},
		{"duplicate upsert", func(d *Delta) { d.UpsertMembers = []Member{base.Members[0], base.Members[0]} }, InvalidRequest},
		{"remove and upsert", func(d *Delta) {
			d.RemoveMembers = []NodeID{base.Members[0].Node}
			d.UpsertMembers = []Member{base.Members[0]}
		}, InvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var delta Delta
			require.NoError(t, json.Unmarshal(encoded, &delta))
			tc.edit(&delta)
			raw, err := json.Marshal(delta)
			require.NoError(t, err)
			got, err := ApplyDelta(base, bytes.NewReader(raw))
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, Publication{}, got, "no partial state on error")
		})
	}

	for _, tc := range []struct {
		name       string
		base, next Publication
		want       error
	}{
		{"invalid base", Publication{}, next, UnsupportedVersion},
		{"invalid next", base, Publication{}, UnsupportedVersion},
		{"no advance", base, base, Conflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := EncodeDelta(tc.base, tc.next)
			require.ErrorIs(t, err, tc.want)
			require.Nil(t, raw)
		})
	}

	got, err := ApplyDelta(Publication{}, bytes.NewReader(encoded))
	require.ErrorIs(t, err, UnsupportedVersion)
	require.Equal(t, Publication{}, got)
	got, err = ApplyDelta(base, strings.NewReader("{"))
	require.ErrorIs(t, err, InvalidRequest)
	require.Equal(t, Publication{}, got)
}

func TestUnicodeEscapeValidation(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`"\u0041"`, true},
		{`"\ud800\udc00"`, true},
		{`"\udbff\udfff"`, true},
		{`"\\ud800"`, true},
		{`"\"\u0041"`, true},
		{`"\`, false},
		{`"\u12`, false},
		{`"\uxxxx"`, false},
		{`"\ud800\u0041"`, false},
		{`"\ud800\uxxxx"`, false},
		{`"\ud800abcdef"`, false},
		{`"\udfff"`, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			require.Equal(t, tc.valid, validSurrogates([]byte(tc.raw)))
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestAdmittedMemberReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader io.Reader
		want   error
	}{
		{"read error", failingReader{}, InvalidRequest},
		{"over bound", &repeatedReader{remaining: MaxBootstrapBytes + 1}, TooLarge},
		{"malformed JSON", strings.NewReader("{"), InvalidRequest},
		{"invalid legacy", strings.NewReader(`{"rails":[]}`), InvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeAdmittedMember(tc.reader)
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, Member{}, got)
		})
	}

	_, err := DecodeBootstrap(failingReader{})
	require.ErrorIs(t, err, InvalidRequest)
}

// Shared Site vectors and opt-in regeneration. Ordinary tests never write goldens.

type siteVector struct {
	Name           string `json:"name"`
	Site           string `json:"site"`
	Publication    string `json:"publication"`
	Content        string `json:"content"`
	Membership     string `json:"membership"`
	ContentHash    string `json:"content_hash"`
	MembershipHash string `json:"membership_hash"`
	Delta          string `json:"delta,omitempty"`
}

// Regenerate with RACER_UPDATE_SITE_VECTORS=1 and a bounded Go test command
// selecting TestGenerateSharedSiteVectors. Rust consumes the same output.
func TestGenerateSharedSiteVectors(t *testing.T) {
	if os.Getenv("RACER_UPDATE_SITE_VECTORS") != "1" {
		t.Skip("set RACER_UPDATE_SITE_VECTORS=1 to regenerate shared Site vectors")
	}

	regenerateSharedVectors(t)
	p, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	require.NoError(t, err)

	var (
		previous Publication
		vectors  []siteVector
	)

	for i, step := range siteSteps {
		p.Sequence, p.MembershipVersion = Sequence(i+1), MembershipVersion(i+1)
		p.Members[0].Site = step.site
		v := siteVector{Name: step.name, Site: step.site}
		publication, err := EncodePublication(p)
		require.NoError(t, err)

		v.Publication = string(publication)
		candidate, err := NewCanonicalCandidate(p)
		require.NoError(t, err)
		content, membership, err := candidate.canonicalContent()
		require.NoError(t, err)

		v.Content, v.Membership = string(content), string(membership)
		v.ContentHash, v.MembershipHash, err = candidate.ContentHashes()
		require.NoError(t, err)

		if i > 0 {
			delta, err := EncodeDelta(previous, p)
			require.NoError(t, err)

			v.Delta = string(delta)
		}

		vectors = append(vectors, v)
		previous = canonicalPublication(p)
	}

	encoded, err := json.MarshalIndent(vectors, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile("testdata/site-vectors.json", append(encoded, '\n'), 0o644))
}

var siteSteps = []struct{ name, site string }{
	{"absent", ""},
	{"added", "Site_1.west-2"},
	{"changed", "Site_2.east-1"},
	{"removed", ""},
}

func TestSharedSiteVectors(t *testing.T) {
	var vectors []siteVector
	require.NoError(t, json.Unmarshal(fixture(t, "site-vectors.json"), &vectors))
	require.Len(t, vectors, 4)
	legacy, err := DecodePublication(bytes.NewReader(fixture(t, "publication.json")))
	require.NoError(t, err)

	var previous Publication

	for i, step := range siteSteps {
		v := vectors[i]

		t.Run(step.name, func(t *testing.T) {
			require.Equal(t, step.name, v.Name)
			require.Equal(t, step.site, v.Site)
			p, err := DecodePublication(bytes.NewBufferString(v.Publication))
			require.NoError(t, err)

			want := canonicalPublication(legacy)
			want.Sequence, want.MembershipVersion = Sequence(i+1), MembershipVersion(i+1)
			want.Members[0].Site = step.site
			require.Equal(t, want, p, "only Site and counters change")
			encoded, err := EncodePublication(p)
			require.NoError(t, err)
			require.Equal(t, []byte(v.Publication), encoded)

			if step.site == "" {
				require.Contains(t, string(encoded), `"site":""`)
			} else {
				require.Contains(t, string(encoded), `"rdma_nics":[],"site":"`+step.site+`"`)
			}

			candidate, err := NewCanonicalCandidate(p)
			require.NoError(t, err)
			content, membership, err := candidate.canonicalContent()
			require.NoError(t, err)
			require.Equal(t, []byte(v.Content), content)
			require.Equal(t, []byte(v.Membership), membership)

			ph, mh, err := ContentHashes(p)
			require.NoError(t, err)
			require.Equal(t, v.ContentHash, ph)
			require.Equal(t, v.MembershipHash, mh)

			if i == 0 {
				require.Empty(t, v.Delta)
				return
			}

			require.NotEqual(t, vectors[i-1].ContentHash, ph)
			require.NotEqual(t, vectors[i-1].MembershipHash, mh)

			delta, err := EncodeDelta(previous, p)
			require.NoError(t, err)
			require.Equal(t, []byte(v.Delta), delta)
			applied, err := ApplyDelta(previous, bytes.NewBufferString(v.Delta))
			require.NoError(t, err)
			require.Equal(t, p, applied)

			var d Delta
			require.NoError(t, json.Unmarshal([]byte(v.Delta), &d))
			require.Equal(t, []Member{p.Members[0]}, d.UpsertMembers)
			require.Empty(t, d.RemoveMembers, "removing Site replaces the member, not the node")
			d.UpsertMembers[0].Site = "tampered-site"
			bad, err := json.Marshal(d)
			require.NoError(t, err)
			_, err = ApplyDelta(previous, bytes.NewReader(bad))
			require.ErrorIs(t, err, Conflict, "Site is bound to the target hash")

			wrongBase := canonicalPublication(previous)
			wrongBase.Members[0].Site = "stale-site"
			_, err = ApplyDelta(wrongBase, bytes.NewBufferString(v.Delta))
			require.ErrorIs(t, err, Conflict, "Site is bound to the base hash")
			_, err = ApplyDelta(p, bytes.NewBufferString(v.Delta))
			require.ErrorIs(t, err, Conflict, "replay rejected")
		})

		previous, err = DecodePublication(bytes.NewBufferString(v.Publication))
		require.NoError(t, err)
	}

	require.Equal(t, vectors[0].Content, vectors[3].Content)
	require.Equal(t, vectors[0].Membership, vectors[3].Membership)
	require.Equal(t, vectors[0].ContentHash, vectors[3].ContentHash)
	require.Equal(t, vectors[0].MembershipHash, vectors[3].MembershipHash)
}

// Only public fixture inputs are persisted, never private certificate keys.
func regenerateSharedVectors(t *testing.T) {
	t.Helper()

	var p Publication
	require.NoError(t, json.Unmarshal(fixture(t, "publication.json"), &p))
	// Preserve unsorted members/NICs and escaped text when upgrading old input.
	var old struct {
		Members []struct {
			Rails []struct {
				Rail     uint16
				Fabric   string
				NUMANode *uint32 `json:"numa_node"`
			}
		}
	}
	require.NoError(t, json.Unmarshal(fixture(t, "publication.json"), &old))

	for i := range p.Members {
		if p.Members[i].RDMANICs == nil {
			p.Members[i].RDMANICs = []RDMANIC{}
			for _, rail := range old.Members[i].Rails {
				p.Members[i].RDMANICs = append(p.Members[i].RDMANICs, RDMANIC{Device: rail.Fabric, Port: 1, Rail: rail.Rail, NUMANode: rail.NUMANode})
			}
		}
	}

	b, err := json.Marshal(p)
	require.NoError(t, err)
	writeSharedVector(t, "publication.json", b)

	candidate, err := NewCanonicalCandidate(p)
	require.NoError(t, err)
	c, m, err := candidate.canonicalContent()
	require.NoError(t, err)
	writeSharedVector(t, "content.json", c)
	writeSharedVector(t, "membership.json", m)

	ph, mh, err := candidate.ContentHashes()
	require.NoError(t, err)
	b, err = json.Marshal(map[string]string{"content": ph, "membership": mh})
	require.NoError(t, err)
	writeSharedVector(t, "hashes.json", b)

	var request BootstrapRequest
	require.NoError(t, json.Unmarshal(fixture(t, "bootstrap-request.json"), &request))
	request.Shares = DefaultShares
	b, err = EncodeBootstrapRequest(request)
	require.NoError(t, err)
	writeSharedVector(t, "bootstrap-request.json", b)
	regenerateRejections(t)
	writeSharedVector(t, "bootstrap-response.json", fixture(t, "bootstrap-response.json"))

	var bundle bundleJSON
	require.NoError(t, json.Unmarshal(fixture(t, "bundle.json"), &bundle))

	for i := range bundle.CacheKeys {
		id := make([]byte, 16)
		copy(id, "RKG1")
		binary.BigEndian.PutUint64(id[4:12], uint64(i%2+1))
		bundle.CacheKeys[i].ID = id
		// Deterministic public test material must be unique across refs too.
		bundle.CacheKeys[i].Material = bytes.Repeat([]byte{byte(i)}, 32)
	}

	b, err = json.Marshal(bundle)
	require.NoError(t, err)
	_, err = DecodeBundle(bytes.NewReader(b))
	require.NoError(t, err)
	writeSharedVector(t, "bundle.json", b)
}

func writeSharedVector(t *testing.T, name string, b []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile("testdata/"+name, append(b, '\n'), 0o644))

	if name == "publication.json" || name == "bootstrap-request.json" || name == "bootstrap-response.json" || name == "bundle.json" {
		// Rust reads this directory directly. Remove old generated copies too.
		err := os.Remove("../../../cmd/racer-dataplane/src/control/testdata/" + name)
		if !os.IsNotExist(err) {
			require.NoError(t, err)
		}
	}
}

type rejectionVector struct {
	Name string    `json:"name"`
	File string    `json:"file"`
	Old  string    `json:"old"`
	New  string    `json:"new"`
	Code ErrorCode `json:"code"`
}

func regenerateRejections(t *testing.T) {
	t.Helper()

	var rejections []rejectionVector
	require.NoError(t, json.Unmarshal(fixture(t, "rejections.json"), &rejections))

	for i := range rejections {
		switch rejections[i].Name {
		case "missing required field":
			rejections[i].Old, rejections[i].New = `"rdma_nics":[]`, `"ignored_nics":[]`
		case "duplicate rail":
			rejections[i].Name = "zero NIC port"
			rejections[i].Old, rejections[i].New = `"port":1`, `"port":0`
		case "empty fabric":
			rejections[i].Name = "empty device"
		}
	}

	for _, addition := range []struct{ name, file, old, new string }{
		{"duplicate key material", "bundle.json", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		{"missing bootstrap NIC report", "bootstrap-request.json", `"rdma_nics":[],`, ""},
		{"null bootstrap NIC report", "bootstrap-request.json", `"rdma_nics":[]`, `"rdma_nics":null`},
		{"legacy bootstrap rails", "bootstrap-request.json", `"rdma_nics":[]`, `"rails":[]`},
		{"legacy member rails", "publication.json", `"rdma_nics":[]`, `"rails":[],"alignment_enabled":true`},
		{"overflow NIC port", "publication.json", `"port":1`, `"port":256`},
		{"null NIC GID", "publication.json", `"port":1`, `"port":1,"gid":null`},
		{"empty NIC GID", "publication.json", `"port":1`, `"port":1,"gid":""`},
		{"uppercase NIC GID", "publication.json", `"port":1`, `"port":1,"gid":"ABCDEF0123456789abcdef0123456789"`},
	} {
		found := slices.ContainsFunc(rejections, func(rejection rejectionVector) bool { return rejection.Name == addition.name })
		if !found {
			rejections = append(rejections, rejectionVector{addition.name, addition.file, addition.old, addition.new, InvalidRequest})
		}
	}

	b, err := json.MarshalIndent(rejections, "", "  ")
	require.NoError(t, err)
	writeSharedVector(t, "rejections.json", b)
}
