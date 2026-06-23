// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package storageconfig holds the Go bindings for the unbounded-storage
// daemon configuration schema. The schema source of truth is config.proto
// in this directory; both these Go bindings and the daemon's Rust (prost)
// bindings are generated from it, so the on-disk protobuf wire format is
// shared across the supervisor (writer) and the daemon (reader).
package storageconfig

//go:generate protoc --go_out=. --go_opt=paths=source_relative config.proto
