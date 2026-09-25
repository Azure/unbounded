// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type cachedScaleClient struct {
	client.Client
	reader client.Reader
}

func (c cachedScaleClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.reader.Get(ctx, key, obj, opts...)
}

func (c cachedScaleClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, list, opts...)
}

// The informer uses a synthetic list/watch HTTP source, but the cache, field
// index, deep copies, reconciler, canonical hashes, encoding and waiting are real.
// Only durable version CAS uses a fake client. This is deliberately not an HTTPS
// authentication or API-server capacity benchmark.
func TestServerScale(t *testing.T) {
	if os.Getenv("RACER_SCALE") != "1" {
		t.Skip("set RACER_SCALE=1 for 100,000-member reconciliation and waiter measurements")
	}

	for _, count := range []int{1_000, 10_000, 100_000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r := initializedTopology(t)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			reader := scaleCache(t, r, count)
			r.Client = cachedScaleClient{Client: r.Client, reader: reader}

			runtime.GC()

			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			start := time.Now()
			first := reconcileTopology(t, r, ctx)
			cold := time.Since(start)

			runtime.ReadMemStats(&after)

			if len(r.Accepted) != count {
				t.Fatalf("accepted %d of %d", len(r.Accepted), count)
			}

			t.Logf("members=%d cold_reconcile=%s allocated_bytes=%d publication_bytes=%d", count, cold, after.TotalAlloc-before.TotalAlloc, len(first.Encoding()))

			start = time.Now()

			if current := reconcileTopology(t, r, ctx); current != first {
				t.Fatal("no-op reconcile replaced publication")
			}

			t.Logf("members=%d unchanged_reconcile=%s", count, time.Since(start))

			if count == 100_000 {
				scaleFanout(t, r, ctx, count)
			}
		})
	}
}

func scaleCache(t *testing.T, r *TopologyReconciler, count int) cache.Cache {
	t.Helper()

	nodes := &corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}}
	pods := &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}}

	for i := range count {
		uid := types.UID(fmt.Sprintf("%08x-0000-4000-8000-000000000000", i))
		node := memberNode()
		node.Name, node.UID, node.ResourceVersion = fmt.Sprintf("node-%d", i), uid, "1"
		node.Annotations = map[string]string{wire.RailsAnnotation: `[{"rail":0,"fabric":"rack-a","numa_node":0},{"rail":1,"fabric":"rack-b","numa_node":1}]`}
		pod := memberPod(uid, 1, fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&255, i&255))
		pod.Spec.NodeName, pod.ResourceVersion = node.Name, "1"
		pod.OwnerReferences[0].Name = r.Config.DaemonSetName

		nodes.Items, pods.Items = append(nodes.Items, node), append(pods.Items, pod)
	}

	ds := &appsv1.DaemonSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSetList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []appsv1.DaemonSet{{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: r.Config.DaemonSetName, UID: testDaemonSetUID, ResourceVersion: "1"}}}}

	caches := &racerv1.ClusterCacheList{TypeMeta: metav1.TypeMeta{APIVersion: racerv1.GroupVersion.String(), Kind: "ClusterCacheList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}}
	for i := range 16 {
		caches.Items = append(caches.Items, catalogCache(fmt.Sprintf("cache-%d", i), types.UID(fmt.Sprintf("%08x-1111-4000-8000-000000000000", i)), nil))
	}

	lists := map[string]any{
		"/api/v1/nodes": nodes,
		"/api/v1/namespaces/" + r.Config.Namespace + "/pods":             pods,
		"/apis/apps/v1/namespaces/" + r.Config.Namespace + "/daemonsets": ds,
		"/apis/" + racerv1.GroupVersion.String() + "/clustercaches":      caches,
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if req.URL.Query().Get("sendInitialEvents") == "true" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonBadRequest, Code: 400, Message: "synthetic source supports ordinary list/watch"})

			return
		}

		if req.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			http.NewResponseController(w).Flush()
			<-req.Context().Done()

			return
		}

		list, ok := lists[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}

		json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(source.Close)

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion, appsv1.SchemeGroupVersion, racerv1.GroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Node"), meta.RESTScopeRoot)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("PodList"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	mapper.Add(appsv1.SchemeGroupVersion.WithKind("DaemonSet"), meta.RESTScopeNamespace)
	mapper.Add(racerv1.GroupVersion.WithKind("ClusterCache"), meta.RESTScopeRoot)

	options := managerOptions(r.Config, r.Scheme()).Cache
	options.Scheme, options.Mapper = r.Scheme(), mapper

	reader, err := cache.New(&rest.Config{Host: source.URL, QPS: 1000, Burst: 1000}, options)
	if err != nil {
		t.Fatal(err)
	}

	if err := reader.IndexField(t.Context(), &corev1.Pod{}, podNodeIndex, podNodeKeys); err != nil {
		t.Fatal(err)
	}

	for _, obj := range []client.Object{&corev1.Node{}, &appsv1.DaemonSet{}, &racerv1.ClusterCache{}} {
		if _, err := reader.GetInformer(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() { done <- reader.Start(ctx) }()

	t.Cleanup(func() {
		cancel()

		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	syncCtx, stopSync := context.WithTimeout(ctx, time.Minute)
	defer stopSync()

	if !reader.WaitForCacheSync(syncCtx) {
		t.Fatal("scale informer did not synchronize")
	}

	return reader
}

func scaleFanout(t *testing.T, r *TopologyReconciler, ctx context.Context, count int) {
	t.Helper()

	current, err := r.Publications.Current()
	if err != nil {
		t.Fatal(err)
	}

	cm, previous, err := r.readVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Keep both realistic full-size encodings alive. Prepare before admission so
	// fanout measures Install plus delivery, independent of canonical encoding.
	members := make(AcceptedMembers, count)

	for id, member := range r.Accepted {
		member.Shares++
		members[id] = member
	}

	prepared, err := r.Publications.Prepare(previous, cm.ResourceVersion, members, nil)
	if err != nil {
		t.Fatal(err)
	}

	next, err := r.CommitVersion(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}

	sequence := current.Version().Sequence

	waiting, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan *CommittedPublication, count)
	failures := make(chan error, count)

	var wg sync.WaitGroup

	runtime.GC()
	runtime.GC() // Clear temporary encoding sync.Pools before the waiter baseline.

	var before, parked runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()

	for id := range members {
		identity := pollIdentity(r.Config, id)

		wg.Go(func() {
			p, err := r.Publications.Wait(waiting, identity, &sequence)
			results <- p

			failures <- err
		})
	}

	defer wg.Wait()
	defer cancel()

	eventually(t, "100000 admitted waiters", func() bool {
		r.Publications.mu.Lock()
		defer r.Publications.mu.Unlock()

		return len(r.Publications.polls) == count
	})

	admit := time.Since(start)

	runtime.GC()
	runtime.ReadMemStats(&parked)

	if _, err := r.Publications.Wait(ctx, pollIdentity(r.Config, testOtherUID), &sequence); !errors.Is(err, wire.Overloaded) {
		t.Fatalf("global bound failed: %v", err)
	}

	for id := range members {
		if _, err := r.Publications.Wait(ctx, pollIdentity(r.Config, id), &sequence); !errors.Is(err, wire.Overloaded) {
			t.Fatalf("duplicate bound failed: %v", err)
		}

		break
	}

	start = time.Now()

	if err := r.Publications.Install(next); err != nil {
		t.Fatal(err)
	}

	install := time.Since(start)

	wg.Wait()

	fanout := time.Since(start)

	for range count {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}

		if <-results != next {
			t.Fatal("waiter missed or copied full publication")
		}
	}

	awaitPolls(t, r.Publications, 0)
	t.Logf("waiters=%d GOMAXPROCS=%d admission=%s install=%s all_delivered=%s heap_delta=%d stack_delta=%d next_bytes=%d", count, runtime.GOMAXPROCS(0), admit, install, fanout, int64(parked.HeapAlloc)-int64(before.HeapAlloc), int64(parked.StackInuse)-int64(before.StackInuse), len(next.Encoding()))
}
