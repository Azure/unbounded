// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
)

func TestSeedsAccessor(t *testing.T) {
	n := New(Config{ID: "a", Incarnation: 1, Addr: "a:1", Seeds: []string{"s:1"}}, rand.New(rand.NewPCG(1, 2)))
	if got := n.Seeds(nil); !slices.Equal(got, []string{"s:1"}) {
		t.Fatalf("configured seeds = %v", got)
	}

	// A member without configured seeds learns them from its Lease read.
	n = New(Config{ID: "a", Incarnation: 1, Addr: "a:1"}, rand.New(rand.NewPCG(1, 2)))
	n.Start(1, time.Now())

	ops := n.LeaseOps()
	if len(ops) == 0 {
		t.Fatal("no lease read")
	}

	n.OnLease(2, time.Now(), elect.Result{Seq: ops[0].Seq, Kind: ops[0].Kind, Record: elect.Record{
		Holder: "l", Addr: "l:1", Seeds: []string{"x:1", "a:1"},
	}})

	if got := n.Seeds([]string{"keep"}); !slices.Equal(got, []string{"keep", "l:1", "x:1"}) {
		t.Fatalf("lease seeds = %v", got)
	}
}
