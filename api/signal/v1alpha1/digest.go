// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "regexp"

// SHA256DigestPattern is the canonical Project Signal digest representation.
const SHA256DigestPattern = `^sha256:[0-9a-f]{64}$`

// DigestPinnedImagePattern requires an immutable SHA-256 image reference.
const DigestPinnedImagePattern = `^[^@\s]+@sha256:[0-9a-f]{64}$`

var (
	sha256DigestPattern      = regexp.MustCompile(SHA256DigestPattern)
	digestPinnedImagePattern = regexp.MustCompile(DigestPinnedImagePattern)
)

// IsSHA256Digest reports whether value is an algorithm-qualified SHA-256 digest.
func IsSHA256Digest(value string) bool {
	return sha256DigestPattern.MatchString(value)
}

// IsDigestPinnedImage reports whether value is an immutable SHA-256 image reference.
func IsDigestPinnedImage(value string) bool {
	return digestPinnedImagePattern.MatchString(value)
}
