// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	netmanifests "github.com/Azure/unbounded/deploy/net"
	storagemanifests "github.com/Azure/unbounded/deploy/unbounded-storage-supervisor"
	"github.com/Azure/unbounded/internal/unbounded"
)

const (
	FieldOwner = "unbounded-operator"

	ComponentNet      = "net"
	ComponentMachina  = "machina"
	ComponentMetalman = "metalman"
	ComponentStorage  = "storage"

	// siteLabelKey is the node label the net controller assigns for site
	// membership; the operator uses it to node-select site-scoped components.
	siteLabelKey = "net.unbounded-cloud.io/site"
)

// DefaultNamespace is the namespace the operator installs components into. It
// follows the operator's own namespace (unbounded.SystemNamespace()).
var DefaultNamespace = unbounded.SystemNamespace()

type Config struct {
	// MetalmanImage is the image for the synthesized per-site metalman
	// Deployment. It is stamped to the operator's version by the operator
	// Deployment; the other components carry their version-matched image in
	// the embedded release manifests.
	MetalmanImage string

	// APIServerEndpoint is injected into the machina controller config.
	APIServerEndpoint string
}

type SiteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config
}

func (r *SiteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var site unboundedv1alpha3.Site
	if err := r.Get(ctx, req.NamespacedName, &site); err != nil {
		if apierrors.IsNotFound(err) {
			// A Site was deleted; reconcile the cluster singletons so net/machina
			// are torn down when the last Site goes away.
			return ctrl.Result{}, r.reconcileSingletons(ctx)
		}

		return ctrl.Result{}, err
	}

	statuses := map[string]unboundedv1alpha3.SiteComponentStatus{}

	// Cluster singletons: net is deployed whenever at least one Site exists;
	// machina whenever any Site enables it. Reconciled idempotently on every
	// Site event and reported on each Site's status for visibility.
	statuses[ComponentNet] = r.reconcileNet(ctx)
	statuses[ComponentMachina] = r.reconcileMachina(ctx)

	// Per-site components.
	statuses[ComponentMetalman] = componentStatus(
		!componentEnabled(&site, ComponentMetalman),
		func() error { return r.reconcileMetalman(ctx, &site) },
	)
	statuses[ComponentStorage] = componentStatus(
		!componentEnabled(&site, ComponentStorage),
		func() error { return r.reconcileStorage(ctx, &site) },
	)

	for name, status := range statuses {
		if !status.Ready {
			logger.Info("component not ready", "site", site.Name, "component", name, "message", status.Message)
		}
	}

	patch := client.MergeFrom(site.DeepCopy())

	site.Status.Components = statuses
	if err := r.Status().Patch(ctx, &site, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch site status: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *SiteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&unboundedv1alpha3.Site{}).
		Complete(r)
}

// componentStatus turns a reconcile outcome into a SiteComponentStatus.
func componentStatus(disabled bool, reconcile func() error) unboundedv1alpha3.SiteComponentStatus {
	if disabled {
		return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "disabled"}
	}

	if err := reconcile(); err != nil {
		return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
	}

	return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "reconciled"}
}

// reconcileSingletons reconciles just the cluster-singleton components (net,
// machina); used when a Site is deleted.
func (r *SiteReconciler) reconcileSingletons(ctx context.Context) error {
	if s := r.reconcileNet(ctx); !s.Ready {
		return fmt.Errorf("net: %s", s.Message)
	}

	if s := r.reconcileMachina(ctx); !s.Ready {
		return fmt.Errorf("machina: %s", s.Message)
	}

	return nil
}

// reconcileNet deploys the unbounded-net cluster singleton whenever at least
// one Site exists. Net is not a per-Site component: a single controller/node
// pair reads the networking spec of every Site.
func (r *SiteReconciler) reconcileNet(ctx context.Context) unboundedv1alpha3.SiteComponentStatus {
	sites, err := r.listSites(ctx)
	if err != nil {
		return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
	}

	if len(sites) == 0 {
		return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "no sites"}
	}

	if err := r.applyManifestFS(ctx, netmanifests.Manifests, nil); err != nil {
		return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
	}

	return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "reconciled"}
}

// reconcileMachina deploys the machina controller singleton whenever any Site
// enables it. (Machina is a singleton for now; it may become site-scoped.)
func (r *SiteReconciler) reconcileMachina(ctx context.Context) unboundedv1alpha3.SiteComponentStatus {
	sites, err := r.listSites(ctx)
	if err != nil {
		return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
	}

	enabled := false

	for i := range sites {
		if componentEnabled(&sites[i], ComponentMachina) {
			enabled = true
			break
		}
	}

	if !enabled {
		return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "disabled"}
	}

	if err := r.applyManifestFS(ctx, machinamanifests.Manifests, r.mutateMachinaObject); err != nil {
		return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
	}

	return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "reconciled"}
}

// reconcileMetalman deploys the per-site metalman PXE controller and its RBAC.
func (r *SiteReconciler) reconcileMetalman(ctx context.Context, site *unboundedv1alpha3.Site) error {
	if err := r.applyManifestFS(ctx, machinamanifests.Manifests, mutateMetalmanSupportObject); err != nil {
		return err
	}

	return r.applyObject(ctx, metalmanDeployment(site, r.Config))
}

// reconcileStorage deploys a site-scoped storage supervisor DaemonSet that runs
// only on the nodes belonging to the Site.
func (r *SiteReconciler) reconcileStorage(ctx context.Context, site *unboundedv1alpha3.Site) error {
	return r.applyManifestFS(ctx, storagemanifests.Manifests, func(obj *unstructured.Unstructured) error {
		return mutateStorageObject(site, obj)
	})
}

func (r *SiteReconciler) listSites(ctx context.Context) ([]unboundedv1alpha3.Site, error) {
	var sites unboundedv1alpha3.SiteList
	if err := r.List(ctx, &sites); err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}

	return sites.Items, nil
}

func (r *SiteReconciler) applyManifestFS(ctx context.Context, manifests fs.FS, mutate func(*unstructured.Unstructured) error) error {
	files, err := yamlFiles(manifests)
	if err != nil {
		return err
	}

	for _, file := range files {
		data, err := fs.ReadFile(manifests, file)
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", file, err)
		}

		if err := r.applyManifestData(ctx, data, mutate); err != nil {
			return fmt.Errorf("apply manifest %s: %w", file, err)
		}
	}

	return nil
}

func (r *SiteReconciler) applyManifestData(ctx context.Context, data []byte, mutate func(*unstructured.Unstructured) error) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return fmt.Errorf("decode resource: %w", err)
		}

		if obj.Object == nil {
			continue
		}

		if mutate != nil {
			if err := mutate(obj); err != nil {
				return err
			}
		}

		if obj.Object == nil {
			continue
		}

		if err := r.applyObject(ctx, obj); err != nil {
			return err
		}
	}

	return nil
}

func (r *SiteReconciler) applyObject(ctx context.Context, obj client.Object) error {
	applyCfg := client.ApplyConfigurationFromUnstructured(toUnstructured(obj))
	if err := r.Apply(ctx, applyCfg, client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s %s/%s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName(), err)
	}

	return nil
}

func (r *SiteReconciler) mutateMachinaObject(obj *unstructured.Unstructured) error {
	// Metalman RBAC ships in the machina manifests but is applied per-site by
	// reconcileMetalman; skip it here so the singleton apply does not create it.
	if isMetalmanSupportObject(obj) {
		obj.Object = nil
		return nil
	}

	if obj.GetKind() == "ConfigMap" && obj.GetName() == "machina-config" && r.Config.APIServerEndpoint != "" {
		current, _, err := unstructured.NestedString(obj.Object, "data", "config.yaml")
		if err != nil {
			return fmt.Errorf("get machina config data: %w", err)
		}

		updated := strings.ReplaceAll(current, `apiServerEndpoint: ""`, fmt.Sprintf(`apiServerEndpoint: %q`, r.Config.APIServerEndpoint))
		if err := unstructured.SetNestedField(obj.Object, updated, "data", "config.yaml"); err != nil {
			return fmt.Errorf("set machina config data: %w", err)
		}
	}

	return nil
}

// mutateMetalmanSupportObject keeps only the metalman RBAC objects from the
// machina manifests (they are already rendered into unbounded-system).
func mutateMetalmanSupportObject(obj *unstructured.Unstructured) error {
	if filepath.Base(obj.GetName()) == ".gitignore" {
		return nil
	}

	if !isMetalmanSupportObject(obj) {
		obj.Object = nil
	}

	return nil
}

// mutateStorageObject scopes the storage supervisor DaemonSet to the Site's
// nodes (per-site name, labels, and nodeSelector). The DaemonSet's SA, RBAC,
// and ConfigMap are shared across sites.
func mutateStorageObject(site *unboundedv1alpha3.Site, obj *unstructured.Unstructured) error {
	switch {
	case obj.GetKind() == "DaemonSet" && obj.GetName() == "unbounded-storage-supervisor":
		return scopeStorageDaemonSetToSite(site, obj)
	case obj.GetKind() == "ConfigMap" && obj.GetName() == "unbounded-storage-config":
		if site.Spec.Components.Storage != nil && site.Spec.Components.Storage.Config != "" {
			obj.Object["data"] = map[string]any{"config.yaml": site.Spec.Components.Storage.Config}
		}

		return nil
	default:
		return nil
	}
}

func scopeStorageDaemonSetToSite(site *unboundedv1alpha3.Site, obj *unstructured.Unstructured) error {
	obj.SetName("unbounded-storage-supervisor-" + site.Name)

	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}

	labels[siteLabelKey] = site.Name
	obj.SetLabels(labels)

	for _, path := range [][]string{
		{"spec", "selector", "matchLabels", siteLabelKey},
		{"spec", "template", "metadata", "labels", siteLabelKey},
		{"spec", "template", "spec", "nodeSelector", siteLabelKey},
	} {
		if err := unstructured.SetNestedField(obj.Object, site.Name, path...); err != nil {
			return fmt.Errorf("scope storage daemonset (%v): %w", path, err)
		}
	}

	return nil
}

func componentEnabled(site *unboundedv1alpha3.Site, component string) bool {
	switch component {
	case ComponentMachina:
		return unboundedv1alpha3.ComponentEnabled(site.Spec.Components.Machina)
	case ComponentMetalman:
		if site.Spec.Components.Metalman == nil {
			return false
		}

		return unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.Metalman.SiteComponentSpec)
	case ComponentStorage:
		if site.Spec.Components.Storage == nil {
			return false
		}

		return unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.Storage.SiteComponentSpec)
	default:
		return false
	}
}

func isMetalmanSupportObject(obj *unstructured.Unstructured) bool {
	switch obj.GetKind() {
	case "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding":
		return strings.Contains(obj.GetName(), "metalman")
	default:
		return false
	}
}

func metalmanDeployment(site *unboundedv1alpha3.Site, cfg Config) *appsv1.Deployment {
	namespace := DefaultNamespace
	image := cfg.MetalmanImage
	name := "metalman-controller-" + site.Name
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

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrInt32(1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					ServiceAccountName: "metalman-controller",
					NodeSelector: map[string]string{
						unboundedv1alpha3.MachineSiteLabelKey: site.Name,
					},
					Containers: []corev1.Container{{
						Name:            "metalman",
						Image:           image,
						ImagePullPolicy: corev1.PullAlways,
						Args:            args,
						Env: []corev1.EnvVar{{
							// Metalman resolves its leader-election lease
							// namespace from POD_NAMESPACE so the lease and its
							// namespace-scoped RBAC stay co-located with the
							// Deployment under any install namespace.
							Name: "POD_NAMESPACE",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
							},
						}},
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

func yamlFiles(fsys fs.FS) ([]string, error) {
	var files []string

	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".yaml" || ext == ".yml" {
			files = append(files, path)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	sort.Strings(files)

	return files, nil
}

func toUnstructured(obj client.Object) *unstructured.Unstructured {
	if unstr, ok := obj.(*unstructured.Unstructured); ok {
		return unstr
	}

	objectMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		panic(err)
	}

	return &unstructured.Unstructured{Object: objectMap}
}

func ptrInt32(value int32) *int32 {
	return &value
}
