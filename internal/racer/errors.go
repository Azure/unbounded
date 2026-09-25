// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import "github.com/Azure/unbounded/internal/racer/wire"

var ErrUnimplemented = wire.ErrUnimplemented

func pending(operation string) error { return wire.Pending(operation) }
