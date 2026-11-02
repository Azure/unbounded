// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Azure/unbounded/internal/racer/disks"
	"github.com/Azure/unbounded/internal/racer/workload"
)

const ns = "unbounded-system"

func fixture(t *testing.T) string {
	t.Helper()

	b, err := os.ReadFile("../../../../internal/racer/testdata/disk-report.json")
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func node(name string, ann map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: ann}}
}

func pod(name, nodeName, report string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: workload.Labels()},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if report != "" {
		p.Annotations = map[string]string{disks.ReportAnnotation: report}
	}

	return p
}

type harness struct {
	e        *env
	c        *fake.Clientset
	out, err *bytes.Buffer
}

func newHarness(stdin string, terminal bool, objs ...runtime.Object) *harness {
	c := fake.NewClientset(objs...)
	h := &harness{c: c, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	h.e = &env{
		client: c, namespace: ns, in: strings.NewReader(stdin), out: h.out, errOut: h.err,
		isTerminal: func() bool { return terminal }, pollInterval: time.Millisecond,
	}

	return h
}

func (h *harness) annotation(t *testing.T, name string) (string, bool) {
	t.Helper()

	n, err := h.c.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	v, ok := n.Annotations[disks.Annotation]

	return v, ok
}

func TestHumanSize(t *testing.T) {
	for in, want := range map[uint64]string{0: "-", 1: "1B", 1024: "1.0Ki", 1000204886016: "931.5Gi"} {
		if got := humanSize(in); got != want {
			t.Errorf("%d: got %q want %q", in, got, want)
		}
	}
}

func TestListTable(t *testing.T) {
	h := newHarness("", false,
		node("b", map[string]string{disks.Annotation: "wwn-0x5000c500a1b2c3d4", disks.LegacyBlockDevicesAnnotation: ".*"}),
		node("a", nil), node("c", nil),
		pod("p-b", "b", fixture(t)), pod("p-a", "a", `{"shares":1}`), pod("p-c", "c", ""))
	if err := h.e.list(context.Background(), nil, ""); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 6 || !strings.HasPrefix(lines[0], "NODE") || strings.Contains(lines[0], "MODEL") {
		t.Fatalf("table:\n%s", h.out)
	}

	// Sorted by id within node.
	if !strings.Contains(lines[1], "nvme-eui.0025385b71b2c3d4") || !strings.Contains(lines[1], "931.5Gi") {
		t.Fatalf("row 1: %s", lines[1])
	}

	if !strings.Contains(lines[2], "wwn-0x1111") || !strings.Contains(lines[2], "mounted") {
		t.Fatalf("row 2: %s", lines[2])
	}

	for _, w := range []string{"node a:", "predate", "node c:", "has no", "kubectl annotate node b racer.unbounded-cloud.io/block-devices-"} {
		if !strings.Contains(h.err.String(), w) {
			t.Errorf("stderr missing %q:\n%s", w, h.err)
		}
	}
}

func TestListWideAndJSON(t *testing.T) {
	h := newHarness("", false, node("b", map[string]string{disks.Annotation: "x,wwn-0x1111"}), pod("p-b", "b", fixture(t)))
	if err := h.e.list(context.Background(), []string{"b"}, "wide"); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(h.out.String(), "MODEL") || !strings.Contains(h.out.String(), "nvme-Samsung_SSD_980_S64ANS0T123456") {
		t.Fatalf("wide:\n%s", h.out)
	}

	h.out.Reset()

	if err := h.e.list(context.Background(), []string{"b"}, "json"); err != nil {
		t.Fatal(err)
	}

	var got []nodeDisks
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Node != "b" || len(got[0].Disks) != 5 || strings.Join(got[0].Approved, ",") != "x,wwn-0x1111" {
		t.Fatalf("json: %+v", got)
	}

	if err := h.e.list(context.Background(), nil, "yaml"); err == nil {
		t.Fatal("want bad output error")
	}
}

func TestListMissingPod(t *testing.T) {
	h := newHarness("", false, node("a", nil))
	if err := h.e.list(context.Background(), []string{"a"}, "json"); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(h.err.String(), "no racer dataplane pod") {
		t.Fatalf("stderr: %s", h.err)
	}
}

func TestAllowCanonicalizesAndMerges(t *testing.T) {
	h := newHarness("", false, node("n", map[string]string{disks.Annotation: "zzz"}), pod("p", "n", fixture(t)))

	err := h.e.allow(context.Background(), "n", []string{"nvme-Samsung_SSD_980_S64ANS0T123456", "wwn-0x5000c500a1b2c3d4", "nvme-eui.0025385b71b2c3d4"}, allowOpts{yes: true})
	if err != nil {
		t.Fatal(err)
	}

	if v, _ := h.annotation(t, "n"); v != "nvme-eui.0025385b71b2c3d4,wwn-0x5000c500a1b2c3d4,zzz" {
		t.Fatalf("annotation %q", v)
	}

	for _, w := range []string{"ERASE the first and last 1 MiB", "Samsung SSD 980", "S64ANS0T123456", "931.5Gi", "restart"} {
		if !strings.Contains(h.out.String(), w) {
			t.Errorf("output missing %q", w)
		}
	}

	h.out.Reset()

	if err := h.e.allow(context.Background(), "n", []string{"nvme-eui.0025385b71b2c3d4"}, allowOpts{}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(h.out.String(), "already approved") {
		t.Fatalf("out: %s", h.out)
	}
}

func TestAllowRefusals(t *testing.T) {
	h := newHarness("", false, node("n", nil), pod("p", "n", fixture(t)))
	ctx := context.Background()

	if err := h.e.allow(ctx, "n", []string{"wwn-0x1111"}, allowOpts{yes: true}); err == nil || !strings.Contains(err.Error(), "mounted") {
		t.Fatalf("unavailable: %v", err)
	}

	if err := h.e.allow(ctx, "n", []string{"unknown-disk"}, allowOpts{yes: true}); err == nil || !strings.Contains(err.Error(), "not reported") {
		t.Fatalf("unknown: %v", err)
	}

	if err := h.e.allow(ctx, "n", []string{"a/b"}, allowOpts{yes: true}); !errors.Is(err, disks.ErrInvalidID) {
		t.Fatalf("invalid: %v", err)
	}

	if _, ok := h.annotation(t, "n"); ok {
		t.Fatal("annotation set after refusal")
	}

	if err := h.e.allow(ctx, "n", []string{"wwn-0x1111", "unknown-disk"}, allowOpts{yes: true, force: true}); err != nil {
		t.Fatal(err)
	}

	if v, _ := h.annotation(t, "n"); v != "unknown-disk,wwn-0x1111" {
		t.Fatalf("annotation %q", v)
	}
}

func TestAllowPrompt(t *testing.T) {
	ctx := context.Background()
	ids := []string{"nvme-eui.0025385b71b2c3d4"}

	h := newHarness("no\n", true, node("n", nil), pod("p", "n", fixture(t)))
	if err := h.e.allow(ctx, "n", ids, allowOpts{}); !errors.Is(err, errAborted) {
		t.Fatalf("want abort, got %v", err)
	}

	if _, ok := h.annotation(t, "n"); ok {
		t.Fatal("annotation set after abort")
	}

	h = newHarness("", false, node("n", nil), pod("p", "n", fixture(t)))
	if err := h.e.allow(ctx, "n", ids, allowOpts{}); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-terminal: %v", err)
	}

	h = newHarness("yes\n", true, node("n", nil), pod("p", "n", fixture(t)))
	if err := h.e.allow(ctx, "n", ids, allowOpts{}); err != nil {
		t.Fatal(err)
	}

	if v, _ := h.annotation(t, "n"); v != ids[0] {
		t.Fatalf("annotation %q", v)
	}
}

func TestAllowConflictRetry(t *testing.T) {
	h := newHarness("", false, node("n", nil), pod("p", "n", fixture(t)))

	var calls atomic.Int32

	h.c.PrependReactor("update", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		if calls.Add(1) == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "n", errors.New("stale"))
		}

		return false, nil, nil
	})

	if err := h.e.allow(context.Background(), "n", []string{"nvme-eui.0025385b71b2c3d4"}, allowOpts{yes: true}); err != nil {
		t.Fatal(err)
	}

	if calls.Load() != 2 {
		t.Fatalf("update calls %d", calls.Load())
	}

	if v, _ := h.annotation(t, "n"); v != "nvme-eui.0025385b71b2c3d4" {
		t.Fatalf("annotation %q", v)
	}
}

func setState(t *testing.T, report, id, state, reason string) string {
	t.Helper()

	var m map[string]any
	if err := json.Unmarshal([]byte(report), &m); err != nil {
		t.Fatal(err)
	}

	for _, d := range m["disks"].([]any) {
		dm := d.(map[string]any)
		if dm["id"] == id {
			dm["state"], dm["reason"] = state, reason
		}
	}

	b, _ := json.Marshal(m)

	return string(b)
}

func TestAllowWait(t *testing.T) {
	ctx := context.Background()
	id := "nvme-eui.0025385b71b2c3d4"

	// Success: the pod is recreated with a new name reporting claimed.
	h := newHarness("", false, node("n", nil), pod("p", "n", fixture(t)))

	var lists atomic.Int32

	h.c.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if lists.Add(1) < 3 {
			return false, nil, nil
		}

		// Read through the tracker; calling the clientset here would deadlock.
		o, err := h.c.Tracker().Get(corev1.SchemeGroupVersion.WithResource("nodes"), "", "n")
		if err != nil {
			return true, nil, err
		}

		rev := o.(*corev1.Node).Annotations[disks.RevisionAnnotation]
		report := withRevision(t, setState(t, fixture(t), id, "claimed", ""), rev)

		return true, &corev1.PodList{Items: []corev1.Pod{*pod("p2", "n", report)}}, nil
	})

	if err := h.e.allow(ctx, "n", []string{id}, allowOpts{yes: true, wait: true, timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}

	if lists.Load() < 3 {
		t.Fatalf("accepted a report before the new revision: %d lists", lists.Load())
	}

	if !strings.Contains(h.out.String(), id+": claimed") {
		t.Fatalf("out: %s", h.out)
	}

	// Rejected.
	h = newHarness("", false, node("n", nil), pod("p", "n", fixture(t)))
	h.c.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{Items: []corev1.Pod{*pod("p", "n", setState(t, fixture(t), id, "rejected", "wipe-failed"))}}, nil
	})

	err := h.e.waitClaimed(ctx, h.c, ns, "n", "", []string{id}, 5*time.Second)
	if err == nil || !strings.Contains(h.out.String(), "rejected (wipe-failed)") {
		t.Fatalf("rejected: %v %s", err, h.out)
	}

	// Timeout: stays available.
	h = newHarness("", false, node("n", nil), pod("p", "n", fixture(t)))
	if err := h.e.waitClaimed(ctx, h.c, ns, "n", "", []string{id}, 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout: %v", err)
	}
}

func TestAllowWaitIgnoresStaleRevision(t *testing.T) {
	ctx := context.Background()
	id := "wwn-0x2222"

	// A report from before the approval change, here a stale rejection,
	// must not satisfy the wait even though its state is terminal.
	stale := withRevision(t, fixture(t), "old")
	h := newHarness("", false, node("n", nil), pod("p", "n", stale))

	err := h.e.waitClaimed(ctx, h.c, ns, "n", "new", []string{id}, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") || strings.Contains(h.out.String(), "rejected") {
		t.Fatalf("stale: %v %s", err, h.out)
	}

	// The same report is accepted once it carries the requested revision.
	h = newHarness("", false, node("n", nil), pod("p", "n", withRevision(t, fixture(t), "new")))

	err = h.e.waitClaimed(ctx, h.c, ns, "n", "new", []string{id}, 5*time.Second)
	if err == nil || !strings.Contains(h.out.String(), "rejected (too-small)") {
		t.Fatalf("current: %v %s", err, h.out)
	}
}

func TestUpdateApprovedSetsRevision(t *testing.T) {
	ctx := context.Background()
	h := newHarness("", false, node("n", nil))

	add := func(cur []string) []string { return append(cur, "wwn-1") }

	r1, err := updateApproved(ctx, h.c, "n", add)
	if err != nil || r1 == "" {
		t.Fatalf("first: %q %v", r1, err)
	}

	n, err := h.c.CoreV1().Nodes().Get(ctx, "n", metav1.GetOptions{})
	if err != nil || n.Annotations[disks.RevisionAnnotation] != r1 {
		t.Fatalf("annotation: %v %v", n.Annotations, err)
	}

	// No change leaves the revision alone and returns it, so --wait still
	// filters stale reports.
	same := func(cur []string) []string { return cur }
	if r, err := updateApproved(ctx, h.c, "n", same); err != nil || r != r1 {
		t.Fatalf("unchanged: %q %q %v", r1, r, err)
	}

	// An unchanged list with a missing or invalid token gets a fresh one.
	for _, bad := range []string{"", "not valid"} {
		n, err := h.c.CoreV1().Nodes().Get(ctx, "n", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}

		if bad == "" {
			delete(n.Annotations, disks.RevisionAnnotation)
		} else {
			n.Annotations[disks.RevisionAnnotation] = bad
		}

		if _, err := h.c.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}

		r, err := updateApproved(ctx, h.c, "n", same)
		if err != nil || !disks.ValidRevision(r) || r == r1 {
			t.Fatalf("repair %q: %q %v", bad, r, err)
		}

		n, err = h.c.CoreV1().Nodes().Get(ctx, "n", metav1.GetOptions{})
		if err != nil || n.Annotations[disks.RevisionAnnotation] != r || n.Annotations[disks.Annotation] != "wwn-1" {
			t.Fatalf("repair %q annotations: %v %v", bad, n.Annotations, err)
		}
	}

	// Revoking everything still records a new revision.
	r2, err := updateApproved(ctx, h.c, "n", func([]string) []string { return nil })
	if err != nil || r2 == "" || r2 == r1 {
		t.Fatalf("revoke: %q %q %v", r1, r2, err)
	}
}

func withRevision(t *testing.T, report, rev string) string {
	t.Helper()

	var m map[string]any
	if err := json.Unmarshal([]byte(report), &m); err != nil {
		t.Fatal(err)
	}

	m["disks_revision"] = rev
	b, _ := json.Marshal(m)

	return string(b)
}

func TestRevoke(t *testing.T) {
	ctx := context.Background()
	ann := map[string]string{disks.Annotation: "a,wwn-0x5000c500a1b2c3d4,b"}

	h := newHarness("", false, node("n", ann), pod("p", "n", fixture(t)))
	// Alias resolves through the report.
	if err := h.e.revoke(ctx, "n", []string{"nvme-Samsung_SSD_980_S64ANS0T123456", "a"}, false, true); err != nil {
		t.Fatal(err)
	}

	if v, _ := h.annotation(t, "n"); v != "b" {
		t.Fatalf("annotation %q", v)
	}

	if !strings.Contains(h.out.String(), "Nothing is erased") {
		t.Fatalf("out: %s", h.out)
	}

	if err := h.e.revoke(ctx, "n", []string{"zzz"}, false, true); err == nil {
		t.Fatal("want not approved error")
	}

	if err := h.e.revoke(ctx, "n", []string{"b"}, true, true); err == nil {
		t.Fatal("want ids and --all error")
	}

	if err := h.e.revoke(ctx, "n", nil, false, true); err == nil {
		t.Fatal("want missing ids error")
	}

	// Removing the last entry removes the annotation.
	if err := h.e.revoke(ctx, "n", []string{"b"}, false, true); err != nil {
		t.Fatal(err)
	}

	if _, ok := h.annotation(t, "n"); ok {
		t.Fatal("annotation still present")
	}
}

func TestRevokeAliases(t *testing.T) {
	ctx := context.Background()
	pref := "wwn-0x5000c500a1b2c3d4"
	alias := "nvme-Samsung_SSD_980_S64ANS0T123456"

	cases := []struct {
		name, approved, revoke, want string
		noPod                        bool
		wantErr                      bool
	}{
		{name: "preferred ID with only alias approved", approved: alias + ",b", revoke: pref, want: "b"},
		{name: "preferred ID with both approved", approved: pref + "," + alias + ",b", revoke: pref, want: "b"},
		{name: "alias with both approved", approved: pref + "," + alias + ",b", revoke: alias, want: "b"},
		{name: "alias directly", approved: alias + ",b", revoke: alias, want: "b"},
		{name: "no report exact match", approved: alias + ",b", revoke: alias, want: "b", noPod: true},
		{name: "no report other name", approved: alias + ",b", revoke: pref, noPod: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []runtime.Object{node("n", map[string]string{disks.Annotation: tc.approved})}
			if !tc.noPod {
				objs = append(objs, pod("p", "n", fixture(t)))
			}

			h := newHarness("", false, objs...)

			err := h.e.revoke(ctx, "n", []string{tc.revoke}, false, true)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if v, _ := h.annotation(t, "n"); v != tc.want {
				t.Fatalf("annotation %q, want %q", v, tc.want)
			}
		})
	}
}

func TestRevokeAll(t *testing.T) {
	h := newHarness("", false, node("n", map[string]string{disks.Annotation: "a,b"}))
	if err := h.e.revoke(context.Background(), "n", nil, true, true); err != nil {
		t.Fatal(err)
	}

	if _, ok := h.annotation(t, "n"); ok {
		t.Fatal("annotation still present")
	}

	h.out.Reset()

	if err := h.e.revoke(context.Background(), "n", nil, true, true); err != nil || !strings.Contains(h.out.String(), "Nothing to do") {
		t.Fatalf("%v %s", err, h.out)
	}
}

func TestMutationsRefuseInvalidExisting(t *testing.T) {
	ctx := context.Background()
	oversized := "wwn-old" + strings.Repeat(",", disks.MaxAnnotationBytes)

	for name, old := range map[string]string{
		"oversized":     oversized,
		"invalid entry": "wwn-old,bad/id",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness("", false, node("n", map[string]string{disks.Annotation: old}))
			if err := h.e.allow(ctx, "n", []string{"wwn-new"}, allowOpts{yes: true, force: true}); err == nil || !strings.Contains(err.Error(), "revoke --all") {
				t.Fatalf("allow: want invalid existing error, got %v", err)
			}

			if v, _ := h.annotation(t, "n"); v != old {
				t.Fatal("allow modified annotation")
			}

			// Clearing is always permitted.
			if err := h.e.revoke(ctx, "n", nil, true, true); err != nil {
				t.Fatal(err)
			}

			if _, ok := h.annotation(t, "n"); ok {
				t.Fatal("annotation still present")
			}
		})
	}
}

func TestRevokeAllClearsAnnotationWithNoIDs(t *testing.T) {
	ctx := context.Background()

	for name, old := range map[string]string{
		"all commas oversized": strings.Repeat(",", disks.MaxAnnotationBytes+1),
		"empty string":         "",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness("", false, node("n", map[string]string{disks.Annotation: old}))
			if old != "" {
				if err := h.e.allow(ctx, "n", []string{"wwn-new"}, allowOpts{yes: true, force: true}); err == nil || !strings.Contains(err.Error(), "revoke --all") {
					t.Fatalf("allow: want invalid existing error, got %v", err)
				}
			}

			if err := h.e.revoke(ctx, "n", nil, true, true); err != nil {
				t.Fatal(err)
			}

			if _, ok := h.annotation(t, "n"); ok {
				t.Fatal("annotation still present")
			}

			n, err := h.c.CoreV1().Nodes().Get(ctx, "n", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}

			if n.Annotations[disks.RevisionAnnotation] == "" {
				t.Fatal("revision not written")
			}

			// The recovery leaves allow usable.
			if err := h.e.allow(ctx, "n", []string{"wwn-new"}, allowOpts{yes: true, force: true}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRevokeAllAbsentAnnotationNoop(t *testing.T) {
	ctx := context.Background()
	h := newHarness("", false, node("n", nil))

	if err := h.e.revoke(ctx, "n", nil, true, true); err != nil || !strings.Contains(h.out.String(), "Nothing to do") {
		t.Fatalf("%v %s", err, h.out)
	}

	n, err := h.c.CoreV1().Nodes().Get(ctx, "n", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := n.Annotations[disks.RevisionAnnotation]; ok {
		t.Fatal("revision written for no-op")
	}
}

func TestAllowLegacyWarning(t *testing.T) {
	h := newHarness("", false, node("n", map[string]string{disks.LegacyBlockDevicesAnnotation: "nvme.*"}), pod("p", "n", fixture(t)))
	if err := h.e.allow(context.Background(), "n", []string{"nvme-eui.0025385b71b2c3d4"}, allowOpts{yes: true}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(h.err.String(), "no longer honored") {
		t.Fatalf("stderr: %s", h.err)
	}
}

func TestCommandWiring(t *testing.T) {
	cmd := newCommand(newEnv())
	for _, p := range [][]string{{"disks", "list"}, {"disks", "allow"}, {"disks", "revoke"}} {
		if c, _, err := cmd.Find(p); err != nil || c.Name() != p[1] {
			t.Fatalf("%v: %v", p, err)
		}
	}
}
