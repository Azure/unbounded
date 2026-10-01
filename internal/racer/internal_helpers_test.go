// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

func trustReady(trust *Trust) bool {
	_, err := trust.pool()
	return err == nil
}
