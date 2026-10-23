//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

const (
	dataplaneServiceAccount   = "racer-dataplane"
	dataplaneReportAnnotation = "racer.unbounded-cloud.io/report"
	// tokenAuthzProbeKey is an annotation no Racer component owns.
	tokenAuthzProbeKey = "e2e.unbounded-cloud.io/token-authz-probe"
	// tokenAuthzUnrelated names Secrets and Leases Racer must not touch.
	tokenAuthzUnrelated = "racer-e2e-unrelated"
)

// redactToken removes token from err's text so a credential never reaches a
// failure message. API error classification is kept, but status details are
// dropped because they can also contain the token.
func redactToken(err error, token string) error {
	if err == nil {
		return nil
	}

	msg := err.Error()
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "[REDACTED]")
	}

	var status interface{ Status() metav1.Status }
	if errors.As(err, &status) {
		s := status.Status()

		return &apierrors.StatusError{ErrStatus: metav1.Status{
			Status: s.Status, Message: msg, Reason: s.Reason, Code: s.Code,
		}}
	}

	return errors.New(msg)
}

// podTokenClient requests a token for the dataplane ServiceAccount bound to
// pod, the same kind of token the kubelet projects into it, and returns a
// client that authenticates with it. The token stays in memory: it is never
// passed as a command argument, written to disk, or logged.
func (h *harness) podTokenClient(ctx context.Context, admin kubernetes.Interface, pod *corev1.Pod) (kubernetes.Interface, func(error) error) {
	h.t.Helper()

	tr, err := admin.CoreV1().ServiceAccounts(operatorNamespace).CreateToken(ctx, dataplaneServiceAccount,
		&authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{
			ExpirationSeconds: ptr.To[int64](600),
			BoundObjectRef:    &authnv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
		}}, metav1.CreateOptions{})
	if err != nil {
		// The response is unusable, so the error cannot hold a token.
		h.t.Fatalf("request token bound to pod %s: %v", pod.Name, err)
	}

	token := tr.Status.Token
	redact := func(err error) error { return redactToken(err, token) }

	base, err := clientcmd.BuildConfigFromFlags("", h.kubeconfig)
	if err != nil {
		h.t.Fatalf("load kubeconfig: %v", err)
	}

	cfg := rest.AnonymousClientConfig(base)
	cfg.BearerToken = token

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		h.t.Fatalf("build pod token client: %v", redact(err))
	}

	return client, redact
}

// TestDataplaneTokenAuthorization checks what a racer-dataplane pod's own
// bound token may write. The token comes from TokenRequest bound to a live
// pod, so the API server attaches the pod and node extras that admission
// policies match on; impersonation would drop them. Every write is a server
// dry run.
func TestDataplaneTokenAuthorization(t *testing.T) {
	h := cluster(t)
	t.Cleanup(func() { h.dumpDiagnostics() })

	h.applyCache()
	h.installRacer()

	base, err := clientcmd.BuildConfigFromFlags("", h.kubeconfig)
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}

	admin, err := kubernetes.NewForConfig(base)
	if err != nil {
		t.Fatalf("build admin client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var pods []corev1.Pod

	h.waitFor("two running racer-dataplane pods with reports", 5*time.Minute, func() (bool, string) {
		list, err := admin.CoreV1().Pods(operatorNamespace).List(ctx, metav1.ListOptions{LabelSelector: dataplaneLabel})
		if err != nil {
			return false, err.Error()
		}

		pods = pods[:0]

		for _, p := range list.Items {
			if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" && p.Annotations[dataplaneReportAnnotation] != "" {
				pods = append(pods, p)
			}
		}

		return len(pods) >= 2, fmt.Sprintf("running pods with reports: %d", len(pods))
	})

	own, other := &pods[0], &pods[1]
	client, redact := h.podTokenClient(ctx, admin, own)
	dry := metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}}
	reportKey := dataplaneReportAnnotation

	// Log annotation keys only; values are runtime data the suite does not
	// need to show.
	t.Logf("own pod %s annotation keys: %v", own.Name, annotationKeys(own.Annotations))

	patchPod := func(pod *corev1.Pod, patch map[string]any) error {
		body, err := json.Marshal(patch)
		if err != nil {
			t.Fatalf("marshal patch: %v", err)
		}

		_, err = client.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.MergePatchType, body, dry)

		return redact(err)
	}
	annotate := func(key, value string) map[string]any {
		return map[string]any{"metadata": map[string]any{"annotations": map[string]string{key: value}}}
	}

	t.Run("own-pod-report", func(t *testing.T) {
		value, ok := own.Annotations[reportKey]
		if !ok {
			t.Fatalf("own pod has no %s annotation", reportKey)
		}

		// Rewriting the current value keeps the patch valid for whatever
		// schema the runtime uses.
		if err := patchPod(own, annotate(reportKey, value)); err != nil {
			t.Fatalf("own pod report patch denied: %v", err)
		}
	})

	t.Run("other-pod-report", func(t *testing.T) {
		value := other.Annotations[reportKey]
		requireDenied(t, "report patch on other pod "+other.Name, patchPod(other, annotate(reportKey, value)))
	})

	t.Run("own-pod-other-annotation", func(t *testing.T) {
		requireDenied(t, "unrelated annotation on own pod", patchPod(own, annotate(tokenAuthzProbeKey, "x")))
	})

	t.Run("own-pod-spec", func(t *testing.T) {
		// activeDeadlineSeconds may be set on a running pod, so only
		// authorization or admission can reject this.
		requireDenied(t, "spec change on own pod",
			patchPod(own, map[string]any{"spec": map[string]any{"activeDeadlineSeconds": 86400}}))
	})

	t.Run("nodes", func(t *testing.T) {
		body, _ := json.Marshal(annotate(tokenAuthzProbeKey, "x"))
		_, err := client.CoreV1().Nodes().Patch(ctx, own.Spec.NodeName, types.MergePatchType, body, dry)
		requireDenied(t, "node patch", redact(err))

		_, err = client.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: tokenAuthzUnrelated}},
			metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		requireForbidden(t, "node create", redact(err))
	})

	create := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	body, _ := json.Marshal(annotate(tokenAuthzProbeKey, "x"))

	t.Run("secrets", func(t *testing.T) {
		secrets := client.CoreV1().Secrets(operatorNamespace)
		_, err := secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tokenAuthzUnrelated}}, create)
		requireForbidden(t, "unrelated secret create", redact(err))

		// RBAC is checked before the object is loaded, so a missing name
		// still yields Forbidden rather than NotFound.
		_, err = secrets.Patch(ctx, tokenAuthzUnrelated, types.MergePatchType, body, dry)
		requireForbidden(t, "unrelated secret patch", redact(err))

		_, err = secrets.Update(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tokenAuthzUnrelated}},
			metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
		requireForbidden(t, "unrelated secret update", redact(err))
	})

	t.Run("leases", func(t *testing.T) {
		leases := client.CoordinationV1().Leases(operatorNamespace)
		_, err := leases.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: tokenAuthzUnrelated}}, create)
		requireForbidden(t, "unrelated lease create", redact(err))

		_, err = leases.Patch(ctx, tokenAuthzUnrelated, types.MergePatchType, body, dry)
		requireForbidden(t, "unrelated lease patch", redact(err))

		_, err = leases.Update(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: tokenAuthzUnrelated}},
			metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
		requireForbidden(t, "unrelated lease update", redact(err))

		_, err = client.CoordinationV1().Leases("kube-system").Patch(ctx, "kube-controller-manager",
			types.MergePatchType, body, dry)
		requireForbidden(t, "kube-system lease patch", redact(err))
	})
}

// annotationKeys returns the sorted keys of m.
func annotationKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// requireForbidden fails unless err is an RBAC Forbidden error.
func requireForbidden(t *testing.T, what string, err error) {
	t.Helper()

	if err == nil {
		t.Fatalf("%s succeeded, want Forbidden", what)
	}

	if !apierrors.IsForbidden(err) {
		t.Fatalf("%s: want Forbidden, got %v", what, err)
	}
}

// requireDenied fails unless err is a Forbidden or Invalid error, the two
// ways RBAC and validating admission policies reject a request.
func requireDenied(t *testing.T, what string, err error) {
	t.Helper()

	if err == nil {
		t.Fatalf("%s succeeded, want denial", what)
	}

	if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		t.Fatalf("%s: want Forbidden or Invalid, got %v", what, err)
	}
}

// TestRedactToken covers redactToken without a cluster.
func TestRedactToken(t *testing.T) {
	const token = "eyJhbGciOi.secret.sig"

	if redactToken(nil, token) != nil {
		t.Fatal("nil error must stay nil")
	}

	got := redactToken(errors.New("Bearer "+token+" rejected; "+token), token).Error()
	if strings.Contains(got, token) || strings.Count(got, "[REDACTED]") != 2 {
		t.Fatalf("token not redacted: %q", got)
	}

	if got := redactToken(errors.New("plain"), "").Error(); got != "plain" {
		t.Fatalf("empty token changed message: %q", got)
	}
}

func TestRedactTokenPreservesAPIStatus(t *testing.T) {
	const token = "eyJhbGciOi.secret.sig"

	for _, tc := range []struct {
		reason metav1.StatusReason
		code   int32
		check  func(error) bool
	}{
		{metav1.StatusReasonForbidden, 403, apierrors.IsForbidden},
		{metav1.StatusReasonInvalid, 422, apierrors.IsInvalid},
		{metav1.StatusReasonNotFound, 404, apierrors.IsNotFound},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			original := &apierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure, Reason: tc.reason, Code: tc.code,
				Message: "Bearer " + token + " rejected",
				Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{Message: token}}},
			}}

			got := redactToken(fmt.Errorf("request failed: %w", original), token)
			if !tc.check(got) {
				t.Fatalf("redaction lost %s classification: %v", tc.reason, got)
			}

			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}

			if strings.Contains(got.Error(), token) || strings.Contains(string(encoded), token) {
				t.Fatal("redacted API error still contains the token")
			}
		})
	}
}
