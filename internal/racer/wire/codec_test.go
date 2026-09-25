// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestBundleEncodingCannotBypassCodec(t *testing.T) {
	if _, err := json.Marshal(KeyringBundle{}); !errors.Is(err, UnsupportedVersion) {
		t.Fatalf("standard encoder bypassed bundle validation: %v", err)
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
