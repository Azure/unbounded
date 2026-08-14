// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	k8stesting "k8s.io/client-go/testing"
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

// reconcilingAgent builds an agent that can run a whole reconcile against one
// Node and nothing else: no storage classes, no volumes, and a membership
// lister that is already in place so the pass never has to start an informer.
func reconcilingAgent(t *testing.T, node *corev1.Node) (*Agent, *fake.Clientset) {
	t.Helper()

	agent, client := announcingAgent(t, node)

	index := func() cache.Indexer {
		return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	}

	agent.volumeLister = corelisters.NewPersistentVolumeLister(index())
	agent.classLister = storagelisters.NewStorageClassLister(index())
	agent.memberLister = corelisters.NewConfigMapLister(index())

	return agent, client
}

// halfIdentifiedNode has an id but no zone, which is what a Node looks like
// between the operator's writes. The derivation refuses it, so it is the
// cheapest way to put a reconcile through a failing render.
func halfIdentifiedNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "node-a",
		Annotations: map[string]string{
			racerctrl.NodeAgentAnnotation:  racerctrl.NodeAgentRunning,
			racerctrl.NodeIDAnnotation:     "1",
			racerctrl.NodeCohortAnnotation: "0",
		},
	}}
}

// The status annotations are the only feedback channel the operator's
// sequencing gates read. A node that stopped writing them because its own
// config would not render is a node every sequence waits on forever, so the
// render fault has to be reported without taking the counters down with it.
func TestReconcilePublishesStatusWhenTheRenderFails(t *testing.T) {
	agent, client := reconcilingAgent(t, halfIdentifiedNode())
	agent.self.Health = racerctrl.Health{Generation: 7, Shedding: 2}

	err := agent.Reconcile(context.Background())
	if err == nil {
		t.Fatal("reconcile hid a render that could not derive a config")
	}

	got, getErr := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("get node: %v", getErr)
	}

	want := racerctrl.FormatHealth(racerctrl.Health{Generation: 7, Shedding: 2})
	if got.Annotations[racerctrl.NodeHealthAnnotation] != want {
		t.Fatalf("annotation %q = %q, want %q; a render fault silenced the feedback channel",
			racerctrl.NodeHealthAnnotation, got.Annotations[racerctrl.NodeHealthAnnotation], want)
	}
}

// Publishing status after a failed render must not swallow the render error:
// the fault is still this node's, and the reconcile loop only ever sees what is
// returned.
func TestReconcileReportsBothFailures(t *testing.T) {
	agent, client := reconcilingAgent(t, halfIdentifiedNode())

	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("nodes are read-only today")
	})

	err := agent.Reconcile(context.Background())
	if err == nil {
		t.Fatal("reconcile reported success after both the render and the status write failed")
	}

	if !strings.Contains(err.Error(), "no zone yet") {
		t.Fatalf("error %q lost the render fault", err)
	}

	if !strings.Contains(err.Error(), "read-only today") {
		t.Fatalf("error %q lost the status write fault", err)
	}
}

// announcingAgent builds an agent whose lister and client both see one Node,
// which is the only thing the announcement touches.
func announcingAgent(t *testing.T, node *corev1.Node) (*Agent, *fake.Clientset) {
	t.Helper()

	agent := newBindingsAgent(t, t.TempDir())

	index := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := index.Add(node); err != nil {
		t.Fatalf("index node: %v", err)
	}

	agent.nodeLister = corelisters.NewNodeLister(index)

	client := fake.NewClientset(node)
	agent.client = client

	return agent, client
}

// The announcement is what enrols the node, so it has to be written by an agent
// that has no identity yet. An agent that waited for one would wait forever:
// the operator only allocates identity to a node that has announced.
func TestAnnounceWritesTheAgentAnnotation(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}

	agent, client := announcingAgent(t, node)
	if agent.self.ID != 0 {
		t.Fatal("test agent started with an identity; the announcement is meant to precede one")
	}

	if err := agent.announce(context.Background()); err != nil {
		t.Fatalf("announce: %v", err)
	}

	got, err := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}

	if got.Annotations[racerctrl.NodeAgentAnnotation] != racerctrl.NodeAgentRunning {
		t.Fatalf("annotation %q = %q, want %q", racerctrl.NodeAgentAnnotation,
			got.Annotations[racerctrl.NodeAgentAnnotation], racerctrl.NodeAgentRunning)
	}
}

// Announcing runs at the top of every reconcile, so a node that has already
// announced must not be patched again. Writing an unchanged value on every pass
// would put a write per node per interval on the API server for no information.
func TestAnnounceIsSilentOnceItHasBeenWritten(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "node-a",
		Annotations: map[string]string{racerctrl.NodeAgentAnnotation: racerctrl.NodeAgentRunning},
	}}

	agent, client := announcingAgent(t, node)
	client.ClearActions()

	if err := agent.announce(context.Background()); err != nil {
		t.Fatalf("announce: %v", err)
	}

	if actions := client.Actions(); len(actions) != 0 {
		t.Fatalf("announce made %d API calls for a node that had already announced: %v", len(actions), actions)
	}
}
