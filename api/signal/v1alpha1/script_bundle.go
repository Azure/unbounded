// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// DeriveScriptBundleDigest returns the digest of the selected script contents
// in check order. Each manifest line is the algorithm-qualified digest of one
// script's exact bytes.
func DeriveScriptBundleDigest(checks []CertificationCheck, scripts map[string]string) (string, error) {
	if len(checks) == 0 {
		return "", errors.New("script bundle must contain at least one check")
	}

	var manifest strings.Builder

	for index := range checks {
		content, found := scripts[checks[index].ScriptKey]
		if !found {
			return "", fmt.Errorf("script bundle has no key %s", checks[index].ScriptKey)
		}

		manifest.WriteString(sha256Digest([]byte(content)))
		manifest.WriteByte('\n')
	}

	return sha256Digest([]byte(manifest.String())), nil
}

// DeriveScriptSnapshotBundleDigest returns the aggregate digest represented by
// immutable Run check snapshots.
func DeriveScriptSnapshotBundleDigest(checks []CertificationCheckSnapshot) (string, error) {
	if len(checks) == 0 {
		return "", errors.New("script bundle must contain at least one check")
	}

	var manifest strings.Builder

	for index := range checks {
		if !IsSHA256Digest(checks[index].Script.Digest) {
			return "", fmt.Errorf("check %d script digest is invalid", index)
		}

		manifest.WriteString(checks[index].Script.Digest)
		manifest.WriteByte('\n')
	}

	return sha256Digest([]byte(manifest.String())), nil
}

func sha256Digest(content []byte) string {
	sum := sha256.Sum256(content)

	return "sha256:" + hex.EncodeToString(sum[:])
}
