// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package meshfetch

func allocPage(n int) []byte { return make([]byte, n) }

func freePage([]byte) {}
