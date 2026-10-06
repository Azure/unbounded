// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBootstrapRequestEncodedBoundary(t *testing.T) {
	request, err := DecodeBootstrap(bytes.NewReader(fixture(t, "bootstrap-request.json")))
	if err != nil {
		t.Fatal(err)
	}

	request.CSRDER = []byte{}
	request.RDMANICs = []RDMANIC{{Device: "mlx5_0", Port: 1, Rail: 1, GID: "abcdef0123456789abcdef0123456789"}}

	framing, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	maxDER := (MaxBootstrapBytes - len(framing)) / 4 * 3

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	makeCSR := func(padding int) []byte {
		t.Helper()

		der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: strings.Repeat("x", padding)}}, key)
		if err != nil {
			t.Fatal(err)
		}

		return der
	}
	padding := maxDER - 256
	padding += maxDER - len(makeCSR(padding))

	// Exercise every base64 padding case at the largest accepted encoding and
	// the next DER byte, where framing pushes the document over the limit.
	for _, delta := range []int{-2, -1, 0, 1} {
		request.CSRDER = makeCSR(padding + delta)
		if len(request.CSRDER) != maxDER+delta {
			t.Fatal("CSR fixture missed the encoded boundary")
		}

		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}

		validationErr := ValidateBootstrapRequest(request)
		encoded, encodeErr := EncodeBootstrapRequest(request)

		_, decodeErr := DecodeBootstrap(bytes.NewReader(raw))
		if delta == 1 {
			if len(raw) <= MaxBootstrapBytes || len(request.CSRDER) >= MaxBootstrapBytes {
				t.Fatal("fixture must overflow only after encoding")
			}

			if !errors.Is(validationErr, TooLarge) || !errors.Is(encodeErr, TooLarge) || !errors.Is(decodeErr, TooLarge) || encoded != nil {
				t.Fatalf("over bound: validate=%v encode=%v decode=%v", validationErr, encodeErr, decodeErr)
			}

			continue
		}

		if MaxBootstrapBytes-len(raw) >= 4 || validationErr != nil || encodeErr != nil || decodeErr != nil || !bytes.Equal(raw, encoded) {
			t.Fatalf("at bound (%d bytes): validate=%v encode=%v decode=%v", len(raw), validationErr, encodeErr, decodeErr)
		}
	}
}

func TestBootstrapResponseEncodedBoundary(t *testing.T) {
	response, err := DecodeBootstrapResponse(bytes.NewReader(fixture(t, "bootstrap-response.json")))
	if err != nil {
		t.Fatal(err)
	}

	cert := response.CertificateChain[0]
	response.CertificateChain = nil

	var previous []byte

	for {
		response.CertificateChain = append(response.CertificateChain, cert)

		raw, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}

		encoded, encodeErr := EncodeBootstrap(response)
		if len(raw) <= MaxBootstrapBytes {
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}

			previous = encoded

			continue
		}

		if len(response.CertificateChain)*len(cert) >= MaxBootstrapBytes {
			t.Fatal("fixture must overflow only after encoding")
		}

		if !errors.Is(encodeErr, TooLarge) || encoded != nil {
			t.Fatalf("encoded response overflow: %v", encodeErr)
		}

		if _, err := DecodeBootstrapResponse(bytes.NewReader(raw)); !errors.Is(err, TooLarge) {
			t.Fatalf("decoded response overflow: %v", err)
		}

		if _, err := DecodeBootstrapResponse(bytes.NewReader(previous)); err != nil {
			t.Fatalf("last fitting response: %v", err)
		}

		break
	}
}
