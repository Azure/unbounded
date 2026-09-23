// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer adapts OCI digest objects to the Racer streaming SDK.
package racer

import (
	"encoding/base64"
	"errors"
	"strings"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
)

// Target is a canonical, credential-free registry/repository/kind/digest key.
// Registry must be the configured canonical name, never an alias or endpoint.
func Target(ref ifaces.OriginRef) (string, error) {
	if ref.Registry == "" || len(ref.Registry) > 253 || strings.ContainsAny(ref.Registry, "/@?#\\ \t\r\n") || ref.Digest.IsZero() {
		return "", errors.New("invalid Racer OCI reference")
	}

	if err := oci.ValidateRepositoryName(ref.Repository); err != nil {
		return "", err
	}

	kind := "blobs"

	switch ref.Kind {
	case ifaces.KindManifest:
		kind = "manifests"
	case ifaces.KindBlob, ifaces.KindConfig:
	default:
		return "", errors.New("invalid Racer OCI kind")
	}

	return "/gantry/v1/" + base64.RawURLEncoding.EncodeToString([]byte(ref.Registry)) + "/" + base64.RawURLEncoding.EncodeToString([]byte(ref.Repository)) + "/" + kind + "/" + ref.Digest.String(), nil
}

// ParseTarget accepts only the canonical encoding produced by Target.
func ParseTarget(target string) (ifaces.OriginRef, error) {
	var ref ifaces.OriginRef

	parts := strings.Split(target, "/")
	if len(parts) != 7 || parts[0] != "" || parts[1] != "gantry" || parts[2] != "v1" {
		return ref, errors.New("invalid Racer OCI target")
	}

	registry, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return ref, err
	}

	repo, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		return ref, err
	}

	ref.Registry, ref.Repository = string(registry), string(repo)

	switch parts[5] {
	case "blobs":
		ref.Kind = ifaces.KindBlob
	case "manifests":
		ref.Kind = ifaces.KindManifest
	default:
		return ref, errors.New("invalid Racer OCI kind")
	}

	ref.Digest, err = digest.Parse(parts[6])
	if err != nil {
		return ref, err
	}

	canonical, err := Target(ref)
	if err != nil || canonical != target {
		return ref, errors.New("noncanonical Racer OCI target")
	}

	return ref, nil
}
