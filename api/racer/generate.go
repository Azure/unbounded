// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racerconfig contains the Go bindings for Racer's shared control schema.
// control.proto was imported from racer-dataplane/proto/control.proto at
// c9bf09848a66df58d7cde6bb09bd5c6fcc61913c. Preserve its wire package and field
// numbers when generating Go and Rust bindings from this source of truth.
package racerconfig

//go:generate protoc --go_out=. --go_opt=paths=source_relative control.proto
