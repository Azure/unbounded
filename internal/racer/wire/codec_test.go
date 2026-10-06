// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestKeyringDeliveryEncodingBoundsAndGeneration(t *testing.T) {
	bundle, err := DecodeBundle(bytes.NewReader(fixture(t, "bundle.json")))
	if err != nil {
		t.Fatal(err)
	}
	// Delivery uses the existing full bundle, including lossless uint64 counters.
	bundle.Generation = ^Generation(0)

	encoded, err := EncodeBundle(bundle)
	if err != nil || !bytes.Contains(encoded, []byte(`"generation":"18446744073709551615"`)) {
		t.Fatalf("generation encoding: %v", err)
	}

	decoded, err := DecodeBundle(bytes.NewReader(encoded))
	if err != nil || decoded.Generation != bundle.Generation || !decoded.CacheKeys[0].EqualMaterial(bundle.CacheKeys[0]) {
		t.Fatalf("full delivery round trip: %v", err)
	}

	for n := uint64(1); n <= 4000; n++ {
		ref := bundle.CacheKeys[0].Key
		ref.ID = make([]byte, 16)
		copy(ref.ID, "RKG1")
		binary.BigEndian.PutUint64(ref.ID[4:12], n+10)

		var material [32]byte
		binary.BigEndian.PutUint64(material[:8], n+10)

		key, err := NewCacheKey(ref, PreparedKey, material)
		if err != nil {
			t.Fatal(err)
		}

		bundle.CacheKeys = append(bundle.CacheKeys, key)
	}

	if encoded, err := EncodeBundle(bundle); !errors.Is(err, TooLarge) || encoded != nil {
		t.Fatalf("oversize delivery encoding not rejected: %v", err)
	}
}

func TestBundleEncodingCannotBypassCodec(t *testing.T) {
	if _, err := json.Marshal(KeyringBundle{}); !errors.Is(err, UnsupportedVersion) {
		t.Fatalf("standard encoder bypassed bundle validation: %v", err)
	}
}

// Rust IdentityManager::install also rejects material shared by distinct refs,
// regardless of scope or state. Keep both Go codec directions equally strict.
func TestBundleDuplicateMaterialParity(t *testing.T) {
	for _, scope := range []string{"id", "purpose", "cache"} {
		t.Run(scope, func(t *testing.T) {
			bundle, err := DecodeBundle(bytes.NewReader(fixture(t, "bundle.json")))
			if err != nil {
				t.Fatal(err)
			}

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
			if err != nil {
				t.Fatal("distinct material rejected", err)
			}

			var document map[string]any
			if err := json.Unmarshal(valid, &document); err != nil {
				t.Fatal(err)
			}

			keys := document["cache_keys"].([]any)
			keys[1].(map[string]any)["material"] = keys[0].(map[string]any)["material"]

			corrupt, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := DecodeBundle(bytes.NewReader(corrupt)); !errors.Is(err, InvalidRequest) {
				t.Fatalf("decoder accepted duplicate material: %v", err)
			}

			bundle.CacheKeys[1].material = first.material
			if _, err := EncodeBundle(bundle); !errors.Is(err, InvalidRequest) {
				t.Fatalf("encoder accepted duplicate material: %v", err)
			}
		})
	}
}

func TestRetiringKeyStateIsRejected(t *testing.T) {
	raw := fixture(t, "bundle.json")
	// Replace prepared, not active: an active key remains, so rejection must
	// enforce the enum rather than merely the one-active-key requirement.
	changed := bytes.Replace(raw, []byte(`"state":"prepared"`), []byte(`"state":"retiring"`), 1)
	if bytes.Equal(raw, changed) {
		t.Fatal("mutation did not match")
	}

	if _, err := DecodeBundle(bytes.NewReader(changed)); !errors.Is(err, InvalidRequest) {
		t.Fatalf("retiring wire state accepted: %v", err)
	}

	bundle, err := DecodeBundle(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := NewCacheKey(bundle.CacheKeys[0].Key, KeyState("retiring"), [32]byte{}); !errors.Is(err, InvalidRequest) {
		t.Fatalf("retiring constructor state accepted: %v", err)
	}

	bundle.CacheKeys[1].State = KeyState("retiring")
	if _, err := EncodeBundle(bundle); !errors.Is(err, InvalidRequest) {
		t.Fatalf("retiring encoder state accepted: %v", err)
	}
}

func TestKeyIDsRequireCurrentNamespaceAndValidGeneration(t *testing.T) {
	bundle, err := DecodeBundle(bytes.NewReader(fixture(t, "bundle.json")))
	if err != nil {
		t.Fatal(err)
	}

	bundle.Generation = 2
	for _, generation := range []uint64{0, 1, 2, 3} {
		ref := bundle.CacheKeys[0].Key
		ref.ID = bytes.Clone(ref.ID)
		binary.BigEndian.PutUint64(ref.ID[4:12], generation)

		key, err := NewCacheKey(ref, ActiveKey, [32]byte{})
		if generation == 0 {
			if err == nil {
				t.Fatal("zero generation accepted")
			}

			continue
		}

		if err != nil {
			t.Fatal(err)
		}

		candidate := bundle
		candidate.CacheKeys = []CacheKey{key}

		_, err = EncodeBundle(candidate)
		if (err == nil) != (generation <= 2) {
			t.Fatal(generation, err)
		}
	}

	for _, id := range [][]byte{make([]byte, 16), []byte("RKG0abcdefgh1234"), []byte("RKG1")} {
		ref := bundle.CacheKeys[0].Key

		ref.ID = id
		if _, err := NewCacheKey(ref, ActiveKey, [32]byte{}); err == nil {
			t.Fatal("legacy ID accepted")
		}
	}
}

func TestKeyDiagnosticsRedactMaterial(t *testing.T) {
	key := CacheKey{material: [32]byte{1, 2, 3}}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if got := fmt.Sprintf(format, key); got != "<redacted cache key>" {
			t.Fatalf("diagnostic leaked key structure: %s", got)
		}
	}

	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(encoded), "material") {
		t.Fatal("standard JSON exposed private key material")
	}
}
