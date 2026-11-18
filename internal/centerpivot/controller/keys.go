// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package controller is centerpivot's controller: a scheduler that hands out
// update slots, and a node reconciler that takes each node with a slot
// through its update, one step per pass. See designs/centerpivot.md.
package controller

import (
	"github.com/Azure/unbounded/internal/centerpivot"
	"github.com/Azure/unbounded/internal/centerpivot/contract"
)

// Keys are the agent's annotation contract under centerpivot's prefix.
var Keys = contract.KeysFor(centerpivot.Prefix)
