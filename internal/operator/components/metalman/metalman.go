// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package metalman implements the per-Site metalman PXE controller component.
package metalman

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/builder"

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
func (c Component) Plan(_ context.Context, env *component.Env, site *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	support, err := env.DecodeManifestFS(machinamanifests.Manifests, mutateSupportObject)
	if err != nil {
		return nil, component.Result{}, err
	}

	plan := component.NewPlan()

	for _, obj := range support {
		plan.Add(component.Operation{
			Kind:      component.OpApply,
			Object:    obj,
			Component: c.Name(),
			Site:      site.Name,
			SharedKey: sharedSupportKey(obj),
		})
	}

	plan.Add(component.Operation{
		Kind:        component.OpApply,
		Object:      component.ToUnstructured(deployment(site, env.Namespace, env.Config)),
		Component:   c.Name(),
		Site:        site.Name,
		Overridable: true,
	})

	return plan, component.Reconciled(), nil
}

// CleanupPlan removes the per-site metalman Deployment. The shared metalman
// RBAC is left in place; it is harmless when unreferenced and may still be used
// by other sites.
func (c Component) CleanupPlan(_ context.Context, env *component.Env, site *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	plan := component.NewPlan()
	plan.Add(component.DeleteOperation(&appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: DeploymentName(site.Name), Namespace: env.Namespace},
	}, c.Name(), site.Name))

	return plan, component.Disabled("component disabled"), nil
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

// IsSupportObject reports whether obj is the metalman RBAC that ships in the
// machina manifests. The machina component skips these; the metalman component
// applies them.
func IsSupportObject(obj *unstructured.Unstructured) bool {
	return component.IsRBACObject(obj, SupportObjectNameSubstring)
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
					// the node-label deprecation window. Storage scopes its
					// DaemonSet the same way.
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
