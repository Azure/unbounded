// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/racer/members"
)

func TestRenderedNodePolicyContract(t *testing.T) {
	for _, namespace := range []string{"unbounded-system", "custom-racer"} {
		t.Run(namespace, func(t *testing.T) {
			data := map[string]string{}
			if namespace != "unbounded-system" {
				data["Namespace"] = namespace
			}

			out := t.TempDir()
			require.NoError(t, render.Render(".", out, data))
			objects := decodeRenderedObjects(t, out, "node-restriction.yaml")
			require.Len(t, objects, 2)

			var policy admissionv1.ValidatingAdmissionPolicy

			encoded, err := json.Marshal(objects[0])
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(encoded, &policy))
			require.NotNil(t, policy.Spec.FailurePolicy)
			require.Equal(t, admissionv1.Fail, *policy.Spec.FailurePolicy)
			require.Len(t, policy.Spec.MatchConditions, 1)
			require.Equal(t, `request.userInfo.username == "system:serviceaccount:`+namespace+`:racer-controller"`, policy.Spec.MatchConditions[0].Expression)
			require.Equal(t, []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
				Operations: []admissionv1.OperationType{admissionv1.Update},
				Rule:       admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"nodes", "nodes/status"}},
			}}}, policy.Spec.MatchConstraints.ResourceRules)

			var binding admissionv1.ValidatingAdmissionPolicyBinding

			encoded, err = json.Marshal(objects[1])
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(encoded, &binding))
			require.Equal(t, policy.Name, binding.Spec.PolicyName)
			require.Equal(t, []admissionv1.ValidationAction{admissionv1.Deny}, binding.Spec.ValidationActions)
			require.Nil(t, binding.Spec.MatchResources)
		})
	}
}

func TestEnvtestRenderedNodePolicy(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API-server Node admission assertions")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	admin, err := client.New(rc, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	for _, namespace := range []string{"unbounded-system", "custom-racer"} {
		t.Run(namespace, func(t *testing.T) {
			ctx := t.Context()

			data := map[string]string{}
			if namespace != "unbounded-system" {
				data["Namespace"] = namespace
			}

			out := t.TempDir()
			require.NoError(t, render.Render(".", out, data))
			require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))

			for _, obj := range decodeRenderedObjects(t, out, "node-restriction.yaml", "rbac.yaml") {
				require.NoError(t, admin.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner("racer-node-policy-test")))
			}

			username := "system:serviceaccount:" + namespace + ":racer-controller"
			asUser := func(username string) *kubernetes.Clientset {
				t.Helper()

				config := rest.CopyConfig(rc)
				config.Impersonate = rest.ImpersonationConfig{UserName: username}
				c, err := kubernetes.NewForConfig(config)
				require.NoError(t, err)

				return c
			}
			controller := asUser(username)
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: namespace + "-worker"}}
			require.NoError(t, admin.Create(ctx, node))

			patch := func(c *kubernetes.Clientset, body string, subresources ...string) error {
				t.Helper()

				_, err := c.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, []byte(body), metav1.PatchOptions{}, subresources...)

				return err
			}
			denied := func(err error) {
				t.Helper()
				require.True(t, apierrors.IsForbidden(err), "%v", err)
				require.Contains(t, err.Error(), "racer-node-write-restriction", "must be denied by admission, not just RBAC")
			}

			require.EventuallyWithT(t, func(c *assert.CollectT) {
				// Envtest has no controller-manager to populate policy type-check status.
				// Exercise both outcomes so fail-closed CEL errors cannot look like success.
				require.NoError(c, admin.Get(ctx, client.ObjectKeyFromObject(node), node))
				node.Spec.Unschedulable = false
				require.NoError(c, admin.Update(ctx, node))
				require.NoError(c, patch(controller, `{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"1"}}}`))
				err := patch(controller, `{"spec":{"unschedulable":true}}`)
				require.True(c, apierrors.IsForbidden(err), "%v", err)
				require.ErrorContains(c, err, "racer-node-write-restriction")
			}, 15*time.Second, 100*time.Millisecond)

			t.Run("empty-fields-and-allowed-annotations", func(t *testing.T) {
				require.NoError(t, patch(controller, `{}`))

				for _, key := range []string{members.EnrolledSharesAnnotation, members.EnrolledRDMANICsAnnotation, members.AdmittedMemberAnnotation} {
					for _, value := range []any{"first", "changed", nil} {
						body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: value}}})
						require.NoError(t, err)
						require.NoError(t, patch(controller, string(body)))
					}
				}

				require.NoError(t, patch(controller, `{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"1","racer.unbounded-cloud.io/enrolled-rdma-nics":"[]","racer.unbounded-cloud.io/last-admitted-member":"saved"}}}`))
				require.NoError(t, patch(controller, `{"metadata":{"annotations":null}}`))
				require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(node), node))
				require.Empty(t, node.Annotations)
			})

			for name, body := range map[string]string{
				"spec":                    `{"spec":{"unschedulable":true}}`,
				"taints":                  `{"spec":{"taints":[{"key":"unrelated","effect":"NoSchedule"}]}}`,
				"pod-cidr":                `{"spec":{"podCIDR":"10.42.0.0/24"}}`,
				"labels":                  `{"metadata":{"labels":{"racer.unbounded-cloud.io/test":"true"}}}`,
				"other-annotation":        `{"metadata":{"annotations":{"unrelated":"true"}}}`,
				"racer-input-annotation":  `{"metadata":{"annotations":{"racer.unbounded-cloud.io/shares":"99"}}}`,
				"racer-prefix-annotation": `{"metadata":{"annotations":{"racer.unbounded-cloud.io/other":"true"}}}`,
				"finalizers":              `{"metadata":{"finalizers":["example.com/hold"]}}`,
				"owner-references":        `{"metadata":{"ownerReferences":[{"apiVersion":"v1","kind":"Node","name":"owner","uid":"11111111-1111-1111-1111-111111111111"}]}}`,
				"mixed":                   `{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"9","unrelated":"true"}},"spec":{"unschedulable":true}}`,
				"mixed-annotations-only":  `{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"9","unrelated":"true"}}}`,
			} {
				t.Run("deny-add-"+name, func(t *testing.T) { denied(patch(controller, body)) })
			}

			// Seed unrelated fields through the administrator, then preserve them on allowed writes.
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(node), node))
			node.Labels = map[string]string{"unrelated": "keep"}
			node.Annotations = map[string]string{"unrelated": "keep", members.EnrolledSharesAnnotation: "1"}
			node.Finalizers = []string{"example.com/hold"}
			node.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: "owner", UID: "11111111-1111-1111-1111-111111111111"}}
			node.Spec.Unschedulable = true
			require.NoError(t, admin.Update(ctx, node))
			node.Status.Phase = corev1.NodeRunning
			require.NoError(t, admin.Status().Update(ctx, node))
			require.NoError(t, patch(controller, `{"metadata":{"annotations":{"racer.unbounded-cloud.io/enrolled-shares":"2"}}}`))
			// Match the optimistic merge patch used by enrollment and recovery writers.
			controllerConfig := rest.CopyConfig(rc)
			controllerConfig.Impersonate = rest.ImpersonationConfig{UserName: username}
			writer, err := client.New(controllerConfig, client.Options{Scheme: scheme.Scheme})
			require.NoError(t, err)
			require.NoError(t, writer.Get(ctx, client.ObjectKeyFromObject(node), node))
			before := node.DeepCopy()
			node.Annotations[members.AdmittedMemberAnnotation] = "saved"
			require.NoError(t, writer.Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))

			for name, body := range map[string]string{
				"spec":                    `{"spec":{"unschedulable":false}}`,
				"labels-change":           `{"metadata":{"labels":{"unrelated":"changed"}}}`,
				"labels-remove":           `{"metadata":{"labels":null}}`,
				"annotations-change":      `{"metadata":{"annotations":{"unrelated":"changed"}}}`,
				"annotations-remove-key":  `{"metadata":{"annotations":{"unrelated":null}}}`,
				"annotations-remove-map":  `{"metadata":{"annotations":null}}`,
				"finalizers-remove":       `{"metadata":{"finalizers":null}}`,
				"owner-references-remove": `{"metadata":{"ownerReferences":null}}`,
				"owner-references-change": `{"metadata":{"ownerReferences":[{"apiVersion":"v1","kind":"Node","name":"other-owner","uid":"22222222-2222-2222-2222-222222222222"}]}}`,
			} {
				t.Run("deny-"+name, func(t *testing.T) { denied(patch(controller, body)) })
			}

			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(node), node))
			require.Equal(t, "keep", node.Labels["unrelated"])
			require.Equal(t, map[string]string{"unrelated": "keep", members.EnrolledSharesAnnotation: "2", members.AdmittedMemberAnnotation: "saved"}, node.Annotations)
			require.True(t, node.Spec.Unschedulable)
			require.Equal(t, corev1.NodeRunning, node.Status.Phase)
			err = controller.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{})
			require.True(t, apierrors.IsForbidden(err), "%v", err)
			_, err = controller.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "forbidden"}}, metav1.CreateOptions{})
			require.True(t, apierrors.IsForbidden(err), "%v", err)
			err = patch(controller, `{"status":{"phase":"Running"}}`, "status")
			require.True(t, apierrors.IsForbidden(err), "%v", err)

			// A test-only grant isolates admission from RBAC and proves identity scoping.
			role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: namespace + "-node-test"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"nodes", "nodes/status"}, Verbs: []string{"get", "patch", "update"}}}}
			require.NoError(t, admin.Create(ctx, role))

			users := []string{username, "system:serviceaccount:other-namespace:racer-controller", "system:serviceaccount:" + namespace + ":unbounded-net-node", "system:serviceaccount:" + namespace + ":racer-dataplane", "system:node:worker"}

			subjects := make([]rbacv1.Subject, 0, len(users))
			for _, user := range users {
				subjects = append(subjects, rbacv1.Subject{Kind: "User", APIGroup: rbacv1.GroupName, Name: user})
			}

			require.NoError(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}, Subjects: subjects}))
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				_, err := asUser(users[1]).CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
				require.NoError(c, err)
			}, 10*time.Second, 100*time.Millisecond)
			denied(patch(controller, `{"status":{"phase":"Terminated"}}`, "status"))
			denied(patch(controller, `{"status":{"phase":null}}`, "status"))
			current, err := controller.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
			require.NoError(t, err)

			current.Annotations[members.AdmittedMemberAnnotation] = "recovery"
			current, err = controller.CoreV1().Nodes().Update(ctx, current, metav1.UpdateOptions{})
			require.NoError(t, err)

			current.Spec.Unschedulable = false
			_, err = controller.CoreV1().Nodes().Update(ctx, current, metav1.UpdateOptions{})
			denied(err)

			for _, user := range users[1:] {
				require.NoError(t, patch(asUser(user), `{"spec":{"unschedulable":false},"metadata":{"labels":{"unrelated":"changed"},"annotations":{"net.unbounded-cloud.io/test":"ok"}}}`), user)
				require.NoError(t, patch(asUser(user), `{"spec":{"unschedulable":true},"metadata":{"labels":{"unrelated":"keep"}}}`), user)
			}

			require.NoError(t, admin.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name}}))
		})
	}
}
