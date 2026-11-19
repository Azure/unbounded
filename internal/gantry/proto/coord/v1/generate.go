// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package coordv1 holds gantry's libp2p coordination RPC messages, such as
// pull intent and please-pull. It lives under internal/ so the wire schema is
// not an exported API surface.
package coordv1

//go:generate protoc --go_out=. --go_opt=paths=source_relative coord.proto
