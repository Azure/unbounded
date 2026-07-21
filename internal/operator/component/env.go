// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/unbounded"
)

const (
	// FieldOwner is the server-side apply field manager the operator uses for
	// every object it applies.
	FieldOwner = "unbounded-operator"

	// CRDKind is the Kind of a CustomResourceDefinition object in the embedded
	// manifests. The operator installs CRDs once at startup, so components skip
	// them during reconcile.
	CRDKind = "CustomResourceDefinition"

	// SiteLabelKey is the canonical node label for site membership
	// (unbounded-cloud.io/site). Per-site components node-select on it.
	SiteLabelKey = unboundedv1alpha3.MachineSiteLabelKey

	// DeprecatedSiteLabelKey is the node site-membership label used by released
	// net controllers before the switch to unbounded-cloud.io/site. Per-site
	// workloads target either key during the deprecation window so they schedule
	// before the upgraded net has converged all Nodes to the canonical label.
	DeprecatedSiteLabelKey = "net.unbounded-cloud.io/site"

	// SingletonRequestName cannot collide with a valid Site name. Managed
	// singleton resource events use it independently of Site fan-out.
	SingletonRequestName = "__singletons"

	// BuildDefaultNamespace is the namespace baked into the embedded component
	// manifests at build time. RetargetNamespace rewrites it to the operator's
	// configured namespace when they differ.
	BuildDefaultNamespace = unbounded.DefaultSystemNamespace
)

// DefaultNamespace is the namespace the operator installs components into. It
// follows the operator's own namespace (unbounded.SystemNamespace()).
var DefaultNamespace = unbounded.SystemNamespace()

// Config carries operator-level settings components read while reconciling.
type Config struct {
	ImageRegistry string
	ImageTag      string

	// APIServerEndpoint is injected into the machina controller config and
	// advertised to metalman.
	APIServerEndpoint string
}

// Image returns the operator-managed image for repository.
func (c Config) Image(repository string) string {
	return strings.TrimRight(c.ImageRegistry, "/") + "/azure/" + repository + ":" + c.ImageTag
}

// SetPodSpecImages replaces every init and main container image in a workload.
func SetPodSpecImages(obj *unstructured.Unstructured, image string) error {
	for _, field := range []string{"initContainers", "containers"} {
		containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
		if err != nil {
			return fmt.Errorf("get %s: %w", field, err)
		}

		if !found {
			continue
		}

		for i := range containers {
			container, ok := containers[i].(map[string]any)
			if !ok {
				return fmt.Errorf("%s[%d] is not an object", field, i)
			}

			container["image"] = image
		}

		if err := unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", field); err != nil {
			return fmt.Errorf("set %s: %w", field, err)
		}
	}

	return nil
}

// Env is the shared execution context handed to every component. It bundles the
// Kubernetes client, scheme, target namespace, and operator Config together with
// the manifest-apply and helper machinery components use to reconcile.
type Env struct {
	Client    client.Client
	Scheme    *runtime.Scheme
	Namespace string
	Config    Config
}

// ApplyManifestFS server-side applies every YAML object in the manifest
// filesystem, running mutate on each decoded object first. A mutate that nils
// out obj.Object skips that object.
func (e *Env) ApplyManifestFS(ctx context.Context, manifests fs.FS, mutate func(*unstructured.Unstructured) error) error {
	files, err := YamlFiles(manifests)
	if err != nil {
		return err
	}

	for _, file := range files {
		data, err := fs.ReadFile(manifests, file)
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", file, err)
		}

		if err := e.applyManifestData(ctx, data, mutate); err != nil {
			return fmt.Errorf("apply manifest %s: %w", file, err)
		}
	}

	return nil
}

func (e *Env) applyManifestData(ctx context.Context, data []byte, mutate func(*unstructured.Unstructured) error) error {
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

		e.RetargetNamespace(obj)

		if err := e.ApplyObject(ctx, obj); err != nil {
			return err
		}
	}

	return nil
}

// ApplyObject server-side applies a single object with the operator field owner.
func (e *Env) ApplyObject(ctx context.Context, obj client.Object) error {
	applyCfg := client.ApplyConfigurationFromUnstructured(ToUnstructured(obj))
	if err := e.Client.Apply(ctx, applyCfg, client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s %s/%s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName(), err)
	}

	return nil
}

// ListSites returns every Site in the cluster.
func (e *Env) ListSites(ctx context.Context) ([]unboundedv1alpha3.Site, error) {
	var sites unboundedv1alpha3.SiteList
	if err := e.Client.List(ctx, &sites); err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}

	return sites.Items, nil
}

// DeleteIfExists deletes an object, treating an already-absent object as success.
func (e *Env) DeleteIfExists(ctx context.Context, obj client.Object) error {
	if err := e.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s/%s: %w",
			obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName(), err)
	}

	return nil
}

// DefaultConfigMap extracts the named ConfigMap from a manifest filesystem and
// retargets it to the operator namespace. component names the manifest set for
// error messages.
func (e *Env) DefaultConfigMap(manifests fs.FS, name, component string) (*corev1.ConfigMap, error) {
	files, err := YamlFiles(manifests)
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		data, err := fs.ReadFile(manifests, file)
		if err != nil {
			return nil, fmt.Errorf("read %s manifest %s: %w", component, file, err)
		}

		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

		for {
			obj := &unstructured.Unstructured{}
			if err := decoder.Decode(obj); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				return nil, fmt.Errorf("decode %s manifest %s: %w", component, file, err)
			}

			if obj.Object == nil || obj.GetKind() != "ConfigMap" || obj.GetName() != name {
				continue
			}

			cm := &corev1.ConfigMap{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, cm); err != nil {
				return nil, fmt.Errorf("convert %s config template: %w", component, err)
			}

			cm.Namespace = e.Namespace

			return cm, nil
		}
	}

	return nil, fmt.Errorf("%s config template not found", component)
}

// RetargetNamespace rewrites embedded-manifest references from the build-time
// default namespace (BuildDefaultNamespace) to the operator's configured
// namespace. It is a no-op in the default install where the two are identical.
func (e *Env) RetargetNamespace(obj *unstructured.Unstructured) {
	ns := e.Namespace
	if ns == "" || ns == BuildDefaultNamespace {
		return
	}

	if obj.GetKind() == "Namespace" {
		if obj.GetName() == BuildDefaultNamespace {
			obj.SetName(ns)
		}

		return
	}

	if obj.GetNamespace() == BuildDefaultNamespace {
		obj.SetNamespace(ns)
	}

	rewriteNamespaceValues(obj.Object, BuildDefaultNamespace, ns)
}

// rewriteNamespaceValues recursively retargets every string value that embeds the
// old namespace to the new one, covering exact matches, service-account
// usernames in CEL, and flag values, while leaving unrelated strings (image
// references) untouched.
func rewriteNamespaceValues(value any, oldNS, newNS string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, v := range typed {
			if str, ok := v.(string); ok {
				typed[key] = retargetNamespaceInString(str, oldNS, newNS)
				continue
			}

			rewriteNamespaceValues(v, oldNS, newNS)
		}
	case []any:
		for i, v := range typed {
			if str, ok := v.(string); ok {
				typed[i] = retargetNamespaceInString(str, oldNS, newNS)
				continue
			}

			rewriteNamespaceValues(v, oldNS, newNS)
		}
	}
}

func retargetNamespaceInString(s, oldNS, newNS string) string {
	if s == oldNS {
		return newNS
	}

	if strings.Contains(s, "system:serviceaccount:"+oldNS+":") {
		s = strings.ReplaceAll(s, "system:serviceaccount:"+oldNS+":", "system:serviceaccount:"+newNS+":")
	}

	if strings.HasPrefix(s, "--") && strings.HasSuffix(s, "="+oldNS) {
		s = strings.TrimSuffix(s, "="+oldNS) + "=" + newNS
	}

	return s
}

// YamlFiles returns the sorted list of YAML files in a manifest filesystem.
func YamlFiles(fsys fs.FS) ([]string, error) {
	var files []string

	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		ext := strings.ToLower(pathExt(path))
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

// pathExt returns the file extension of a slash-separated FS path.
func pathExt(path string) string {
	for i := len(path) - 1; i >= 0 && path[i] != '/'; i-- {
		if path[i] == '.' {
			return path[i:]
		}
	}

	return ""
}

// ToUnstructured converts a client.Object into its unstructured form.
func ToUnstructured(obj client.Object) *unstructured.Unstructured {
	if unstr, ok := obj.(*unstructured.Unstructured); ok {
		return unstr
	}

	objectMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		panic(err)
	}

	return &unstructured.Unstructured{Object: objectMap}
}

// IsRBACObject reports whether obj is an RBAC object (ServiceAccount, Role,
// RoleBinding, ClusterRole, or ClusterRoleBinding) whose name contains
// nameContains. It single-sources the shared decision of which RBAC objects in a
// bundled manifest set belong to a given component, so a cluster component that
// skips another component's RBAC and the per-Site component that applies it
// cannot drift apart.
func IsRBACObject(obj *unstructured.Unstructured, nameContains string) bool {
	switch obj.GetKind() {
	case "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding":
		return strings.Contains(obj.GetName(), nameContains)
	default:
		return false
	}
}

// SiteOwnerReference builds the controller owner reference used for per-site
// resources. Using owner references rather than a Site finalizer lets Kubernetes
// garbage collect per-site workloads if a Site is deleted while the operator is
// down.
//
// Controller is set so controller-runtime's Owns() watch (which enqueues only
// via metav1.GetControllerOf) re-reconciles the Site when an owned resource
// drifts or is deleted. BlockOwnerDeletion is intentionally left unset: setting
// it triggers the OwnerReferencesPermissionEnforcement admission check for
// update on sites/finalizers, which the operator ServiceAccount does not hold,
// and would cause every owned-object apply to be rejected.
func SiteOwnerReference(site *unboundedv1alpha3.Site) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: unboundedv1alpha3.GroupVersion.String(),
		Kind:       "Site",
		Name:       site.Name,
		UID:        site.UID,
		Controller: ptr.To(true),
	}
}

// UpsertOwnerReference adds or updates owner in refs, returning whether the slice
// changed. An existing reference to the same owner (matched by UID, Kind, and
// APIVersion) is rewritten when its Name or Controller flag differs, so a
// resource adopted before Controller ownership was introduced converges to a
// controller reference on the next reconcile.
func UpsertOwnerReference(refs []metav1.OwnerReference, owner metav1.OwnerReference) ([]metav1.OwnerReference, bool) {
	for i := range refs {
		if refs[i].UID == owner.UID && refs[i].Kind == owner.Kind && refs[i].APIVersion == owner.APIVersion {
			if refs[i].Name == owner.Name && controllerEqual(refs[i].Controller, owner.Controller) {
				return refs, false
			}

			out := append([]metav1.OwnerReference(nil), refs...)
			out[i] = owner

			return out, true
		}
	}

	out := append([]metav1.OwnerReference(nil), refs...)
	out = append(out, owner)

	return out, true
}

// controllerEqual compares two owner-reference Controller pointers, treating a
// nil pointer as false.
func controllerEqual(a, b *bool) bool {
	return ptr.Deref(a, false) == ptr.Deref(b, false)
}

// SiteNodeAffinity matches Nodes carrying either the canonical site label or the
// deprecated net-prefixed site label. The OR is required during migration.
func SiteNodeAffinity(siteName string) *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					siteNodeSelectorTerm(SiteLabelKey, siteName),
					siteNodeSelectorTerm(DeprecatedSiteLabelKey, siteName),
				},
			},
		},
	}
}

func siteNodeSelectorTerm(key, siteName string) corev1.NodeSelectorTerm {
	return corev1.NodeSelectorTerm{
		MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key:      key,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{siteName},
		}},
	}
}
