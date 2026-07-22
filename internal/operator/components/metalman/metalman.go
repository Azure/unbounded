// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package metalman implements the per-Site Metalman control and serving plane.
package metalman

import (
	"context"
	"crypto/rand"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	"github.com/Azure/unbounded/internal/operator/component"
)

// Component reconciles the per-Site Metalman workloads.
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

// Reconcile deploys the per-site Metalman controller and server plane.
func (Component) Reconcile(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) component.Result {
	if err := env.ApplyManifestFS(ctx, machinamanifests.Manifests, mutateSupportObject); err != nil {
		return component.Failed(err)
	}
	if err := ensureCapabilitySecret(ctx, env, site); err != nil {
		return component.Failed(err)
	}

	for _, obj := range []client.Object{
		controllerDeployment(site, env.Namespace, env.Config),
		serverDeployment(site, env.Namespace, env.Config),
		serverService(site, env.Namespace),
	} {
		if err := env.ApplyObject(ctx, obj); err != nil {
			return component.Failed(err)
		}
	}

	return component.Reconciled()
}

// Cleanup removes the per-site Metalman workloads. The shared Metalman RBAC is
// left in place; it is harmless when unreferenced and may still be used by other
// sites.
func (Component) Cleanup(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) error {
	for _, obj := range []client.Object{
		&appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: DeploymentName(site.Name), Namespace: env.Namespace}},
		&appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: ServerName(site.Name), Namespace: env.Namespace}},
		&corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metav1.ObjectMeta{Name: ServerName(site.Name), Namespace: env.Namespace}},
		&corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: CapabilitySecretName(site.Name), Namespace: env.Namespace}},
	} {
		if err := env.DeleteIfExists(ctx, obj); err != nil {
			return err
		}
	}

	return nil
}

// SetupWatches recreates the per-site Deployment if it is deleted or drifts, via
// its controller owner reference to the Site. The predicate drops status-only
// updates so pod-count churn does not re-apply the Deployment.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Owns(&appsv1.Deployment{}, builder.WithPredicates(env.OwnedWorkloadPredicate()))
	b.Owns(&corev1.Service{}, builder.WithPredicates(env.OwnedWorkloadPredicate()))
}

// DeploymentName is the per-site metalman Deployment name.
func DeploymentName(site string) string { return "metalman-controller-" + site }

// ServerName is the per-site Metalman server Deployment and Service name.
func ServerName(site string) string { return "metalman-server-" + site }

// CapabilitySecretName is the per-site capability signing Secret name.
func CapabilitySecretName(site string) string { return "metalman-capability-" + site }

const capabilitySecretKey = "capability.key"

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

func controllerDeployment(site *unboundedv1alpha3.Site, namespace string, cfg component.Config) *appsv1.Deployment {
	return roleDeployment(site, namespace, cfg, metalmanControllerRole, 1)
}

func serverDeployment(site *unboundedv1alpha3.Site, namespace string, cfg component.Config) *appsv1.Deployment {
	return roleDeployment(site, namespace, cfg, metalmanServerRole, 2)
}

const (
	metalmanControllerRole = "controller"
	metalmanServerRole     = "server"
)

func roleDeployment(site *unboundedv1alpha3.Site, namespace string, cfg component.Config, role string, replicas int32) *appsv1.Deployment {
	image := cfg.Image("metalman")
	name := DeploymentName(site.Name)
	if role == metalmanServerRole {
		name = ServerName(site.Name)
	}

	labels := map[string]string{
		"app":                                 "unbounded-metalman",
		"app.kubernetes.io/name":              "metalman-" + role,
		"app.kubernetes.io/component":         role,
		unboundedv1alpha3.MachineSiteLabelKey: site.Name,
	}

	args := []string{role, "--site=" + site.Name, "--cache-dir=/var/cache/metalman"}

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

	maxSurge := intstr.FromInt32(1)
	maxUnavailable := intstr.FromInt32(0)
	ports := []corev1.ContainerPort{{Name: "health", ContainerPort: 8081, Protocol: corev1.ProtocolTCP}}
	serviceAccountName := "metalman-controller"
	if role == metalmanServerRole {
		serviceAccountName = "metalman-server"
		ports = append(ports, corev1.ContainerPort{Name: "http", ContainerPort: 8880, Protocol: corev1.ProtocolTCP})
	}

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
					ServiceAccountName: serviceAccountName,
					Containers: []corev1.Container{{
						Name:            "metalman",
						Image:           image,
						ImagePullPolicy: corev1.PullAlways,
						Args:            args,
						Env:             env,
						Ports:           ports,
						VolumeMounts: []corev1.VolumeMount{
							{Name: "tmp", MountPath: "/tmp"},
							{Name: "cache", MountPath: "/var/cache/metalman"},
							{Name: "capability-key", MountPath: "/var/run/secrets/metalman", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "capability-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: CapabilitySecretName(site.Name),
						}}},
					},
				},
			},
		},
	}
}

func ensureCapabilitySecret(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) error {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: env.Namespace, Name: CapabilitySecretName(site.Name)}
	if err := env.Client.Get(ctx, key, secret); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get capability Secret: %w", err)
	}

	capabilityKey := make([]byte, 32)
	if _, err := rand.Read(capabilityKey); err != nil {
		return fmt.Errorf("generate capability key: %w", err)
	}

	secret = &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            key.Name,
			Namespace:       key.Namespace,
			OwnerReferences: []metav1.OwnerReference{component.SiteOwnerReference(site)},
		},
		Data: map[string][]byte{capabilitySecretKey: capabilityKey},
	}
	if err := env.Client.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create capability Secret: %w", err)
	}

	return nil
}

func serverService(site *unboundedv1alpha3.Site, namespace string) *corev1.Service {
	labels := map[string]string{
		"app":                                 "unbounded-metalman",
		"app.kubernetes.io/name":              "metalman-server",
		"app.kubernetes.io/component":         metalmanServerRole,
		unboundedv1alpha3.MachineSiteLabelKey: site.Name,
	}

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            ServerName(site.Name),
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{component.SiteOwnerReference(site)},
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       8880,
				TargetPort: intstr.FromInt32(8880),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}
