// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// Object identifies an exact upstream object within a configured namespace.
type Object struct {
	Schema    int    `json:"schema"`
	Namespace string `json:"namespace"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
}

// NewRequest hashes canonical schema-1 JSON and carries it as origin metadata.
// Keys and version IDs are never cleaned, decoded, or normalized.
func NewRequest(namespace, bucket, key, versionID string) (racersdk.Request, error) {
	object := Object{Schema: 1, Namespace: namespace, Bucket: bucket, Key: key, VersionID: versionID}
	if !validObject(object) {
		return racersdk.Request{}, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	canonical, err := json.Marshal(object)
	if err != nil {
		return racersdk.Request{}, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, err)
	}

	metadata, err := racersdk.ParseAdapterMetadata(string(canonical))
	if err != nil {
		return racersdk.Request{}, err
	}

	fetch, err := racersdk.NewFetchContext(metadata, racersdk.Authorization{})
	if err != nil {
		return racersdk.Request{}, err
	}

	return racersdk.Request{Key: sha256.Sum256(canonical), Context: fetch}, nil
}

func validNamespace(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) &&
		strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) == -1
}

func validBucket(value string) bool {
	if value == "" || len(value) > 255 || value == "." || value == ".." {
		return false
	}

	// Include legacy S3-compatible bucket names, but not paths or endpoint URLs.
	for _, c := range value {
		if c < 'a' || c > 'z' {
			if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '-' && c != '_' {
				return false
			}
		}
	}

	return true
}

func validObject(object Object) bool {
	return object.Schema == 1 && validNamespace(object.Namespace) && validBucket(object.Bucket) &&
		object.Key != "" && len(object.Key) <= 1024 && utf8.ValidString(object.Key) &&
		len(object.VersionID) <= 1024 && utf8.ValidString(object.VersionID)
}

func decodeObject(request racersdk.OriginRequest, namespace string, buckets map[string]struct{}) (Object, error) {
	raw := request.Context().Metadata().ForOrigin()

	var object Object
	if err := json.Unmarshal([]byte(raw), &object); err != nil || !validObject(object) {
		return Object{}, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	canonical, err := NewRequest(object.Namespace, object.Bucket, object.Key, object.VersionID)
	if err != nil || canonical.Context.Metadata().ForOrigin() != raw || canonical.Key != request.Key() {
		return Object{}, racersdk.NewOriginError(racersdk.ErrorInvalidArgument, nil)
	}

	if object.Namespace != namespace {
		return Object{}, racersdk.NewOriginError(racersdk.ErrorForbidden, nil)
	}

	if len(buckets) != 0 {
		if _, ok := buckets[object.Bucket]; !ok {
			return Object{}, racersdk.NewOriginError(racersdk.ErrorForbidden, nil)
		}
	}

	return object, nil
}
