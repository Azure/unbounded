// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package coldstart

import (
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestPrefetchDispatchPlanDesynchronizesNodesDeterministically(t *testing.T) {
	t.Parallel()

	coordinationKey := digest.MustParse("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	offsetA, delayA := prefetchDispatchPlan("node-a", coordinationKey, 554, time.Second)
	offsetA2, delayA2 := prefetchDispatchPlan("node-a", coordinationKey, 554, time.Second)
	offsetB, delayB := prefetchDispatchPlan("node-b", coordinationKey, 554, time.Second)

	if offsetA != offsetA2 || delayA != delayA2 {
		t.Fatalf("same node plan changed: (%d, %v) != (%d, %v)", offsetA, delayA, offsetA2, delayA2)
	}

	if offsetA == offsetB && delayA == delayB {
		t.Fatalf("different nodes got the same plan: node-a=(%d,%v) node-b=(%d,%v)", offsetA, delayA, offsetB, delayB)
	}

	for node, plan := range map[ifaces.NodeID]struct {
		offset int
		delay  time.Duration
	}{
		"node-a": {offset: offsetA, delay: delayA},
		"node-b": {offset: offsetB, delay: delayB},
	} {
		if plan.offset < 0 || plan.offset >= 554 {
			t.Errorf("%s offset = %d, want [0,554)", node, plan.offset)
		}

		if plan.delay < 0 || plan.delay >= time.Second {
			t.Errorf("%s delay = %v, want [0,1s)", node, plan.delay)
		}
	}
}

func TestPrefetchDispatchPlanDisabledJitter(t *testing.T) {
	t.Parallel()

	coordinationKey := digest.MustParse("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	offset, delay := prefetchDispatchPlan("node-a", coordinationKey, 0, 0)
	if offset != 0 || delay != 0 {
		t.Fatalf("plan = (%d,%v), want (0,0)", offset, delay)
	}
}
