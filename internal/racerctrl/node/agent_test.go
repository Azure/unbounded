// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"testing"

	corelisters "k8s.io/client-go/listers/core/v1"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"

	racerconfig "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/racerctrl"
)

// emptyCluster gives an agent listers that see nothing, which is what a node
// looks like once every universe has stopped naming it.
func emptyCluster(a *Agent) {
	index := func() cache.Indexer {
		return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	}

	a.nodeLister = corelisters.NewNodeLister(index())
	a.volumeLister = corelisters.NewPersistentVolumeLister(index())
	a.classLister = storagelisters.NewStorageClassLister(index())
}

// A node that has handed everything over derives no universe at all, and racer
// refuses a config that names none. Publishing it would fail validation on
// every reconcile forever; the last config has to stay in force until the
// operator retires the identity.
func TestRenderLeavesTheLastConfigInForceWhenNothingIsLeft(t *testing.T) {
	dir := t.TempDir()
	agent := newBindingsAgent(t, dir)
	emptyCluster(agent)

	agent.self.ID = 1
	agent.self.Zone = 1
	agent.self.Cohort = 0
	agent.generation = 9
	agent.published = &racerconfig.NodeConfig{Generation: 9}

	if err := agent.render(); err != nil {
		t.Fatalf("render refused to leave a drained node alone: %v", err)
	}

	if agent.generation != 9 {
		t.Fatalf("generation moved to %d; nothing was installed", agent.generation)
	}

	if _, err := racerctrl.ReadConfig(agent.cfg.ConfigPath()); err != nil {
		t.Fatalf("read config: %v", err)
	}
}

// The sequencers have no clock: a counter is either reported or it is not, and
// a stale one reads as fact. A scrape that has failed for long enough has to
// withdraw what it last saw so the gates read no report rather than a number
// from before the node stopped answering.
func TestForgetHealthWithdrawsAStaleReport(t *testing.T) {
	agent := newBindingsAgent(t, t.TempDir())
	agent.self.Health = racerctrl.Health{Generation: 4, Shedding: 2}
	agent.self.Live = map[uint32]racerctrl.LiveExtent{1: {Pages: 8}}

	if !agent.forgetHealth() {
		t.Fatal("withdrawing a report that existed reported no change")
	}

	if agent.self.Health != (racerctrl.Health{}) || len(agent.self.Live) != 0 {
		t.Fatalf("health %v live %v survived the withdrawal", agent.self.Health, agent.self.Live)
	}

	if agent.forgetHealth() {
		t.Fatal("withdrawing nothing reported a change, which would rewrite annotations forever")
	}
}

// Withdrawing the counters must not withdraw the record of what was installed:
// that is a fact about this agent, not about racer, and the gates need it to
// tell a node that has not reported from one that never got the change.
func TestForgetHealthKeepsWhatWasInstalled(t *testing.T) {
	agent := newBindingsAgent(t, t.TempDir())
	agent.self.Applied = racerctrl.Applied{Generation: 4}
	agent.self.Health = racerctrl.Health{Generation: 4}

	agent.forgetHealth()

	if agent.self.Applied.Generation != 4 {
		t.Fatalf("applied generation %d was withdrawn with the counters", agent.self.Applied.Generation)
	}
}
