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
)

const (
	FieldOwner = "unbounded-operator"

	ComponentNet              = "net"
	ComponentMachina          = "machina"
	ComponentMetalman         = "metalman"
	ComponentUnboundedStorage = "unboundedStorage"

	DefaultNamespace    = "unbounded-kube"
	DefaultNetNamespace = "unbounded-net"
)

type Config struct {
	DefaultNamespace string
	NetNamespace     string

	NetControllerImage string
	NetNodeImage       string
	MachinaImage       string
	MetalmanImage      string
	StorageImage       string

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
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	statuses := map[string]unboundedv1alpha3.SiteComponentStatus{}
	for _, component := range []string{ComponentNet, ComponentMachina, ComponentMetalman, ComponentUnboundedStorage} {
		status := r.reconcileComponent(ctx, &site, component)
		statuses[component] = status
		if !status.Ready {
			logger.Info("component not ready", "site", site.Name, "component", component, "message", status.Message)
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

func (r *SiteReconciler) reconcileComponent(ctx context.Context, site *unboundedv1alpha3.Site, component string) unboundedv1alpha3.SiteComponentStatus {
	if !componentEnabled(site, component) {
		return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "disabled"}
	}

	if err := r.reconcileEnabledComponent(ctx, site, component); err != nil {
		return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
	}

	return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "reconciled"}
}

func (r *SiteReconciler) reconcileEnabledComponent(ctx context.Context, site *unboundedv1alpha3.Site, component string) error {
	switch component {
	case ComponentNet:
		return r.applyManifestFS(ctx, netmanifests.Manifests, func(obj *unstructured.Unstructured) error {
			return r.mutateNetObject(site, obj)
		})
	case ComponentMachina:
		return r.applyManifestFS(ctx, machinamanifests.Manifests, func(obj *unstructured.Unstructured) error {
			return r.mutateMachinaObject(site, obj)
		})
	case ComponentMetalman:
		defaults := r.defaults()
		namespace := componentNamespace(&site.Spec.Components.Metalman.SiteComponentSpec, defaults.DefaultNamespace)
		if err := r.applyObject(ctx, namespaceObject(namespace)); err != nil {
			return err
		}

		if err := r.applyManifestFS(ctx, machinamanifests.Manifests, func(obj *unstructured.Unstructured) error {
			return r.mutateMetalmanSupportObject(site, obj)
		}); err != nil {
			return err
		}

		return r.applyObject(ctx, metalmanDeployment(site, r.defaults()))
	case ComponentUnboundedStorage:
		return r.applyManifestFS(ctx, storagemanifests.Manifests, func(obj *unstructured.Unstructured) error {
			return r.mutateStorageObject(site, obj)
		})
	default:
		return fmt.Errorf("unknown component %q", component)
	}
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

func (r *SiteReconciler) mutateNetObject(site *unboundedv1alpha3.Site, obj *unstructured.Unstructured) error {
	defaults := r.defaults()
	namespace := componentNamespace(site.Spec.Components.Net, defaults.NetNamespace)

	setNamespace(obj, namespace)
	rewriteNamespace(obj, DefaultNetNamespace, namespace)
	switch obj.GetKind() {
	case "Deployment":
		if obj.GetName() == "unbounded-net-controller" {
			setContainerImage(obj, "controller", firstNonEmpty(site.Spec.Components.Net.Image, defaults.NetControllerImage))
		}
	case "DaemonSet":
		if obj.GetName() == "unbounded-net-node" {
			setContainerImage(obj, "node", firstNonEmpty(site.Spec.Components.Net.Image, defaults.NetNodeImage))
			setInitContainerImage(obj, "install-cni-plugins", firstNonEmpty(site.Spec.Components.Net.Image, defaults.NetNodeImage))
		}
	}

	return nil
}

func (r *SiteReconciler) mutateMachinaObject(site *unboundedv1alpha3.Site, obj *unstructured.Unstructured) error {
	if isMetalmanSupportObject(obj) {
		obj.Object = nil
		return nil
	}

	defaults := r.defaults()
	namespace := componentNamespace(site.Spec.Components.Machina, defaults.DefaultNamespace)

	setNamespace(obj, namespace)
	rewriteNamespace(obj, DefaultNamespace, namespace)
	if obj.GetKind() == "Deployment" && obj.GetName() == "machina-controller" {
		setContainerImage(obj, "machina-controller", firstNonEmpty(site.Spec.Components.Machina.Image, defaults.MachinaImage))
	}

	if obj.GetKind() == "ConfigMap" && obj.GetName() == "machina-config" && defaults.APIServerEndpoint != "" {
		data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
		data["config.yaml"] = strings.ReplaceAll(data["config.yaml"], `apiServerEndpoint: ""`, fmt.Sprintf(`apiServerEndpoint: %q`, defaults.APIServerEndpoint))
		obj.Object["data"] = data
	}

	return nil
}

func (r *SiteReconciler) mutateMetalmanSupportObject(site *unboundedv1alpha3.Site, obj *unstructured.Unstructured) error {
	if filepath.Base(obj.GetName()) == ".gitignore" {
		return nil
	}

	if !isMetalmanSupportObject(obj) {
		obj.Object = nil
		return nil
	}

	defaults := r.defaults()
	namespace := componentNamespace(&site.Spec.Components.Metalman.SiteComponentSpec, defaults.DefaultNamespace)
	if obj.GetNamespace() == DefaultNamespace {
		setNamespace(obj, namespace)
	}
	rewriteNamespace(obj, DefaultNamespace, namespace)

	return nil
}

func (r *SiteReconciler) mutateStorageObject(site *unboundedv1alpha3.Site, obj *unstructured.Unstructured) error {
	defaults := r.defaults()
	namespace := componentNamespace(&site.Spec.Components.UnboundedStorage.SiteComponentSpec, defaults.DefaultNamespace)

	setNamespace(obj, namespace)
	rewriteNamespace(obj, DefaultNamespace, namespace)
	if obj.GetKind() == "DaemonSet" && obj.GetName() == "unbounded-storage-supervisor" {
		image := firstNonEmpty(site.Spec.Components.UnboundedStorage.Image, defaults.StorageImage)
		setContainerImage(obj, "run", image)
		setInitContainerImage(obj, "install", image)
	}

	if obj.GetKind() == "ConfigMap" && obj.GetName() == "unbounded-storage-config" && site.Spec.Components.UnboundedStorage.Config != "" {
		obj.Object["data"] = map[string]any{"config.yaml": site.Spec.Components.UnboundedStorage.Config}
	}

	return nil
}

func componentEnabled(site *unboundedv1alpha3.Site, component string) bool {
	switch component {
	case ComponentNet:
		return unboundedv1alpha3.ComponentEnabled(site.Spec.Components.Net)
	case ComponentMachina:
		return unboundedv1alpha3.ComponentEnabled(site.Spec.Components.Machina)
	case ComponentMetalman:
		if site.Spec.Components.Metalman == nil {
			return false
		}

		return unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.Metalman.SiteComponentSpec)
	case ComponentUnboundedStorage:
		if site.Spec.Components.UnboundedStorage == nil {
			return false
		}

		return unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.UnboundedStorage.SiteComponentSpec)
	default:
		return false
	}
}

func (r *SiteReconciler) defaults() Config {
	cfg := r.Config
	if cfg.DefaultNamespace == "" {
		cfg.DefaultNamespace = DefaultNamespace
	}
	if cfg.NetNamespace == "" {
		cfg.NetNamespace = DefaultNetNamespace
	}

	return cfg
}

func componentNamespace(component *unboundedv1alpha3.SiteComponentSpec, defaultNamespace string) string {
	if component != nil && component.Namespace != "" {
		return component.Namespace
	}

	return defaultNamespace
}

func isMetalmanSupportObject(obj *unstructured.Unstructured) bool {
	switch obj.GetKind() {
	case "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding":
		return strings.Contains(obj.GetName(), "metalman")
	default:
		return false
	}
}

func metalmanDeployment(site *unboundedv1alpha3.Site, defaults Config) *appsv1.Deployment {
	namespace := componentNamespace(&site.Spec.Components.Metalman.SiteComponentSpec, defaults.DefaultNamespace)
	image := firstNonEmpty(site.Spec.Components.Metalman.Image, defaults.MetalmanImage)
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

func namespaceObject(name string) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
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

func setNamespace(obj *unstructured.Unstructured, namespace string) {
	if namespace == "" {
		return
	}
	if obj.GetKind() == "Namespace" {
		obj.SetName(namespace)
		return
	}
	if obj.GetNamespace() == "" {
		return
	}

	obj.SetNamespace(namespace)
}

func rewriteNamespace(obj *unstructured.Unstructured, oldNamespace, newNamespace string) {
	if oldNamespace == "" || newNamespace == "" || oldNamespace == newNamespace {
		return
	}

	rewriteStringValue(obj.Object, oldNamespace, newNamespace)
}

func rewriteStringValue(value any, oldValue, newValue string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok && text == oldValue {
				typed[key] = newValue
				continue
			}

			rewriteStringValue(child, oldValue, newValue)
		}
	case []any:
		for i, child := range typed {
			if text, ok := child.(string); ok && text == oldValue {
				typed[i] = newValue
				continue
			}

			rewriteStringValue(child, oldValue, newValue)
		}
	}
}

func setContainerImage(obj *unstructured.Unstructured, name, image string) {
	if image == "" {
		return
	}

	setPodSpecContainerImage(obj, []string{"spec", "template", "spec", "containers"}, name, image)
}

func setInitContainerImage(obj *unstructured.Unstructured, name, image string) {
	if image == "" {
		return
	}

	setPodSpecContainerImage(obj, []string{"spec", "template", "spec", "initContainers"}, name, image)
}

func setPodSpecContainerImage(obj *unstructured.Unstructured, path []string, name, image string) {
	containers, ok, _ := unstructured.NestedSlice(obj.Object, path...)
	if !ok {
		return
	}

	for i := range containers {
		container, ok := containers[i].(map[string]any)
		if !ok {
			continue
		}
		if container["name"] == name {
			container["image"] = image
			containers[i] = container
			break
		}
	}

	_ = unstructured.SetNestedSlice(obj.Object, containers, path...)
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

func ptrInt32(value int32) *int32 {
	return &value
}
