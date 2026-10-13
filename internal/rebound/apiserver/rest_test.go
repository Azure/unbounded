// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
	"github.com/Azure/unbounded/internal/rebound/ledger"
)

func fixture(t *testing.T) (*storage, *fake.Clientset, context.Context) {
	t.Helper()

	client := fake.NewClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ledger.ConfigMapName, Namespace: "system", ResourceVersion: "1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
	)
	// The fake tracker does not assign resource versions.
	rv := 1

	client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		update, ok := action.(clienttesting.UpdateAction)
		if !ok {
			t.Fatal("expected update")
		}

		cm, ok := update.GetObject().(*corev1.ConfigMap)
		if !ok {
			t.Fatal("expected ConfigMap")
		}

		rv++
		cm.ResourceVersion = fmt.Sprint(rv)

		return false, nil, nil
	})
	s := newStorage(ledger.Store{Client: client, Namespace: "system"}, time.Hour, clocktesting.NewFakeClock(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)))
	s.observe(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ledger.ConfigMapName, Namespace: "system", ResourceVersion: "1"}}, false)
	t.Cleanup(s.Destroy)
	ctx := request.WithUser(request.WithNamespace(t.Context(), "team"), &user.DefaultInfo{Name: "alice"})

	return s, client, ctx
}

func grant(name string, nodes ...string) *rebound.NodeGrant {
	return &rebound.NodeGrant{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"team": "blue"}, Annotations: map[string]string{"note": "test"}}, Spec: rebound.NodeGrantSpec{Nodes: nodes}}
}

func cached(t *testing.T, s *storage) *corev1.ConfigMap {
	t.Helper()

	cm, err := s.store.Client.CoreV1().ConfigMaps("system").Get(t.Context(), ledger.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	s.observe(cm, false)

	return cm
}

func created(t *testing.T, s *storage, ctx context.Context, name string, nodes ...string) *rebound.NodeGrant {
	t.Helper()

	obj, err := s.Create(ctx, grant(name, nodes...), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	g, ok := obj.(*rebound.NodeGrant)
	if !ok {
		t.Fatalf("unexpected object %T", obj)
	}

	cached(t, s)

	return g
}

func TestCreate(t *testing.T) {
	s, _, ctx := fixture(t)

	g := created(t, s, ctx, "first", "node-a")
	if g.Namespace != "team" || g.UID == "" || g.ResourceVersion != "2" || g.Status.Holder != "alice" ||
		g.Spec.TTL.Duration != time.Hour || !g.Status.ExpiresAt.Equal(&metav1.Time{Time: s.clock.Now().Add(time.Hour)}) || g.Annotations["note"] != "test" {
		t.Fatalf("unexpected grant: %+v", g)
	}

	before := cached(t, s).Data

	_, err := s.Create(ctx, grant("overlap", "node-a", "node-b"), nil, nil)
	if !apierrors.IsConflict(err) || !strings.Contains(err.Error(), "node-a") {
		t.Fatalf("expected node conflict, got %v", err)
	}

	if !reflect.DeepEqual(before, cached(t, s).Data) {
		t.Fatal("conflict changed ledger")
	}

	_, err = s.Create(ctx, grant("first", "node-b"), nil, nil)
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("expected AlreadyExists, got %v", err)
	}

	_, err = s.Create(ctx, grant("validation", "node-b"), func(context.Context, runtime.Object) error { return errors.New("denied") }, nil)
	if err == nil || err.Error() != "denied" {
		t.Fatalf("validation: %v", err)
	}

	if !reflect.DeepEqual(before, cached(t, s).Data) {
		t.Fatal("validation changed ledger")
	}
}

func TestCreateInvalid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*rebound.NodeGrant)
	}{
		{"ttl large", func(g *rebound.NodeGrant) { g.Spec.TTL = &metav1.Duration{Duration: 2 * time.Hour} }},
		{"ttl zero", func(g *rebound.NodeGrant) { g.Spec.TTL = &metav1.Duration{} }},
		{"ttl negative", func(g *rebound.NodeGrant) { g.Spec.TTL = &metav1.Duration{Duration: -time.Second} }},
		{"missing node", func(g *rebound.NodeGrant) { g.Spec.Nodes = []string{"missing"} }},
		{"invalid node", func(g *rebound.NodeGrant) { g.Spec.Nodes = []string{"BAD"} }},
		{"empty nodes", func(g *rebound.NodeGrant) { g.Spec.Nodes = nil }},
		{"duplicate nodes", func(g *rebound.NodeGrant) { g.Spec.Nodes = []string{"node-a", "node-a"} }},
		{"invalid name", func(g *rebound.NodeGrant) { g.Name = "has.dot" }},
		{"empty name", func(g *rebound.NodeGrant) { g.Name = "" }},
		{"generate name", func(g *rebound.NodeGrant) { g.GenerateName = "prefix-" }},
		{"invalid label", func(g *rebound.NodeGrant) { g.Labels = map[string]string{"bad key": "value"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, ctx := fixture(t)
			g := grant("test", "node-a")
			tc.change(g)

			_, err := s.Create(ctx, g, nil, nil)
			if !apierrors.IsInvalid(err) {
				t.Fatalf("expected Invalid, got %v", err)
			}

			if len(cached(t, s).Data) != 0 {
				t.Fatal("invalid create wrote ledger")
			}
		})
	}
}

func TestDryRunAndRetry(t *testing.T) {
	s, client, ctx := fixture(t)
	g := grant("test", "node-a")
	g.Spec.TTL = &metav1.Duration{Duration: time.Minute}

	obj, err := s.Create(ctx, g, nil, &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		t.Fatal(err)
	}

	result, ok := obj.(*rebound.NodeGrant)
	if !ok || result.Spec.TTL.Duration != time.Minute {
		t.Fatalf("TTL not preserved: %v", obj)
	}

	if len(cached(t, s).Data) != 0 {
		t.Fatal("dry run wrote data")
	}

	calls := 0

	client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), ledger.ConfigMapName, errors.New("retry"))
		}

		return false, nil, nil
	})
	created(t, s, ctx, "test", "node-a")

	if calls != 2 {
		t.Fatalf("update calls=%d", calls)
	}
}

func TestGetListDelete(t *testing.T) {
	s, _, ctx := fixture(t)
	g := created(t, s, ctx, "first", "node-a")
	created(t, s, request.WithNamespace(ctx, "other"), "second", "node-b")

	obj, err := s.Get(ctx, "first", nil)
	if err != nil {
		t.Fatal(err)
	}

	got, ok := obj.(*rebound.NodeGrant)
	if !ok || got.UID != g.UID {
		t.Fatalf("got %v", obj)
	}

	for _, tc := range []struct {
		ns, label, field string
		count            int
	}{
		{"team", "team=blue", "metadata.name=first", 1},
		{"team", "team=red", "", 0},
		{"", "", "metadata.namespace=other", 1},
		{"", "", "", 2},
		{"team", "", "metadata.name=second", 0},
	} {
		ls, err := labels.Parse(tc.label)
		if err != nil {
			t.Fatal(err)
		}

		listed, err := s.List(request.WithNamespace(ctx, tc.ns), &metainternal.ListOptions{LabelSelector: ls, FieldSelector: fields.ParseSelectorOrDie(tc.field)})
		if err != nil {
			t.Fatal(err)
		}

		list, ok := listed.(*rebound.NodeGrantList)
		if !ok || len(list.Items) != tc.count || list.ResourceVersion != "3" {
			t.Fatalf("list %v: %+v", tc, listed)
		}
	}

	_, err = s.List(ctx, &metainternal.ListOptions{FieldSelector: fields.OneTermEqualSelector("spec.nodes", "node-a")})
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("bad selector: %v", err)
	}

	_, _, err = s.Delete(ctx, "first", nil, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: ptr.To(types.UID("wrong"))}})
	if !apierrors.IsConflict(err) {
		t.Fatalf("UID check: %v", err)
	}

	_, _, err = s.Delete(ctx, "first", func(context.Context, runtime.Object) error { return errors.New("denied") }, nil)
	if err == nil {
		t.Fatal("validation ignored")
	}

	_, _, err = s.Delete(ctx, "first", nil, &metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		t.Fatal(err)
	}

	if len(ledger.Parse(cached(t, s).Data).Grants()) != 2 {
		t.Fatal("dry delete changed ledger")
	}

	obj, deleted, err := s.Delete(ctx, "first", nil, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &g.UID}})
	if err != nil || !deleted || obj == nil {
		t.Fatalf("delete: %v, %v", deleted, err)
	}

	cached(t, s)

	_, _, err = s.Delete(ctx, "first", nil, nil)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("delete missing: %v", err)
	}

	_, err = s.Get(ctx, "first", nil)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get missing: %v", err)
	}
}

func TestExpiredAndDeleteCollection(t *testing.T) {
	s, _, ctx := fixture(t)
	created(t, s, ctx, "first", "node-a")
	created(t, s, ctx, "second", "node-b")

	options := &metainternal.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", "first")}

	_, err := s.DeleteCollection(ctx, nil, &metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}, options)
	if err != nil {
		t.Fatal(err)
	}

	if len(ledger.Parse(cached(t, s).Data).Grants()) != 2 {
		t.Fatal("dry collection delete changed ledger")
	}

	obj, err := s.DeleteCollection(ctx, nil, nil, options)
	if err != nil {
		t.Fatal(err)
	}

	list, ok := obj.(*rebound.NodeGrantList)
	if !ok || len(list.Items) != 1 || list.Items[0].Name != "first" {
		t.Fatalf("collection: %v", obj)
	}

	cached(t, s)

	c, ok := s.clock.(*clocktesting.FakeClock)
	if !ok {
		t.Fatal("expected fake clock")
	}

	c.Step(time.Hour)

	_, err = s.Get(ctx, "second", nil)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expired get: %v", err)
	}

	obj, err = s.List(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	list, ok = obj.(*rebound.NodeGrantList)
	if !ok || len(list.Items) != 0 {
		t.Fatalf("expired list: %v", obj)
	}

	_, err = s.DeleteCollection(ctx, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(ledger.Parse(cached(t, s).Data).Grants()) != 0 {
		t.Fatal("expired grants block namespace cleanup")
	}
}

func event(t *testing.T, w watch.Interface, typ watch.EventType, name, rv string) {
	t.Helper()

	select {
	case e, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("watch closed")
		}

		g, ok := e.Object.(*rebound.NodeGrant)
		if !ok || e.Type != typ || g.Name != name || g.ResourceVersion != rv {
			t.Fatalf("event: %+v, object: %+v", e, e.Object)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch event timed out")
	}
}

func TestWatch(t *testing.T) {
	s, _, ctx := fixture(t)
	created(t, s, ctx, "first", "node-a")

	w, err := s.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	event(t, w, watch.Added, "first", "2")
	created(t, s, ctx, "second", "node-b")
	event(t, w, watch.Added, "second", "3")

	_, _, err = s.Delete(ctx, "first", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	cached(t, s)
	event(t, w, watch.Deleted, "first", "4")

	replay, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Stop()

	event(t, replay, watch.Added, "second", "3")
	event(t, replay, watch.Deleted, "first", "4")

	_, err = s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: "unknown"})
	if !apierrors.IsResourceExpired(err) {
		t.Fatalf("expected 410: %v", err)
	}

	_, err = s.Watch(ctx, &metainternal.ListOptions{SendInitialEvents: ptr.To(true)})
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("watch list: %v", err)
	}

	w.Stop()

	select {
	case _, ok := <-w.ResultChan():
		if ok {
			t.Fatal("watch not closed")
		}
	case <-time.After(time.Second):
		t.Fatal("stop blocked")
	}

	cancelCtx, cancel := context.WithCancel(ctx)

	canceled, err := s.Watch(cancelCtx, &metainternal.ListOptions{ResourceVersion: "4"})
	if err != nil {
		t.Fatal(err)
	}

	cancel()

	select {
	case _, ok := <-canceled.ResultChan():
		if ok {
			t.Fatal("canceled watch not closed")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel leaked watcher")
	}

	replay.Stop()
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.watchers) != 0 {
		t.Fatalf("watchers leaked: %d", len(s.watchers))
	}
}

func TestWatchReplacementSelectorsAndHistory(t *testing.T) {
	s, _, ctx := fixture(t)
	created(t, s, ctx, "first", "node-a")

	w, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: "2", LabelSelector: labels.SelectorFromSet(labels.Set{"team": "blue"}), FieldSelector: fields.OneTermEqualSelector("metadata.name", "first")})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	cm := cached(t, s)
	l := ledger.Parse(cm.Data)

	g, ok := l.Get(types.NamespacedName{Namespace: "team", Name: "first"})
	if !ok {
		t.Fatal("grant missing")
	}

	g.UID = "replacement"
	cm.Data = l.Encode()
	cm.ResourceVersion = "3"
	s.observe(cm, false)
	event(t, w, watch.Deleted, "first", "2")
	event(t, w, watch.Added, "first", "3")

	for i := 4; i < 110; i++ {
		cm.ResourceVersion = fmt.Sprint(i)
		s.observe(cm, false)
	}

	_, err = s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: "2"})
	if !apierrors.IsResourceExpired(err) {
		t.Fatalf("evicted history: %v", err)
	}

	if len(s.history) != historyLimit {
		t.Fatalf("history size %d", len(s.history))
	}
}

func TestTable(t *testing.T) {
	s, _, ctx := fixture(t)
	g := created(t, s, ctx, "test", "node-a", "node-b")

	table, err := s.ConvertToTable(ctx, g, nil)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, col := range table.ColumnDefinitions {
		names = append(names, col.Name)
	}

	if !reflect.DeepEqual(names, []string{"Name", "Nodes", "Holder", "Expires", "Age"}) {
		t.Fatalf("columns %v", names)
	}

	if !reflect.DeepEqual(table.Rows[0].Cells, []interface{}{"test", "node-a,node-b", "alice", "2026-10-13T13:00:00Z", "0s"}) {
		t.Fatalf("cells %v", table.Rows[0].Cells)
	}

	obj, err := s.List(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	table, err = s.ConvertToTable(ctx, obj, &metav1.TableOptions{NoHeaders: true})
	if err != nil || len(table.ColumnDefinitions) != 0 || len(table.Rows) != 1 || table.ResourceVersion != "2" {
		t.Fatalf("table: %+v %v", table, err)
	}
}

func TestCollectionValidationIsAtomic(t *testing.T) {
	s, _, ctx := fixture(t)
	created(t, s, ctx, "first", "node-a")
	created(t, s, ctx, "second", "node-b")
	before := cached(t, s).Data
	calls := 0

	_, err := s.DeleteCollection(ctx, func(_ context.Context, obj runtime.Object) error {
		calls++

		g, ok := obj.(*rebound.NodeGrant)
		if !ok {
			t.Fatal("expected NodeGrant")
		}

		if g.Name == "second" {
			return apierrors.NewConflict(rebound.GroupResource(), g.Name, errors.New("denied"))
		}

		return nil
	}, nil, nil)
	if !apierrors.IsConflict(err) || calls != 2 {
		t.Fatalf("validation conflict was retried: %v, calls %d", err, calls)
	}

	if !reflect.DeepEqual(before, cached(t, s).Data) {
		t.Fatal("failed collection deletion changed ledger")
	}
}

func TestWatchFilterAndSlowConsumer(t *testing.T) {
	s, _, ctx := fixture(t)

	w, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: "1", LabelSelector: labels.SelectorFromSet(labels.Set{"team": "red"})})
	if err != nil {
		t.Fatal(err)
	}

	created(t, s, ctx, "first", "node-a")

	select {
	case e := <-w.ResultChan():
		t.Fatalf("selector mismatch delivered: %v", e)
	default:
	}

	w.Stop()

	slow, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}

	cm := cached(t, s)
	l := ledger.Parse(cm.Data)

	g, ok := l.Get(types.NamespacedName{Namespace: "team", Name: "first"})
	if !ok {
		t.Fatal("missing grant")
	}

	for i := 3; i < 140; i++ {
		g.UID = types.UID(fmt.Sprint(i))
		cm.Data, cm.ResourceVersion = l.Encode(), fmt.Sprint(i)
		s.observe(cm, false)
	}

	s.mu.Lock()
	remaining := len(s.watchers)
	s.mu.Unlock()

	if remaining != 0 {
		t.Fatal("slow consumer not disconnected")
	}

	for range slow.ResultChan() {
	}
}

func TestStorageErrors(t *testing.T) {
	s, client, ctx := fixture(t)
	client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("node API unavailable")
	})

	_, err := s.Create(ctx, grant("test", "node-a"), nil, nil)
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("node failure hidden: %v", err)
	}

	if len(cached(t, s).Data) != 0 {
		t.Fatal("node failure changed ledger")
	}

	if err := storageError("test", ledger.ErrTooLarge); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "ledger full") {
		t.Fatalf("full ledger: %v", err)
	}
}
