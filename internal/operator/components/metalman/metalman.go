// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package metalman implements the per-Site metalman PXE controller component.
package metalman

import (
	"context"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	"github.com/Azure/unbounded/internal/operator/component"
)

// Component reconciles the per-Site metalman PXE controller.
type Component struct{}

// New returns the metalman per-Site component.
func New() component.SiteComponent { return Component{} }

// Name implements component.SiteComponent.
func (Component) Name() string { return "metalman" }

// ConditionType implements component.SiteComponent.
func (Component) ConditionType() string { return "MetalmanReady" }

// Enabled reports whether the Site enables metalman.
func (Component) Enabled(site *unboundedv1alpha3.Site) bool {
	if site.Spec.Components.Metalman == nil {
		return false
	}

	return unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.Metalman.SiteComponentSpec)
}

// Plan deploys the per-site metalman PXE controller and its RBAC.
//
// The support RBAC ships in the machina manifest set and is identical for every
// Site, so it carries a shared key and the executor writes it once per pass
// rather than once per Site.
func (c Component) Plan(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	support, err := env.DecodeManifestFS(machinamanifests.Manifests, mutateSupportObject)
	if err != nil {
		return nil, component.Result{}, err
	}

	plan := component.NewPlan()

	restriction := addSupportOps(ctx, env, plan, support, c.Name(), site.Name)

	plan.Add(component.Operation{
		Kind:        component.OpApply,
		Object:      component.ToUnstructured(deployment(site, env.Namespace, env.Config)),
		Component:   c.Name(),
		Site:        site.Name,
		Overridable: true,
		DependsOn:   restriction,
	})

	return plan, component.Reconciled(), nil
}

// BootstrapMachineBindingName is the ClusterRoleBinding that grants the
// metalman-bootstrap ServiceAccount write access to Machine status. It must not
// be applied until the admission policy restricting that access is in place.
const BootstrapMachineBindingName = "metalman:bootstrap-machine"

// addSupportOps adds the shared support RBAC, the admission restriction, and
// the stale RBAC deletes to plan, and returns the admission refs that anything
// relying on the restriction must depend on. Plan and CleanupPlan emit the same
// operations, so the shared keys dedupe whether a Site is enabled or not.
//
// The live restriction is verified first. An apply cannot remove fields that
// another field manager added, and the executor skips applies whose desired
// fields already match, so a live policy or binding with extra matching
// constraints would otherwise be reported as applied. When the restriction
// cannot be verified, the bootstrap grant is deleted rather than applied, and
// drifted specs are replaced in full. The grant returns on a later pass once
// the live restriction matches.
func addSupportOps(ctx context.Context, env *component.Env, plan *component.Plan, support []*unstructured.Unstructured, componentName, site string) []component.ObjectRef {
	// The executor ranks RBAC and Deployments ahead of admission policies, so
	// without an explicit dependency the bootstrap grant and the token-minting
	// Deployment would go live before the policy that restricts them, and stay
	// live if the policy failed to apply.
	restriction := admissionRefs(support)
	repairs, verified := verifyRestriction(ctx, env, support)

	for _, obj := range support {
		if repair, ok := repairs[component.RefOf(obj)]; ok {
			repair.Component, repair.Site, repair.SharedKey = componentName, site, sharedSupportKey(obj)
			plan.Add(repair)

			continue
		}

		op := component.Operation{
			Kind:      component.OpApply,
			Object:    obj,
			Component: componentName,
			Site:      site,
			SharedKey: sharedSupportKey(obj),
		}
		if obj.GetKind() == "ClusterRoleBinding" && obj.GetName() == BootstrapMachineBindingName {
			if !verified {
				op = component.DeleteOperation(obj.DeepCopy(), componentName, site)
				op.SharedKey = sharedSupportKey(obj)
				plan.Add(op)

				continue
			}

			// An earlier release may already have installed the grant, so
			// skipping the apply is not enough: revoke it until the
			// restriction is back.
			op.DependsOn = restriction
			op.RevokeOnDependencyFailure = true
		}

		plan.Add(op)
	}

	// Remove RBAC that earlier releases installed and that is now unsafe to
	// keep. Deletes are idempotent, so they run on every pass.
	for _, op := range staleSupportDeletes(componentName, site) {
		plan.Add(op)
	}

	return restriction
}

// verifyRestriction compares the live admission policy and binding with the
// desired support objects. It returns merge patch repairs, keyed by object,
// that replace a drifted spec in full, and whether the grant may be applied in
// this pass. A missing object is safe to grant alongside, because its apply
// creates it exactly as desired and the grant depends on that apply. Drift, a
// terminating object, or a failed read withholds the grant.
func verifyRestriction(ctx context.Context, env *component.Env, support []*unstructured.Unstructured) (map[component.ObjectRef]component.Operation, bool) {
	repairs := map[component.ObjectRef]component.Operation{}
	verified := true

	reader := env.LiveReader()
	if reader == nil {
		return repairs, false
	}

	for _, obj := range support {
		var expected, current client.Object

		switch obj.GetKind() {
		case "ValidatingAdmissionPolicy":
			expected, current = &admissionv1.ValidatingAdmissionPolicy{}, &admissionv1.ValidatingAdmissionPolicy{}
		case "ValidatingAdmissionPolicyBinding":
			expected, current = &admissionv1.ValidatingAdmissionPolicyBinding{}, &admissionv1.ValidatingAdmissionPolicyBinding{}
		default:
			continue
		}

		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, expected); err != nil {
			verified = false
			continue
		}

		err := reader.Get(ctx, client.ObjectKeyFromObject(expected), current)

		switch {
		case apierrors.IsNotFound(err):
		case err != nil, current.GetDeletionTimestamp() != nil:
			verified = false
		case !equality.Semantic.DeepEqual(component.EffectiveAdmissionSpec(expected), component.EffectiveAdmissionSpec(current)):
			verified = false
			base := component.ToUnstructured(current)
			base.SetGroupVersionKind(obj.GroupVersionKind())
			desired := base.DeepCopy()
			// The merge patch is computed against the live object, so fields
			// absent from the desired spec are sent as null and removed.
			desired.Object["spec"] = obj.DeepCopy().Object["spec"]
			repairs[component.RefOf(obj)] = component.Operation{Kind: component.OpMergePatch, Base: base, Object: desired}
		}
	}

	return repairs, verified
}

// admissionRefs returns the refs of the admission policy objects in support.
func admissionRefs(support []*unstructured.Unstructured) []component.ObjectRef {
	var refs []component.ObjectRef

	for _, obj := range support {
		switch obj.GetKind() {
		case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
			refs = append(refs, component.RefOf(obj))
		}
	}

	return refs
}

// CleanupPlan removes the per-site metalman Deployment. The shared metalman
// RBAC is kept because other Sites may use it, and bootstrap tokens minted
// earlier stay valid after the Deployment is gone. The shared support set is
// therefore planned exactly as in Plan: the admission restriction is applied,
// the retained bootstrap grant is revoked if the restriction fails, and RBAC
// that is no longer shipped is removed.
func (c Component) CleanupPlan(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	support, err := env.DecodeManifestFS(machinamanifests.Manifests, mutateSupportObject)
	if err != nil {
		return nil, component.Result{}, err
	}

	plan := component.NewPlan()

	// The Deployment delete gets its own failure domain. Removals of workloads
	// rank ahead of RBAC removals and writes, so in the shared domain a failed
	// Deployment delete would skip the stale auto-approval delete and the
	// restriction applies below for as long as the failure persists.
	remove := component.DeleteOperation(&appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: DeploymentName(site.Name), Namespace: env.Namespace},
	}, c.Name(), site.Name)
	remove.FailureDomain = deploymentFailureDomain
	plan.Add(remove)
	addSupportOps(ctx, env, plan, support, c.Name(), site.Name)

	return plan, component.Disabled("component disabled"), nil
}

// deploymentFailureDomain isolates the per-site Deployment delete so its
// failure does not gate the shared support operations.
const deploymentFailureDomain = "deployment"

// StaleAutoApproveBindingName is the ClusterRoleBinding that earlier releases
// installed to let kube-controller-manager auto-approve kubelet client
// certificates for any node requested with a metalman-bootstrap token. Machina
// now approves those requests only for the attested Machine, so the binding
// must be removed on upgrade.
const StaleAutoApproveBindingName = "metalman:node-autoapprove-bootstrap"

// staleSupportDeletes returns delete operations for shared metalman RBAC that
// is no longer shipped. They carry a shared key so every Site plans the same
// delete and the executor issues it once per pass.
func staleSupportDeletes(componentName, site string) []component.Operation {
	op := component.DeleteOperation(&rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: StaleAutoApproveBindingName},
	}, componentName, site)
	op.SharedKey = sharedSupportKey(op.Object)

	return []component.Operation{op}
}

// sharedSupportKey identifies a support object across Sites. The objects are
// byte-identical for every Site, so the key is just their identity.
func sharedSupportKey(obj *unstructured.Unstructured) string {
	return "metalman/support/" + component.RefOf(obj).String()
}

// SetupWatches recreates the per-site Deployment if it is deleted or drifts, via
// its controller owner reference to the Site. The predicate drops status-only
// updates so pod-count churn does not re-apply the Deployment.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Owns(&appsv1.Deployment{}, builder.WithPredicates(env.OwnedWorkloadPredicate()))
}

// DeploymentName is the per-site metalman Deployment name.
func DeploymentName(site string) string { return "metalman-controller-" + site }

// SupportObjectNameSubstring identifies the metalman RBAC objects that ship in
// the machina manifest set. It is exported so the machina component can skip
// exactly the objects the metalman component owns and applies.
const SupportObjectNameSubstring = "metalman"

// IsSupportObject reports whether obj is the metalman RBAC or admission policy
// that ships in the machina manifests. The machina component skips these; the
// metalman component applies them.
func IsSupportObject(obj *unstructured.Unstructured) bool {
	switch obj.GetKind() {
	case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
		return strings.Contains(obj.GetName(), SupportObjectNameSubstring)
	default:
		return component.IsRBACObject(obj, SupportObjectNameSubstring)
	}
}

// mutateSupportObject keeps only the metalman RBAC objects from the machina
// manifests (they are already rendered into the operator namespace).
func mutateSupportObject(obj *unstructured.Unstructured) error {
	if !IsSupportObject(obj) {
		obj.Object = nil
	}

	return nil
}

func deployment(site *unboundedv1alpha3.Site, namespace string, cfg component.Config) *appsv1.Deployment {
	image := cfg.Image("metalman")
	name := DeploymentName(site.Name)
	labels := map[string]string{
		"app":                                 "unbounded-pxe",
		"app.kubernetes.io/name":              "metalman-controller",
		"app.kubernetes.io/component":         "metalman",
		unboundedv1alpha3.MachineSiteLabelKey: site.Name,
	}

	args := []string{"serve-pxe", "--site=" + site.Name}
	if site.Spec.Components.Metalman.DHCPAutoInterface != nil && *site.Spec.Components.Metalman.DHCPAutoInterface {
		args = append(args, "--dhcp-auto-interface")
	}

	replicas := int32(1)
	if site.Spec.Components.Metalman.Replicas != nil {
		replicas = *site.Spec.Components.Metalman.Replicas
	}

	env := []corev1.EnvVar{{
		// Metalman resolves its leader-election lease namespace from
		// POD_NAMESPACE so the lease and its namespace-scoped RBAC stay
		// co-located with the Deployment under any install namespace.
		Name: "POD_NAMESPACE",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		},
	}}

	// Advertise the operator-resolved API server endpoint to metalman so the
	// kubeconfig it serves to provisioning nodes matches what machina writes,
	// and so metalman does not need to rediscover it (managed control planes
	// such as AKS do not publish kube-public/cluster-info). Metalman still
	// sources the CA from the in-cluster service-account mount when cluster-info
	// is unavailable.
	if cfg.APIServerEndpoint != "" {
		env = append(env, corev1.EnvVar{Name: "METALMAN_APISERVER_URL", Value: cfg.APIServerEndpoint})
	}

	// metalman is hostNetwork and binds host ports (DHCP/TFTP/HTTP), so a surge
	// pod cannot start while the old pod holds them on the same node. Terminate
	// the old pod before creating the new one to avoid a rollout deadlock.
	maxSurge := intstr.FromInt32(0)
	maxUnavailable := intstr.FromInt32(1)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{component.SiteOwnerReference(site)},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       &maxSurge,
					MaxUnavailable: &maxUnavailable,
				},
			},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					ServiceAccountName: "metalman-controller",
					// Match either the canonical or deprecated site label during
					// the node-label deprecation window.
					Affinity: component.SiteNodeAffinity(site.Name),
					Containers: []corev1.Container{{
						Name:            "metalman",
						Image:           image,
						ImagePullPolicy: corev1.PullAlways,
						Args:            args,
						Env:             env,
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: 8880, Protocol: corev1.ProtocolTCP},
							{Name: "health", ContainerPort: 8081, Protocol: corev1.ProtocolTCP},
							{Name: "dhcp", ContainerPort: 67, Protocol: corev1.ProtocolUDP},
							{Name: "tftp", ContainerPort: 69, Protocol: corev1.ProtocolUDP},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "tmp", MountPath: "/tmp"},
							{Name: "cache", MountPath: "/var/cache/metalman"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}
