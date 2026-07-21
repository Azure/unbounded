// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machina

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/components/metalman"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, unboundedv1alpha3.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return scheme
}

func siteEnabling(name string) *unboundedv1alpha3.Site {
	enabled := true

	return &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
			Machina: &unboundedv1alpha3.MachinaComponentSpec{
				SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
			},
		}},
	}
}

func TestEnabledFor(t *testing.T) {
	if EnabledFor(&unboundedv1alpha3.Site{}) {
		t.Fatal("machina enabled with no component spec")
	}

	if !EnabledFor(siteEnabling("edge")) {
		t.Fatal("machina not enabled when spec enables it")
	}
}

func TestApplyMutatorSkipsMetalmanSupportAndCRD(t *testing.T) {
	for _, obj := range []*unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "ClusterRole",
			"metadata":   map[string]any{"name": "metalman-controller"},
		}},
		{Object: map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1",
			"kind":       component.CRDKind,
			"metadata":   map[string]any{"name": "sites.unbounded-cloud.io"},
		}},
	} {
		if err := applyMutator(component.Config{}, "hash")(obj); err != nil {
			t.Fatalf("applyMutator: %v", err)
		}

		if obj.Object != nil {
			t.Fatalf("object was not skipped: %#v", obj.Object)
		}
	}
}

// TestApplyMutatorSkipsExactlyMetalmanRBAC guards the cross-component invariant
// that machina skips every metalman RBAC object metalman owns and applies. Both
// sides now decide via component.IsRBACObject, so this catches any future drift
// between them across the real machina manifest set.
func TestApplyMutatorSkipsExactlyMetalmanRBAC(t *testing.T) {
	files, err := component.YamlFiles(machinamanifests.Manifests)
	if err != nil {
		t.Fatalf("list machina manifests: %v", err)
	}

	supportSeen := 0

	for _, file := range files {
		data, err := fs.ReadFile(machinamanifests.Manifests, file)
		if err != nil {
			t.Fatalf("read manifest %s: %v", file, err)
		}

		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

		for {
			obj := &unstructured.Unstructured{}
			if err := decoder.Decode(obj); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				t.Fatalf("decode manifest %s: %v", file, err)
			}

			if obj.Object == nil || !metalman.IsSupportObject(obj) {
				continue
			}

			supportSeen++

			work := obj.DeepCopy()
			if err := applyMutator(component.Config{}, "hash")(work); err != nil {
				t.Fatalf("applyMutator: %v", err)
			}

			if work.Object != nil {
				t.Fatalf("machina applied metalman RBAC %s/%s that the metalman component owns", obj.GetKind(), obj.GetName())
			}
		}
	}

	if supportSeen == 0 {
		t.Fatal("no metalman RBAC found in machina manifests; the guard is vacuous")
	}
}

func TestApplyMutatorStampsConfigHash(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": controllerName},
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{},
			"spec":     map[string]any{"containers": []any{map[string]any{"name": "controller", "image": "old:image"}}},
		}},
	}}

	const hash = "config-hash"

	cfg := component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"}
	if err := applyMutator(cfg, hash)(obj); err != nil {
		t.Fatalf("applyMutator: %v", err)
	}

	got, found, err := unstructured.NestedString(obj.Object,
		"spec", "template", "metadata", "annotations", ConfigHashAnnotation)
	if err != nil || !found || got != hash {
		t.Fatalf("config hash = %q found=%t err=%v, want %q", got, found, err, hash)
	}

	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	image := containers[0].(map[string]any)["image"].(string)

	if image != "registry.example.com/azure/machina:v1.2.3" {
		t.Fatalf("image = %q", image)
	}
}

func TestSetAPIServerEndpointPreservesConfig(t *testing.T) {
	input := `# retained comment
apiServerEndpoint: "https://old.example:6443"
maxConcurrentReconciles: 17
customField: keep-me
`
	endpoint := "https://api.example:6443/path?mode=\"strict\"&ready=true"

	got, err := SetAPIServerEndpoint(input, endpoint)
	if err != nil {
		t.Fatalf("SetAPIServerEndpoint: %v", err)
	}

	var config map[string]any
	if err := yaml.Unmarshal([]byte(got), &config); err != nil {
		t.Fatalf("unmarshal merged config: %v", err)
	}

	if config["apiServerEndpoint"] != endpoint {
		t.Fatalf("apiServerEndpoint = %q, want %q", config["apiServerEndpoint"], endpoint)
	}

	if config["maxConcurrentReconciles"] != 17 || config["customField"] != "keep-me" {
		t.Fatalf("custom config was not preserved: %#v", config)
	}

	if !strings.Contains(got, "# retained comment") {
		t.Fatalf("config comment was not preserved: %q", got)
	}
}

func TestEnsureConfigUpdatesEndpointAndPreservesData(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: component.DefaultNamespace},
		Data: map[string]string{
			"config.yaml": "apiServerEndpoint: https://old.example:6443\ncustom: true\n",
		},
	}
	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build(),
		Namespace: component.DefaultNamespace,
		Config:    component.Config{APIServerEndpoint: "https://new.example:6443"},
	}

	hash, err := ensureConfig(t.Context(), env)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get machina config: %v", err)
	}

	var config map[string]any
	if err := yaml.Unmarshal([]byte(got.Data["config.yaml"]), &config); err != nil {
		t.Fatalf("unmarshal updated config: %v", err)
	}

	if config["apiServerEndpoint"] != "https://new.example:6443" || config["custom"] != true {
		t.Fatalf("updated config = %#v", config)
	}

	if hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want hash of exact stored config", hash)
	}
}

func TestEnsureConfigOptimisticConflictReloadsFreshState(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: component.DefaultNamespace, ResourceVersion: "1"},
		Data:       map[string]string{"config.yaml": "apiServerEndpoint: https://old.example:6443\n"},
	}

	base := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	conflicted := false
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, underlying client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			data, err := patch.Data(obj)
			if err != nil {
				t.Fatalf("encode patch: %v", err)
			}

			if !bytes.Contains(data, []byte(`"resourceVersion":`)) {
				t.Fatalf("optimistic patch does not include resourceVersion: %s", data)
			}

			if !conflicted {
				conflicted = true

				var latest corev1.ConfigMap
				if err := underlying.Get(ctx, client.ObjectKeyFromObject(existing), &latest); err != nil {
					t.Fatalf("get concurrent config: %v", err)
				}

				latest.Data["concurrent"] = "preserved"
				if err := underlying.Update(ctx, &latest); err != nil {
					t.Fatalf("write concurrent config: %v", err)
				}

				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("concurrent edit"))
			}

			return underlying.Patch(ctx, obj, patch, opts...)
		},
	})
	env := &component.Env{Client: cl, Namespace: component.DefaultNamespace, Config: component.Config{APIServerEndpoint: "https://new.example:6443"}}

	if _, err := ensureConfig(t.Context(), env); !apierrors.IsConflict(err) {
		t.Fatalf("first ensureConfig error = %v, want conflict", err)
	}

	if _, err := ensureConfig(t.Context(), env); err != nil {
		t.Fatalf("retry ensureConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get updated config: %v", err)
	}

	if got.Data["concurrent"] != "preserved" || !strings.Contains(got.Data["config.yaml"], "https://new.example:6443") {
		t.Fatalf("retry did not preserve fresh config: %#v", got.Data)
	}
}

func TestReconcileRetainedWithNoEnablingSitesUpdatesConfigAndHash(t *testing.T) {
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: configName},
		Data: map[string]string{
			"config.yaml": "apiServerEndpoint: https://old.example:6443\ncustom: retained\n",
		},
	}
	env, appliedHashes := retainedEnv(t, config)
	env.Config.APIServerEndpoint = "https://new.example:6443"

	res := Component{}.Reconcile(t.Context(), env, nil)
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(config), &got); err != nil {
		t.Fatalf("get retained machina config: %v", err)
	}

	if !strings.Contains(got.Data["config.yaml"], "https://new.example:6443") ||
		!strings.Contains(got.Data["config.yaml"], "custom: retained") {
		t.Fatalf("retained machina config was not merged: %q", got.Data["config.yaml"])
	}

	wantHash := component.ConfigMapPayloadHash(&got)
	if appliedHashes[controllerName] != wantHash {
		t.Fatalf("applied Machina hash = %q, want %q", appliedHashes[controllerName], wantHash)
	}
}

func TestReconcileDoesNotCreateFromNothingWithNoEnablingSites(t *testing.T) {
	env, appliedHashes := retainedEnv(t)

	res := Component{}.Reconcile(t.Context(), env, nil)
	if !res.Ready || res.Reason != component.ReasonDisabled {
		t.Fatalf("Reconcile = %+v, want ready with Disabled", res)
	}

	err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: configName}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) || len(appliedHashes) != 0 {
		t.Fatalf("zero-Site reconcile created Machina from nothing: config err=%v hashes=%#v", err, appliedHashes)
	}
}

func TestReconcileAppliesWhenSiteEnables(t *testing.T) {
	env, appliedHashes := retainedEnv(t)

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{*siteEnabling("edge")})
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: configName}, &got); err != nil {
		t.Fatalf("get created machina config: %v", err)
	}

	if appliedHashes[controllerName] != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("applied hash = %q, want %q", appliedHashes[controllerName], component.ConfigMapPayloadHash(&got))
	}
}

func retainedEnv(t *testing.T, objects ...client.Object) (*component.Env, map[string]string) {
	t.Helper()

	scheme := testScheme(t)
	appliedHashes := map[string]string{}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applied, ok := obj.(interface {
					GetName() string
					GetKind() string
					UnstructuredContent() map[string]any
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				if applied.GetKind() != "Deployment" || applied.GetName() != controllerName {
					return nil
				}

				hash, _, err := unstructured.NestedString(
					applied.UnstructuredContent(),
					"spec", "template", "metadata", "annotations", ConfigHashAnnotation,
				)
				if err != nil {
					t.Fatalf("get applied Machina hash: %v", err)
				}

				appliedHashes[applied.GetName()] = hash

				return nil
			},
		}).
		Build()

	return &component.Env{Client: cl, Scheme: scheme, Namespace: component.DefaultNamespace}, appliedHashes
}
