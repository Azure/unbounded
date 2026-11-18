// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racerobject defines the Racer object identity: the canonical JSON
// and SHA-256 Racer key for an object in an S3 or Azure Blob bucket. The Rust
// racer-object sidecar (cmd/racer-object) computes the same identity byte for
// byte. The package also validates bucket names and Azure Blob container URLs.
package racerobject

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// Object identifies an exact upstream object in the origin's bucket.
type Object struct {
	Schema    int    `json:"schema"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
}

// NewRequest hashes canonical schema-1 JSON and carries it as origin metadata.
// Keys and version IDs are never cleaned, decoded, or normalized.
func NewRequest(bucket, key, versionID string) (racersdk.Request, error) {
	object := Object{Schema: 1, Bucket: bucket, Key: key, VersionID: versionID}
	if !validObject(object) {
		return racersdk.Request{}, racersdk.ErrInvalidRequest
	}

	canonical, err := json.Marshal(object)
	if err != nil {
		return racersdk.Request{}, fmt.Errorf("%w: %w", racersdk.ErrInvalidRequest, err)
	}

	if len(canonical) > 8<<10 {
		return racersdk.Request{}, racersdk.ErrInvalidRequest
	}

	return racersdk.Request{Key: sha256.Sum256(canonical), Metadata: string(canonical)}, nil
}

func validBucket(value string) bool {
	if value == "" || len(value) > 255 || value == "." || value == ".." {
		return false
	}

	// Include legacy S3-compatible bucket names, but not paths or endpoint URLs.
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '-' && c != '_' {
			return false
		}
	}

	return true
}

func validObject(object Object) bool {
	return object.Schema == 1 && validBucket(object.Bucket) &&
		object.Key != "" && len(object.Key) <= 1024 && utf8.ValidString(object.Key) &&
		len(object.VersionID) <= 1024 && utf8.ValidString(object.VersionID)
}

var errInvalidBucket = errors.New("invalid bucket")

// ValidateBucket reports whether an origin accepts bucket, so callers that
// render origin flags can reject it before the origin starts.
func ValidateBucket(bucket string) error {
	if !validBucket(bucket) {
		return errInvalidBucket
	}

	return nil
}
