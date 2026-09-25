// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"errors"
	"fmt"
	"io"
)

var ErrUnimplemented = errors.New("racer operation is not implemented")

func Pending(operation string) error {
	return fmt.Errorf("%s: %w", operation, ErrUnimplemented)
}

// DecodeBootstrap must bound bytes before allocation, reject duplicate fields,
// validate enums/identities/schema, and ignore unknown object fields.
func DecodeBootstrap(_ io.Reader) (BootstrapRequest, error) {
	return BootstrapRequest{}, Pending("wire.decode_bootstrap")
}

func EncodeBootstrap(_ BootstrapResponse) ([]byte, error) {
	return nil, Pending("wire.encode_bootstrap")
}

// EncodePublication must validate all bounds and produce deterministic bytes.
func EncodePublication(_ Publication) ([]byte, error) {
	return nil, Pending("wire.encode_publication")
}

func DecodeBundle(_ io.Reader) (KeyringBundle, error) {
	return KeyringBundle{}, Pending("wire.decode_bundle")
}

func EncodeBundle(_ KeyringBundle) ([]byte, error) {
	return nil, Pending("wire.encode_bundle")
}

// NewCacheKey is the only material ingress besides bounded bundle decoding.
func NewCacheKey(_ CacheKeyRef, _ KeyState, _ [32]byte) (CacheKey, error) {
	return CacheKey{}, Pending("wire.new_cache_key")
}
