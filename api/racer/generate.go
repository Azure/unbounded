// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package racerconfig holds the generated bindings for the RACER node
// configuration schema.
//
// config.proto is the contract between the Go control plane and the Rust
// dataplane in cmd/racer. Both sides generate from this one file: Go through
// protoc below, Rust through prost in cmd/racer/build.rs. Neither hand-writes
// these types, so neither can drift from the other.
package racerconfig

//go:generate protoc --go_out=. --go_opt=paths=source_relative config.proto
