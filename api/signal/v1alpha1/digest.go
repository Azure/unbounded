// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "regexp"

// SHA256DigestPattern is the canonical Project Signal digest representation.
const SHA256DigestPattern = `^sha256:[0-9a-f]{64}$`

var sha256DigestPattern = regexp.MustCompile(SHA256DigestPattern)

// IsSHA256Digest reports whether value is an algorithm-qualified SHA-256 digest.
func IsSHA256Digest(value string) bool {
	return sha256DigestPattern.MatchString(value)
}
