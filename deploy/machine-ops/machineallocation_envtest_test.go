// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestEnvtestMachineAllocationRefPolicy(t *testing.T) {
	env, admin := startPolicyEnvironment(t)
	ctx := context.Background()

	daemon := addPolicyUser(t, env, "daemon", "unbounded-agent-daemons")
	controller := addPolicyUser(t, env, "allocation-controller")

	createAll(
		t, admin,
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "test-daemon-machines"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"unbounded-cloud.io"}, Resources: []string{"machines"}, Verbs: []string{"create"}}},
		},
		clusterRoleBinding("test-daemon-machines", "test-daemon-machines", rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "unbounded-agent-daemons"}),
		clusterRoleBinding("test-allocation-controller", "unbounded-machineallocation-controller", userSubject("allocation-controller")),
	)

	machine := func(name string, bound bool) *unboundedv1alpha3.Machine {
		machine := &unboundedv1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if bound {
			machine.Spec.AllocationRef = &unboundedv1alpha3.UIDReference{Name: "alloc-1", UID: "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b"}
		}

		return machine
	}

	// Policies load asynchronously: wait until a request that must be
	// denied is denied.
	waitForDenial(t, func() error {
		return daemon.Create(ctx, machine("probe", true), client.DryRunAll)
	}, allocationRefPolicyDenial)

	requireDenied(t, daemon.Create(ctx, machine("daemon-bound", true)), allocationRefPolicyDenial)
	require.NoError(t, daemon.Create(ctx, machine("daemon-unbound", false)))
	require.NoError(t, controller.Create(ctx, machine("controller-bound", true)))
}

func TestEnvtestMachineAllocationBootstrapTokenPolicy(t *testing.T) {
	env, admin := startPolicyEnvironment(t)
	ctx := context.Background()

	controller := addPolicyUser(t, env, "allocation-controller")
	withoutVerb := addPolicyUser(t, env, "token-tool")
	clusterAdmin := addPolicyUser(t, env, "admin-user")

	createAll(
		t, admin,
		clusterRoleBinding("test-allocation-controller", "unbounded-machineallocation-controller", userSubject("allocation-controller")),
		roleBinding(metav1.NamespaceSystem, "test-allocation-controller-tokens", "ClusterRole", "unbounded-machineallocation-controller-bootstrap-tokens", userSubject("allocation-controller")),
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: "test-secrets-create"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}}},
		},
		roleBinding(metav1.NamespaceDefault, "test-allocation-controller-default", "Role", "test-secrets-create", userSubject("allocation-controller")),
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: "test-token-tool"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create", "delete"}}},
		},
		roleBinding(metav1.NamespaceSystem, "test-token-tool", "Role", "test-token-tool", userSubject("token-tool")),
		clusterRoleBinding("test-admin-user", "cluster-admin", userSubject("admin-user")),
	)

	secret := func(namespace, name string, secretType corev1.SecretType, labels map[string]string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels}, Type: secretType}
	}
	labeled := map[string]string{unboundedv1alpha3.LabelAllocationUID: "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b"}

	waitForDenial(t, func() error {
		return controller.Create(ctx, secret(metav1.NamespaceSystem, "bootstrap-token-probe0", corev1.SecretTypeBootstrapToken, nil), client.DryRunAll)
	}, bootstrapTokenDenial)

	// The controller creates and deletes only its labeled bootstrap tokens.
	token := secret(metav1.NamespaceSystem, "bootstrap-token-alloc1", corev1.SecretTypeBootstrapToken, labeled)
	require.NoError(t, controller.Create(ctx, token))
	require.NoError(t, controller.Delete(ctx, token))

	requireDenied(t, controller.Create(ctx, secret(metav1.NamespaceSystem, "bootstrap-token-nolab1", corev1.SecretTypeBootstrapToken, nil)), bootstrapTokenDenial)
	requireDenied(t, controller.Create(ctx, secret(metav1.NamespaceSystem, "bootstrap-token-empty1", corev1.SecretTypeBootstrapToken,
		map[string]string{unboundedv1alpha3.LabelAllocationUID: ""})), bootstrapTokenDenial)
	requireDenied(t, controller.Create(ctx, secret(metav1.NamespaceSystem, "opaque-labeled", corev1.SecretTypeOpaque, labeled)), bootstrapTokenDenial)

	unlabeledToken := secret(metav1.NamespaceSystem, "bootstrap-token-site01", corev1.SecretTypeBootstrapToken, nil)
	opaque := secret(metav1.NamespaceSystem, "other-component", corev1.SecretTypeOpaque, nil)
	createAll(t, admin, unlabeledToken.DeepCopy(), opaque.DeepCopy())
	requireDenied(t, controller.Delete(ctx, unlabeledToken), bootstrapTokenDenial)
	requireDenied(t, controller.Delete(ctx, opaque), bootstrapTokenDenial)

	// Outside kube-system the policy does not apply.
	require.NoError(t, controller.Create(ctx, secret(metav1.NamespaceDefault, "unlabeled", corev1.SecretTypeOpaque, nil)))

	// An identity without the verb is unaffected.
	for _, unlabeled := range []*corev1.Secret{
		secret(metav1.NamespaceSystem, "bootstrap-token-tool01", corev1.SecretTypeBootstrapToken, nil),
		secret(metav1.NamespaceSystem, "tool-opaque", corev1.SecretTypeOpaque, nil),
	} {
		require.NoError(t, withoutVerb.Create(ctx, unlabeled))
		require.NoError(t, withoutVerb.Delete(ctx, unlabeled))
	}

	// cluster-admin holds "*" on machines and is exempt.
	adminToken := secret(metav1.NamespaceSystem, "bootstrap-token-admin1", corev1.SecretTypeBootstrapToken, nil)
	require.NoError(t, clusterAdmin.Create(ctx, adminToken))
	require.NoError(t, clusterAdmin.Delete(ctx, adminToken))
}

// startPolicyEnvironment starts an API server with the Machine CRDs, applies
// the allocation roles and both admission policies, and returns an admin
// client.
func startPolicyEnvironment(t *testing.T) (*envtest.Environment, client.Client) {
	t.Helper()

	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API server tests")
	}

	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "machina", "crd")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	admin, err := client.New(cfg, client.Options{Scheme: policyScheme(t)})
	require.NoError(t, err)

	for _, file := range []string{allocationRBACFile, allocationRefPolicyFile, bootstrapTokenPolicyFile} {
		for _, document := range manifestDocuments(t, file) {
			object := &unstructured.Unstructured{}
			require.NoError(t, yaml.Unmarshal([]byte(document), &object.Object))
			require.NoError(t, admin.Create(context.Background(), object), "apply %s %s", object.GetKind(), object.GetName())
		}
	}

	return env, admin
}

func policyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, unboundedv1alpha3.AddToScheme(scheme))

	return scheme
}

func addPolicyUser(t *testing.T, env *envtest.Environment, name string, groups ...string) client.Client {
	t.Helper()

	user, err := env.AddUser(envtest.User{Name: name, Groups: groups}, nil)
	require.NoError(t, err)

	c, err := client.New(user.Config(), client.Options{Scheme: policyScheme(t)})
	require.NoError(t, err)

	return c
}

func createAll(t *testing.T, c client.Client, objects ...client.Object) {
	t.Helper()

	for _, object := range objects {
		require.NoError(t, c.Create(context.Background(), object), "create %T %s", object, object.GetName())
	}
}

func userSubject(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}
}

func clusterRoleBinding(name, clusterRole string, subject rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects:   []rbacv1.Subject{subject},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole},
	}
}

func roleBinding(namespace, name, kind, role string, subject rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Subjects:   []rbacv1.Subject{subject},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kind, Name: role},
	}
}

// waitForDenial retries request until the policy denies it with message.
func waitForDenial(t *testing.T, request func() error, message string) {
	t.Helper()

	require.Eventually(t, func() bool {
		err := request()
		return err != nil && strings.Contains(err.Error(), message)
	}, time.Minute, 250*time.Millisecond, "the admission policy did not start denying requests")
}

func requireDenied(t *testing.T, err error, message string) {
	t.Helper()

	require.Error(t, err)
	require.True(t, apierrors.IsForbidden(err), "unexpected error %v", err)
	require.Contains(t, err.Error(), message)
}
