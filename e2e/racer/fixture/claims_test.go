// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestCertificateClaimsWire(t *testing.T) {
	claims := Claims{Version: 1, Namespace: "probe", Identity: CertificateIdentity{Kind: "node", Universe: strings.Repeat("01", 32), Node: strings.Repeat("02", 32), PodUID: "pod", BootID: strings.Repeat("03", 32), PodName: "worker"}}
	// This literal locks Rust's serde field order independently of Go marshaling.
	wire := `{"version":1,"namespace":"probe","identity":{"kind":"node","universe":"` + strings.Repeat("01", 32) + `","node":"` + strings.Repeat("02", 32) + `","podUID":"pod","bootID":"` + strings.Repeat("03", 32) + `","podName":"worker","containerID":""}}`

	uri := "spiffe://racer/v1/" + hex.EncodeToString([]byte(wire))
	if claims.URI() != uri {
		t.Fatal("certificate claims are not canonical Rust wire format")
	}

	got, err := ParseClaims(uri)
	if err != nil || got != claims {
		t.Fatalf("claims round trip: %+v %v", got, err)
	}

	for _, bad := range []string{"spiffe://racer/controlplane", uri + "00", "spiffe://racer/v1/" + hex.EncodeToString([]byte(strings.Replace(wire, `"version":1`, `"version":2`, 1))), "spiffe://racer/v1/" + hex.EncodeToString([]byte(" "+wire))} {
		if _, err := ParseClaims(bad); err == nil {
			t.Fatal("accepted unsupported or noncanonical claims")
		}
	}
}
