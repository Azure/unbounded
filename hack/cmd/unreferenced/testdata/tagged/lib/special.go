// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build special

package lib

// UseIt is only compiled under the `special` tag, and is itself dead.
func UseIt() { OnlyUnderTag() }
