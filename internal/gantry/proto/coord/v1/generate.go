// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package coordv1 holds libp2p coordination messages such as pull intent
// and please-pull. These messages are private to Gantry.
package coordv1

//go:generate protoc --go_out=. --go_opt=paths=source_relative coord.proto
