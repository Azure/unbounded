// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package control

import "slices"

// Targets returns each member's target load (indexed like members) under
// the Planner rules; non-assignable members get 0.
func (pl *Planner) Targets(bits uint8, members []PlanMember) ([]int64, error) {
	if _, err := pl.prepare(1<<bits, members); err != nil {
		return nil, err
	}

	pl.members = nil

	return slices.Clone(pl.target[:len(members)]), nil
}
