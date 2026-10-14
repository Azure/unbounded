// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
	"github.com/Azure/unbounded/internal/rebound/ledger"
)

func TestSelectorResolution(t *testing.T) {
	for _, expressions := range []bool{false, true} {
		name := "matchLabels"
		if expressions {
			name = "matchExpressions"
		}

		t.Run(name, func(t *testing.T) {
			s, client, ctx := fixture(t)
			_, err := client.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Labels: map[string]string{"pool": "b"}}}, metav1.CreateOptions{})
			require.NoError(t, err)

			g := grant("first", "node-b", "node-a")
			if !expressions {
				g.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "a"}}
			}

			g.Status.Nodes = []string{"forged"}
			original := g.Spec.NodeSelector.DeepCopy()
			obj, err := s.Create(ctx, g, nil, nil)
			require.NoError(t, err)

			got, ok := obj.(*rebound.NodeGrant)
			require.True(t, ok)
			require.Equal(t, []string{"node-a", "node-b"}, got.Status.Nodes)
			require.Equal(t, original, got.Spec.NodeSelector)

			read, err := s.Get(ctx, "first", nil)
			require.NoError(t, err)
			require.Equal(t, obj, read)

			// Changing labels after create does not change the stored node set.
			node, err := client.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
			require.NoError(t, err)

			node.Labels = map[string]string{"pool": "b"}
			_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			require.NoError(t, err)
			client.ClearActions()

			read, err = s.Get(ctx, "first", nil)
			require.NoError(t, err)
			require.Equal(t, obj, read)

			for _, action := range client.Actions() {
				require.NotEqual(t, "nodes", action.GetResource().Resource)
			}
		})
	}
}

func TestSelectorValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector *metav1.LabelSelector
		cause    metav1.CauseType
		message  string
	}{
		{name: "missing", cause: metav1.CauseTypeFieldValueRequired, message: "required"},
		{name: "empty", selector: &metav1.LabelSelector{}, cause: metav1.CauseTypeFieldValueInvalid, message: "must not be empty"},
		{name: "bad operator", selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "pool", Operator: "Bad"}}}, cause: metav1.CauseTypeFieldValueInvalid, message: "Bad"},
		{name: "bad key", selector: &metav1.LabelSelector{MatchLabels: map[string]string{"bad key": "a"}}, cause: metav1.CauseTypeFieldValueInvalid, message: "bad key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, client, ctx := fixture(t)
			g := grant("first", "node-a")
			g.Spec.NodeSelector = tc.selector

			client.ClearActions()

			_, err := s.Create(ctx, g, nil, nil)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			status, ok := err.(apierrors.APIStatus)
			require.True(t, ok)
			require.Equal(t, int32(422), status.Status().Code)
			causes := status.Status().Details.Causes
			require.Len(t, causes, 1)
			require.Equal(t, "spec.nodeSelector", causes[0].Field)
			require.Equal(t, tc.cause, causes[0].Type)
			require.Contains(t, causes[0].Message, tc.message)
			require.Empty(t, client.Actions())
		})
	}
}

func TestSelectorPaging(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pages    [][]string
		limit    int
		want     []string
		tooMany  bool
		failPage int
	}{
		{name: "under max", pages: [][]string{{"node-b"}, {"node-a"}}, limit: 3, want: []string{"node-a", "node-b"}},
		{name: "at max", pages: [][]string{{"node-b"}, {"node-a"}}, limit: 2, want: []string{"node-a", "node-b"}},
		{name: "empty first page", pages: [][]string{{}, {"node-a"}}, limit: 2, want: []string{"node-a"}},
		{name: "empty last page", pages: [][]string{{"node-a"}, {}}, limit: 1, want: []string{"node-a"}},
		{name: "over max", pages: [][]string{{"node-a"}, {"node-b", "node-c"}, {"unfetched"}}, limit: 2, tooMany: true},
		{name: "deduplicate", pages: [][]string{{"node-b", "node-a", "node-b"}}, limit: 3, want: []string{"node-a", "node-b"}},
		{name: "later error", pages: [][]string{{"node-a"}, {}}, limit: 2, failPage: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, client, ctx := fixture(t)
			s.maxNodesPerGrant = tc.limit
			g := grant("first")
			g.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "a"}}
			calls, count := 0, 0

			client.PrependReactor("list", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
				list, ok := action.(clienttesting.ListActionImpl)
				require.True(t, ok)

				options := list.GetListOptions()
				require.Equal(t, "pool=a", options.LabelSelector)
				require.Equal(t, int64(tc.limit-count)+1, options.Limit)

				if calls == 0 {
					require.Empty(t, options.Continue)
				} else {
					require.Equal(t, "next", options.Continue)
				}

				require.Less(t, calls, len(tc.pages))
				page := tc.pages[calls]

				calls++
				if calls == tc.failPage {
					return true, nil, apierrors.NewServiceUnavailable("page unavailable")
				}

				result := &corev1.NodeList{}
				for _, name := range page {
					result.Items = append(result.Items, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"pool": "a"}}})
				}

				count += len(page)
				if calls < len(tc.pages) {
					result.Continue = "next"
				}

				return true, result, nil
			})

			obj, err := s.Create(ctx, g, nil, nil)

			switch {
			case tc.tooMany:
				require.True(t, apierrors.IsInvalid(err), "%v", err)
				status, ok := err.(apierrors.APIStatus)
				require.True(t, ok)
				require.Equal(t, int32(422), status.Status().Code)
				require.Equal(t, metav1.CauseTypeTooMany, status.Status().Details.Causes[0].Type)
				require.Equal(t, "spec.nodeSelector", status.Status().Details.Causes[0].Field)
				require.Equal(t, 2, calls)
				require.Empty(t, cached(t, s).Data)
			case tc.failPage != 0:
				require.True(t, apierrors.IsServiceUnavailable(err), "%v", err)
				require.Equal(t, tc.failPage, calls)
				require.Empty(t, cached(t, s).Data)
			default:
				require.NoError(t, err)

				got, ok := obj.(*rebound.NodeGrant)
				require.True(t, ok)
				require.Equal(t, tc.want, got.Status.Nodes)
				require.Equal(t, len(tc.pages), calls)
			}
		})
	}
}

func TestSelectorRetryResolvesNewSnapshot(t *testing.T) {
	s, client, ctx := fixture(t)
	g := grant("first")
	g.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "a"}}
	attempts, lists := 0, 0

	client.PrependReactor("list", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		lists++
		list, ok := action.(clienttesting.ListActionImpl)
		require.True(t, ok)
		require.Equal(t, "pool=a", list.GetListOptions().LabelSelector)
		require.Equal(t, int64(s.maxNodesPerGrant)+1, list.GetListOptions().Limit)

		return false, nil, nil
	})
	client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts == 1 {
			require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("nodes"), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"pool": "b"}}}, ""))
			return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), ledger.ConfigMapName, errors.New("retry"))
		}

		return false, nil, nil
	})

	obj, err := s.Create(ctx, g, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
	require.Equal(t, attempts, lists)

	got, ok := obj.(*rebound.NodeGrant)
	require.True(t, ok)
	require.Equal(t, []string{"node-b"}, got.Status.Nodes)
	require.NotContains(t, cached(t, s).Data, "node-a")
}

func TestGrantObjectSelectorCopies(t *testing.T) {
	g := &ledger.Grant{Nodes: []string{"node-a"}, Selector: &metav1.LabelSelector{
		MatchLabels:      map[string]string{"pool": "a"},
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "zone", Operator: metav1.LabelSelectorOpIn, Values: []string{"a"}}},
	}}
	obj := grantObject(g, "1")
	obj.Spec.NodeSelector.MatchLabels["pool"] = "b"
	obj.Spec.NodeSelector.MatchExpressions[0].Values[0] = "b"
	obj.Status.Nodes[0] = "node-b"

	require.Equal(t, "a", g.Selector.MatchLabels["pool"])
	require.Equal(t, "a", g.Selector.MatchExpressions[0].Values[0])
	require.Equal(t, []string{"node-a"}, g.Nodes)
}
