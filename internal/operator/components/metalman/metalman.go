// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package metalman implements the per-Site Metalman control and serving plane.
package metalman

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

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
		serverPodDisruptionBudget(site, env.Namespace),
	} {
		if err := env.ApplyObject(ctx, obj); err != nil {
			return component.Failed(err)
		}
	}
	if err := reconcileEndpointEdges(ctx, env, site); err != nil {
		return component.Failed(err)
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
		&policyv1.PodDisruptionBudget{TypeMeta: metav1.TypeMeta{APIVersion: "policy/v1", Kind: "PodDisruptionBudget"}, ObjectMeta: metav1.ObjectMeta{Name: ServerName(site.Name), Namespace: env.Namespace}},
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
	b.Watches(&unboundedv1alpha3.NetbootEndpoint{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		endpoint, ok := obj.(*unboundedv1alpha3.NetbootEndpoint)
		if !ok || endpoint.Spec.SiteRef == "" {
			return nil
		}

		return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: endpoint.Spec.SiteRef}}}
	}))
	b.Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		secret, ok := obj.(*corev1.Secret)
		if !ok {
			return nil
		}

		return requestsForTLSSecret(ctx, env.Client, secret)
	}))
	b.Owns(&appsv1.Deployment{}, builder.WithPredicates(env.OwnedWorkloadPredicate()))
	b.Owns(&corev1.Service{}, builder.WithPredicates(env.OwnedWorkloadPredicate()))
	b.Owns(&corev1.Secret{}, builder.WithPredicates(env.OwnedWorkloadPredicate()))
	b.Owns(&policyv1.PodDisruptionBudget{}, builder.WithPredicates(env.OwnedWorkloadPredicate()))
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
	metalmanControllerRole    = "controller"
	metalmanServerRole        = "server"
	metalmanEdgeRole          = "edge"
	netbootEndpointLabel      = "unbounded-cloud.io/netboot-endpoint"
	edgeTLSChecksumAnnotation = "unbounded-cloud.io/tls-checksum"
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
	probe := func(path string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: path,
			Port: intstr.FromInt32(8081),
		}}}
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
						LivenessProbe:   probe("/healthz"),
						ReadinessProbe:  probe("/readyz"),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
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
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
						MaxSkew:           1,
						TopologyKey:       corev1.LabelHostname,
						WhenUnsatisfiable: corev1.ScheduleAnyway,
						LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
					}},
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
	labels := serverLabels(site.Name)

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

func serverPodDisruptionBudget(site *unboundedv1alpha3.Site, namespace string) *policyv1.PodDisruptionBudget {
	minAvailable := intstr.FromInt32(1)

	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: "policy/v1", Kind: "PodDisruptionBudget"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            ServerName(site.Name),
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{component.SiteOwnerReference(site)},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: serverLabels(site.Name)},
		},
	}
}

func serverLabels(site string) map[string]string {
	return map[string]string{
		"app":                                 "unbounded-metalman",
		"app.kubernetes.io/name":              "metalman-server",
		"app.kubernetes.io/component":         metalmanServerRole,
		unboundedv1alpha3.MachineSiteLabelKey: site,
	}
}

func reconcileEndpointEdges(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) error {
	var endpoints unboundedv1alpha3.NetbootEndpointList
	if err := env.Client.List(ctx, &endpoints); err != nil {
		return fmt.Errorf("list NetbootEndpoints: %w", err)
	}
	sort.Slice(endpoints.Items, func(i, j int) bool { return endpoints.Items[i].Name < endpoints.Items[j].Name })

	desiredDeployments := map[string]struct{}{}
	desiredServices := map[string]struct{}{}
	desiredTLSSecrets := map[string]struct{}{}
	for i := range endpoints.Items {
		endpoint := &endpoints.Items[i]
		if endpoint.Spec.SiteRef != site.Name {
			continue
		}
		deployment, service, err := endpointEdgeObjects(endpoint, site, env.Namespace, env.Config)
		if err != nil {
			return fmt.Errorf("build edge for NetbootEndpoint %s: %w", endpoint.Name, err)
		}
		if deployment != nil {
			if endpoint.Spec.TLS.Mode == unboundedv1alpha3.NetbootEndpointTLSSecret {
				secret, err := mirroredTLSSecret(ctx, env.Client, endpoint, site, env.Namespace)
				if err != nil {
					return fmt.Errorf("mirror TLS Secret for NetbootEndpoint %s: %w", endpoint.Name, err)
				}
				if err := env.ApplyObject(ctx, secret); err != nil {
					return err
				}
				desiredTLSSecrets[secret.Name] = struct{}{}
				deployment.Spec.Template.Annotations = map[string]string{
					edgeTLSChecksumAnnotation: tlsSecretChecksum(secret),
				}
			}
			if err := env.ApplyObject(ctx, deployment); err != nil {
				return err
			}
			desiredDeployments[deployment.Name] = struct{}{}
		}
		if service != nil {
			if err := env.ApplyObject(ctx, service); err != nil {
				return err
			}
			desiredServices[service.Name] = struct{}{}
		}
	}
	match := client.MatchingLabels{
		"app.kubernetes.io/component":         metalmanEdgeRole,
		unboundedv1alpha3.MachineSiteLabelKey: site.Name,
	}
	var deployments appsv1.DeploymentList
	if err := env.Client.List(ctx, &deployments, client.InNamespace(env.Namespace), match); err != nil {
		return fmt.Errorf("list managed edge Deployments: %w", err)
	}
	for i := range deployments.Items {
		if _, ok := desiredDeployments[deployments.Items[i].Name]; !ok {
			if err := env.DeleteIfExists(ctx, &deployments.Items[i]); err != nil {
				return err
			}
		}
	}
	var services corev1.ServiceList
	if err := env.Client.List(ctx, &services, client.InNamespace(env.Namespace), match); err != nil {
		return fmt.Errorf("list managed edge Services: %w", err)
	}
	for i := range services.Items {
		if _, ok := desiredServices[services.Items[i].Name]; !ok {
			if err := env.DeleteIfExists(ctx, &services.Items[i]); err != nil {
				return err
			}
		}
	}
	var tlsSecrets corev1.SecretList
	if err := env.Client.List(ctx, &tlsSecrets, client.InNamespace(env.Namespace), match); err != nil {
		return fmt.Errorf("list managed edge TLS Secrets: %w", err)
	}
	for i := range tlsSecrets.Items {
		if _, ok := desiredTLSSecrets[tlsSecrets.Items[i].Name]; !ok {
			if err := env.DeleteIfExists(ctx, &tlsSecrets.Items[i]); err != nil {
				return err
			}
		}
	}

	return nil
}

func tlsSecretChecksum(secret *corev1.Secret) string {
	digest := sha256.New()
	_, _ = digest.Write(secret.Data[corev1.TLSCertKey])
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(secret.Data[corev1.TLSPrivateKeyKey])

	return fmt.Sprintf("%x", digest.Sum(nil))
}

func mirroredTLSSecret(
	ctx context.Context,
	kubeClient client.Client,
	endpoint *unboundedv1alpha3.NetbootEndpoint,
	site *unboundedv1alpha3.Site,
	namespace string,
) (*corev1.Secret, error) {
	if endpoint.Spec.TLS.SecretRef == nil {
		return nil, fmt.Errorf("TLS secretRef is required")
	}
	ref := endpoint.Spec.TLS.SecretRef
	source := &corev1.Secret{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, source); err != nil {
		return nil, fmt.Errorf("get source Secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	certificate, certOK := source.Data[corev1.TLSCertKey]
	privateKey, keyOK := source.Data[corev1.TLSPrivateKeyKey]
	if !certOK || !keyOK {
		return nil, fmt.Errorf("source Secret %s/%s must contain %s and %s", ref.Namespace, ref.Name, corev1.TLSCertKey, corev1.TLSPrivateKeyKey)
	}

	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            EdgeTLSSecretName(endpoint.Name),
			Namespace:       namespace,
			Labels:          endpointEdgeLabels(endpoint, site.Name),
			OwnerReferences: []metav1.OwnerReference{component.SiteOwnerReference(site)},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       append([]byte(nil), certificate...),
			corev1.TLSPrivateKeyKey: append([]byte(nil), privateKey...),
		},
	}, nil
}

func requestsForTLSSecret(ctx context.Context, kubeClient client.Client, secret *corev1.Secret) []ctrl.Request {
	var endpoints unboundedv1alpha3.NetbootEndpointList
	if err := kubeClient.List(ctx, &endpoints); err != nil {
		return nil
	}

	sites := map[string]struct{}{}
	for i := range endpoints.Items {
		ref := endpoints.Items[i].Spec.TLS.SecretRef
		if endpoints.Items[i].Spec.TLS.Mode == unboundedv1alpha3.NetbootEndpointTLSSecret && ref != nil &&
			ref.Namespace == secret.Namespace && ref.Name == secret.Name {
			sites[endpoints.Items[i].Spec.SiteRef] = struct{}{}
		}
	}
	names := make([]string, 0, len(sites))
	for site := range sites {
		if site != "" {
			names = append(names, site)
		}
	}
	sort.Strings(names)
	requests := make([]ctrl.Request, 0, len(names))
	for _, site := range names {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKey{Name: site}})
	}

	return requests
}

func endpointEdgeObjects(
	endpoint *unboundedv1alpha3.NetbootEndpoint,
	site *unboundedv1alpha3.Site,
	namespace string,
	cfg component.Config,
) (*appsv1.Deployment, *corev1.Service, error) {
	if endpoint.Spec.SiteRef != site.Name {
		return nil, nil, fmt.Errorf("endpoint site %q does not match %q", endpoint.Spec.SiteRef, site.Name)
	}
	if endpoint.Spec.Type == unboundedv1alpha3.NetbootEndpointTypeExternalL2 {
		return nil, nil, nil
	}

	labels := endpointEdgeLabels(endpoint, site.Name)
	replicas := int32(2)
	name := EdgeName(endpoint.Name)
	backendURL := fmt.Sprintf("http://%s.%s.svc:8880", ServerName(site.Name), namespace)
	args := []string{
		metalmanEdgeRole,
		"--backend-url=" + backendURL,
		"--endpoint=" + endpoint.Name,
	}
	ports := []corev1.ContainerPort{{Name: "http", ContainerPort: 8880, Protocol: corev1.ProtocolTCP}}
	podSpec := corev1.PodSpec{ServiceAccountName: "metalman-edge"}
	maxSurge := intstr.FromInt32(1)
	maxUnavailable := intstr.FromInt32(0)
	strategy := appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       &maxSurge,
			MaxUnavailable: &maxUnavailable,
		},
	}

	switch endpoint.Spec.Type {
	case unboundedv1alpha3.NetbootEndpointTypeManagedL2:
		if endpoint.Spec.ManagedL2 == nil {
			return nil, nil, fmt.Errorf("managedL2 configuration is required")
		}
		replicas = 1
		args = append(args,
			"--bind-address="+endpoint.Spec.ManagedL2.Address,
			"--dhcp-enabled",
			"--dhcp-interface="+endpoint.Spec.ManagedL2.Interface,
			"--dhcp-server-ip="+endpoint.Spec.ManagedL2.Address,
			"--tftp-enabled",
			"--tftp-bind-address="+endpoint.Spec.ManagedL2.Address,
		)
		ports = append(ports,
			corev1.ContainerPort{Name: "dhcp", ContainerPort: 67, Protocol: corev1.ProtocolUDP},
			corev1.ContainerPort{Name: "tftp", ContainerPort: 69, Protocol: corev1.ProtocolUDP},
		)
		affinity, err := requiredNodeAffinity(endpoint.Spec.ManagedL2.NodeSelector)
		if err != nil {
			return nil, nil, err
		}
		podSpec.HostNetwork = true
		podSpec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
		podSpec.Affinity = affinity
		strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	case unboundedv1alpha3.NetbootEndpointTypeHTTP:
		if endpoint.Spec.HTTP == nil {
			return nil, nil, fmt.Errorf("http configuration is required")
		}
	default:
		return nil, nil, fmt.Errorf("unsupported endpoint type %q", endpoint.Spec.Type)
	}

	expirationSeconds := int64(3600)
	container := corev1.Container{
		Name:            "metalman",
		Image:           cfg.Image("metalman"),
		ImagePullPolicy: corev1.PullAlways,
		Args:            args,
		Ports:           ports,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
		},
	}
	if endpoint.Spec.Type == unboundedv1alpha3.NetbootEndpointTypeManagedL2 {
		container.VolumeMounts = []corev1.VolumeMount{{Name: "edge-token", MountPath: "/var/run/secrets/metalman", ReadOnly: true}}
		podSpec.Volumes = []corev1.Volume{{Name: "edge-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience:          "metalman-edge",
				ExpirationSeconds: &expirationSeconds,
				Path:              "token",
			}}},
		}}}}
	}
	if endpoint.Spec.TLS.Mode == unboundedv1alpha3.NetbootEndpointTLSSecret {
		args = append(args,
			"--tls-cert-file=/var/run/secrets/metalman-tls/tls.crt",
			"--tls-key-file=/var/run/secrets/metalman-tls/tls.key",
		)
		container.Args = args
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: "tls", MountPath: "/var/run/secrets/metalman-tls", ReadOnly: true,
		})
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: EdgeTLSSecretName(endpoint.Name)}},
		})
	}
	podSpec.Containers = []corev1.Container{container}

	deployment := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{component.SiteOwnerReference(site)},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: strategy,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: podSpec},
		},
	}

	if endpoint.Spec.Type != unboundedv1alpha3.NetbootEndpointTypeHTTP {
		return deployment, nil, nil
	}
	serviceType := endpoint.Spec.HTTP.ServiceType
	if serviceType == "" {
		serviceType = corev1.ServiceTypeClusterIP
	}
	servicePort := corev1.ServicePort{Name: "http", Port: 8880, TargetPort: intstr.FromInt32(8880), Protocol: corev1.ProtocolTCP}
	if endpoint.Spec.TLS.Mode == unboundedv1alpha3.NetbootEndpointTLSSecret {
		servicePort.Name = "https"
		servicePort.Port = 443
	}
	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{component.SiteOwnerReference(site)},
		},
		Spec: corev1.ServiceSpec{
			Type:     serviceType,
			Selector: labels,
			Ports:    []corev1.ServicePort{servicePort},
		},
	}

	return deployment, service, nil
}

// EdgeName returns the in-cluster workload name for an endpoint.
func EdgeName(endpoint string) string { return "metalman-edge-" + endpoint }

// EdgeTLSSecretName returns the mirrored serving-certificate Secret name.
func EdgeTLSSecretName(endpoint string) string { return EdgeName(endpoint) + "-tls" }

func endpointEdgeLabels(endpoint *unboundedv1alpha3.NetbootEndpoint, site string) map[string]string {
	return map[string]string{
		"app":                                 "unbounded-metalman",
		"app.kubernetes.io/name":              "metalman-edge",
		"app.kubernetes.io/component":         metalmanEdgeRole,
		unboundedv1alpha3.MachineSiteLabelKey: site,
		netbootEndpointLabel:                  endpoint.Name,
	}
}

func requiredNodeAffinity(selector metav1.LabelSelector) (*corev1.Affinity, error) {
	requirements := make([]corev1.NodeSelectorRequirement, 0, len(selector.MatchLabels)+len(selector.MatchExpressions))
	keys := make([]string, 0, len(selector.MatchLabels))
	for key := range selector.MatchLabels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		requirements = append(requirements, corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{selector.MatchLabels[key]}})
	}
	for _, expression := range selector.MatchExpressions {
		operator := corev1.NodeSelectorOperator(expression.Operator)
		switch operator {
		case corev1.NodeSelectorOpIn, corev1.NodeSelectorOpNotIn, corev1.NodeSelectorOpExists, corev1.NodeSelectorOpDoesNotExist:
		default:
			return nil, fmt.Errorf("unsupported node selector operator %q", expression.Operator)
		}
		requirements = append(requirements, corev1.NodeSelectorRequirement{Key: expression.Key, Operator: operator, Values: expression.Values})
	}

	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: requirements}},
	}}}, nil
}
