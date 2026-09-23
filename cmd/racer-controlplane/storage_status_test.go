// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/racer"
)

type statusAPI struct {
	client.Client
	patches int
	fail    bool
}

func (c *statusAPI) Patch(ctx context.Context, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches++
	if c.fail {
		return errors.New("status patch unavailable")
	}

	return c.Client.Patch(ctx, object, patch, opts...)
}

func TestStorageStatusFreshnessBindingAndPatchRate(t *testing.T) {
	f := newCoordinationFixture(t, nil)
	api := &statusAPI{Client: f.api}
	r := newStorageTest(t, api, f.s)
	ctx := context.Background()
	node := &corev1.Node{}

	key := client.ObjectKey{Name: "node"}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	record := f.s.storagePolicies[f.node]
	reportKey := recipient{identityBytes("universe", "default"), identityBytes("node", "node-uid")}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Racer-Boot", strings.Repeat("ab", 32))

	now := time.Now().UTC()
	publish := func(at time.Time) cacheStatus {
		t.Helper()

		if err := api.Get(ctx, key, node); err != nil {
			t.Fatal(err)
		}

		before := node.DeepCopy()
		if err := r.publishStorageStatus(ctx, node, nil, record, at); err != nil {
			t.Fatal(err)
		}

		if nodeChanged(before, node) || storageNodeChanged(before, node) {
			t.Fatal("status fed reconciliation predicates")
		}

		var status cacheStatus
		if err := json.Unmarshal([]byte(node.Annotations[racer.CacheStatusAnnotationKey]), &status); err != nil {
			t.Fatal(err)
		}

		return status
	}

	f.s.storageCommand(req, reportKey, "pod-uid")

	if got := publish(now); got.Phase != "unsupported" || !got.Fresh || got.AppliedBytes != 0 {
		t.Fatalf("legacy: %+v", got)
	}

	req.Header.Set("X-Racer-Storage-Policy", "1")
	req.Header.Set("X-Racer-Storage-Identity", record.Identity)
	req.Header.Set("X-Racer-Storage-Version", "1")
	req.Header.Set("X-Racer-Storage-State", "applied")
	req.Header.Set("X-Racer-Storage-Applied-Bytes", "10737418240")
	req.Header.Set("X-Racer-Storage-Shards", "2")
	f.s.storageCommand(req, reportKey, "pod-uid")

	if got := publish(now); got.Phase != "pending" || got.AppliedBytes != 0 {
		t.Fatalf("unoffered ack: %+v", got)
	}

	f.s.storageCommand(req, reportKey, "pod-uid")

	if got := publish(now); got.Phase != "applied" || got.Shards != 2 || got.AppliedVersion != 1 || got.SelectedPodUID != "pod-uid" {
		t.Fatalf("applied: %+v", got)
	}

	node.Annotations[racer.CacheSizeAnnotationKey] = "invalid"
	record.ValidationError = "invalid desired quantity"

	invalid := f.s.cacheStatus(node, nil, record, now)
	if invalid.Phase != "invalid" || invalid.PolicyPhase != "applied" || invalid.AppliedVersion != 1 || invalid.AppliedBytes != 10<<30 || invalid.RequestedBytes != nil {
		t.Fatalf("invalid desired hid last-good outcome: %+v", invalid)
	}

	delete(node.Annotations, racer.CacheSizeAnnotationKey)

	record.ValidationError = ""

	patches := api.patches

	for i := 1; i <= 59; i++ {
		f.s.storageCommand(req, reportKey, "pod-uid")
		report := f.s.storageReports[reportKey]
		report.Seen = now.Add(time.Duration(i) * time.Second)
		f.s.storageReports[reportKey] = report
		publish(report.Seen)
	}

	if api.patches != patches {
		t.Fatalf("heartbeat patches: %d", api.patches-patches)
	}

	publish(now.Add(time.Minute))

	if api.patches != patches+1 {
		t.Fatal("missing coalesced freshness refresh")
	}

	if got := publish(now.Add(75 * time.Second)); got.Phase != "stale" || got.Fresh || got.AppliedBytes != 0 {
		t.Fatalf("stale ack: %+v", got)
	}

	patches = api.patches

	publish(now.Add(10 * time.Minute))

	if api.patches != patches {
		t.Fatal("unchanged stale status wrote again")
	}

	// A controller restart must overwrite the persisted applied observation.
	f.s.storageReports = nil

	if got := publish(now); got.Phase != "pending" || got.Boot != "" || got.AppliedVersion != 0 {
		t.Fatalf("restart: %+v", got)
	}

	f.s.storageCommand(req, reportKey, "pod-uid")
	f.s.storageCommand(req, reportKey, "pod-uid")
	req.Header.Set("X-Racer-Boot", strings.Repeat("cd", 32))
	f.s.storageCommand(req, reportKey, "pod-uid")

	if got := publish(now); got.Phase != "pending" || got.AppliedBytes != 0 || got.Boot != strings.Repeat("cd", 32) {
		t.Fatalf("new boot: %+v", got)
	}

	req.Header.Set("X-Racer-Storage-State", "failed")
	req.Header.Set("X-Racer-Storage-Error", hex.EncodeToString([]byte("disk\nfull")))
	f.s.storageCommand(req, reportKey, "pod-uid")

	if got := publish(now); got.Phase != "failed" || got.Error != "disk\nfull" || got.AppliedBytes != 10<<30 {
		t.Fatalf("failure: %+v", got)
	}

	selected := f.index.g.Nodes["node"]
	selected.PodUID = "replacement"
	f.index.g.Nodes["node"] = selected

	if got := publish(now); got.Phase != "pending" || got.AppliedBytes != 0 || got.Boot != "" || got.SelectedPodUID != "replacement" {
		t.Fatalf("Pod replacement: %+v", got)
	}
}

func TestStorageStatusPatchCASAndTextBounds(t *testing.T) {
	ctx := context.Background()
	node, _, _ := fixtures()
	api := fakeKube(node)
	r := newStorageTest(t, api, &Server{})

	key := client.ObjectKeyFromObject(node)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	if err := api.Get(ctx, key, node); err != nil {
		t.Fatal(err)
	}

	stale := node.DeepCopy()

	node.Annotations[racer.CacheSizeAnnotationKey] = "12Gi"
	if err := api.Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	record := r.server.storagePolicies[identity("node", string(node.UID))]

	stale.Annotations[racer.CacheStatusAnnotationKey] = "force repair"
	if err := r.publishStorageStatus(ctx, stale, nil, record, time.Now()); !apierrors.IsConflict(err) {
		t.Fatalf("stale patch: %v", err)
	}

	if err := api.Get(ctx, key, node); err != nil {
		t.Fatal(err)
	}

	if node.Annotations[racer.CacheSizeAnnotationKey] != "12Gi" {
		t.Fatal("status patch changed desired input")
	}

	for _, value := range []string{strings.Repeat("λ", 1024), strings.Repeat("x", 1023) + "λ", "invalid\xff"} {
		bounded := storageText(value, 1024)
		if len(bounded) > 1024 || !utf8.ValidString(bounded) {
			t.Fatal("unbounded or invalid UTF-8 diagnostic")
		}

		raw, err := json.Marshal(bounded)
		if err != nil {
			t.Fatal(err)
		}

		var roundtrip string
		if err := json.Unmarshal(raw, &roundtrip); err != nil || roundtrip != bounded {
			t.Fatal("diagnostic serialization would cause repeated patches")
		}
	}
}

func TestStorageStatusInputDeletionInvalidAndPatchRetry(t *testing.T) {
	ctx := context.Background()
	node, _, _ := fixtures()
	api := &statusAPI{Client: fakeKube(node)}
	r := newStorageTest(t, api, &Server{})
	size := resource.MustParse("10Gi")
	site := &machina.Site{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

	if err := api.Get(ctx, client.ObjectKeyFromObject(site), site); err != nil {
		t.Fatal(err)
	}

	site.Spec.Components.Racer.CacheSize = &size
	if err := api.Update(ctx, site); err != nil {
		t.Fatal(err)
	}

	step := func() cacheStatus {
		t.Helper()

		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)}); err != nil {
			t.Fatal(err)
		}

		if err := api.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
			t.Fatal(err)
		}

		var status cacheStatus
		if err := json.Unmarshal([]byte(node.Annotations[racer.CacheStatusAnnotationKey]), &status); err != nil {
			t.Fatal(err)
		}

		return status
	}

	first := step()
	if first.Source != "site" || first.RequestedBytes == nil || *first.RequestedBytes != 10<<30 {
		t.Fatalf("Site: %+v", first)
	}

	node.Annotations[racer.CacheSizeAnnotationKey] = "10240Mi"
	if err := api.Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	if got := step(); got.Source != "node" || got.PolicyVersion != first.PolicyVersion || got.PolicyIdentity != first.PolicyIdentity {
		t.Fatalf("equal override: %+v", got)
	}

	node.Annotations[racer.CacheSizeAnnotationKey] = "invalid"
	if err := api.Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	if got := step(); got.Phase != "invalid" || got.PolicyPhase != "pending" || got.RequestedBytes != nil || got.EffectiveBytes != 10<<30 || got.ValidationError == "" {
		t.Fatalf("invalid last good: %+v", got)
	}

	delete(node.Annotations, racer.CacheSizeAnnotationKey)

	if err := api.Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	if got := step(); got.Source != "site" || got.ValidationError != "" || got.PolicyVersion != first.PolicyVersion {
		t.Fatalf("override deletion: %+v", got)
	}

	if err := api.Delete(ctx, site); err != nil {
		t.Fatal(err)
	}

	api.fail = true

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)}); err == nil {
		t.Fatal("patch failure swallowed")
	}

	api.fail = false

	if got := step(); got.Source != "default" || got.PolicyVersion != first.PolicyVersion {
		t.Fatalf("Site deletion/retry: %+v", got)
	}

	if err := api.Delete(ctx, node); err != nil {
		t.Fatal(err)
	}

	patches := api.patches

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)}); err != nil {
		t.Fatal(err)
	}

	if api.patches != patches {
		t.Fatal("patched deleted Node")
	}
	// Reusing the name cannot reuse the old Node UID's policy or applied state.
	node.ResourceVersion, node.UID, node.Annotations = "", "replacement", nil
	if err := api.Create(ctx, node); err != nil {
		t.Fatal(err)
	}

	if got := step(); got.PolicyIdentity == first.PolicyIdentity || got.AppliedBytes != 0 {
		t.Fatalf("recreated Node: %+v", got)
	}
}
