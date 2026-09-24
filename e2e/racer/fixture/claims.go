// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Claims mirrors the versioned certificate SAN contract, including JSON field
// order: the Rust verifier rejects noncanonical encodings.
type Claims struct {
	Version   int                 `json:"version"`
	Namespace string              `json:"namespace"`
	Identity  CertificateIdentity `json:"identity"`
}

type CertificateIdentity struct {
	Kind        string `json:"kind"`
	Universe    string `json:"universe"`
	Node        string `json:"node"`
	PodUID      string `json:"podUID"`
	BootID      string `json:"bootID"`
	PodName     string `json:"podName"`
	ContainerID string `json:"containerID"`
}

func (c Claims) URI() string {
	raw, err := json.Marshal(c)
	if err != nil {
		panic(err) // Claims contains only strings and integers.
	}

	return "spiffe://racer/v1/" + hex.EncodeToString(raw)
}

func ParseClaims(uri string) (Claims, error) {
	var c Claims

	raw, err := hex.DecodeString(strings.TrimPrefix(uri, "spiffe://racer/v1/"))
	if err != nil {
		return c, err
	}

	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}

	if c.Version != 1 || c.URI() != uri {
		return c, fmt.Errorf("invalid certificate claims")
	}

	return c, nil
}
