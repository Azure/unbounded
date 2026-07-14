// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatormanifests "github.com/Azure/unbounded/deploy/unbounded-operator"
	"github.com/Azure/unbounded/internal/kube"
	"github.com/Azure/unbounded/internal/operator"
	"github.com/Azure/unbounded/internal/unbounded"
)

const defaultInstallTimeout = 5 * time.Minute

const (
	// operatorConfigHashAnnotation is stamped onto the operator Deployment pod
	// template so a change to the operator ConfigMap data triggers a rollout.
	operatorConfigHashAnnotation = "unbounded-cloud.io/operator-config-hash"

	// operatorCRDRepairAnnotation restarts an existing operator when its startup
	// CRD bootstrap needs to repair a missing or unestablished CRD.
	operatorCRDRepairAnnotation = "unbounded-cloud.io/operator-crd-repair-token"
)

type installHandler struct {
	kubeconfigPath string
	namespace      string

	operatorImage     string
	metalmanImage     string
	apiServerEndpoint string

	wait    bool
	timeout time.Duration

	kubeCli          kubernetes.Interface
	kubeResourcesCli client.Client
	restConfig       *rest.Config
	logger           *slog.Logger

	operatorConfigData  map[string]string
	operatorRepairToken string
	newRepairToken      func() (string, error)
}

func installCommand() *cobra.Command {
	handler := installHandler{}

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Bootstrap Unbounded CRDs and the unbounded-operator",
		Long: `Bootstrap the cluster with the CRDs and unbounded-operator needed to
reconcile Site.spec.components. Component workloads such as unbounded-net,
machina, metalman, and unbounded-storage are deployed by the operator after
Sites are created or updated.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return handler.execute(cmd.Context())
		},
	}

	addInstallFlags(cmd, &handler)

	return cmd
}

func addInstallFlags(cmd *cobra.Command, handler *installHandler) {
	cmd.Flags().StringVar(&handler.kubeconfigPath, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringVar(&handler.namespace, "namespace", unbounded.SystemNamespace(), "Namespace for unbounded-operator and default components")
	cmd.Flags().StringVar(&handler.operatorImage, "operator-image", "", "unbounded-operator image override")
	cmd.Flags().StringVar(&handler.metalmanImage, "metalman-image", "", "metalman image override")
	cmd.Flags().StringVar(&handler.apiServerEndpoint, "api-server-endpoint", "", "Override the Kubernetes API server endpoint advertised to provisioned machines; by default the operator auto-discovers it from kube-public/cluster-info, or the KUBERNETES_SERVICE_HOST FQDN on clusters (e.g. AKS) that do not publish cluster-info")
	cmd.Flags().BoolVar(&handler.wait, "wait", true, "Wait for unbounded-operator rollout")
	cmd.Flags().DurationVar(&handler.timeout, "timeout", defaultInstallTimeout, "Timeout for rollout waits")
}

func (h *installHandler) execute(ctx context.Context) error {
	// execute can be called repeatedly in tests and by command embedders. Do not
	// carry values derived from the previous cluster state into this run.
	h.operatorConfigData = nil
	h.operatorRepairToken = ""

	if h.logger == nil {
		h.logger = slog.Default()
	}

	if h.namespace == "" {
		h.namespace = unbounded.SystemNamespace()
	}

	// Refuse to install into a legacy namespace: the operator's migration reaper
	// drains and deletes these namespaces, so installing into one would delete
	// the components we just bootstrapped.
	if unbounded.IsLegacyNamespace(h.namespace) {
		return fmt.Errorf(
			"refusing to install into legacy namespace %q: the operator's migration reaper drains and deletes this namespace; choose a different --namespace (default %q)",
			h.namespace, unbounded.SystemNamespace(),
		)
	}

	if h.timeout == 0 {
		h.timeout = defaultInstallTimeout
	}

	if h.kubeResourcesCli == nil {
		if err := h.initializeClients(); err != nil {
			return err
		}
	}

	if err := h.prepareOperatorConfig(ctx); err != nil {
		return err
	}

	// CRDs are installed and upgraded by the operator itself at startup
	// (operator.BootstrapCRDs). install only bootstraps the operator; applying
	// the operator manifests and waiting for its rollout is enough, because the
	// operator becomes Ready only after it has established the CRDs.
	if err := h.prepareOperatorRepair(ctx); err != nil {
		return err
	}

	if err := applyManifestFS(ctx, h.logger, h.kubeResourcesCli, operatormanifests.Manifests, fieldManagerID, h.mutateOperatorObject); err != nil {
		return fmt.Errorf("applying unbounded-operator manifests: %w", err)
	}

	if h.wait {
		if err := h.waitForOperator(ctx); err != nil {
			return err
		}

		if err := h.waitForCRDs(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (h *installHandler) initializeClients() error {
	h.kubeconfigPath = getKubeconfigPath(h.kubeconfigPath)
	if !isReadableFile(h.kubeconfigPath) {
		return fmt.Errorf("kubeconfig %q not readable", h.kubeconfigPath)
	}

	kubeCli, restCfg, err := kube.ClientAndConfigFromFile(h.kubeconfigPath)
	if err != nil {
		return fmt.Errorf("creating Kubernetes client: %w", err)
	}

	kubeResourcesCli, err := client.New(restCfg, client.Options{})
	if err != nil {
		return fmt.Errorf("creating controller-runtime client: %w", err)
	}

	h.kubeCli = kubeCli
	h.kubeResourcesCli = kubeResourcesCli
	h.restConfig = restCfg

	return nil
}

func (h *installHandler) mutateOperatorObject(obj *unstructured.Unstructured) error {
	if obj.GetKind() == "" {
		obj.Object = nil
		return nil
	}

	rewriteNamespace(obj, unbounded.SystemNamespace(), h.namespace)
	setNamespace(obj, h.namespace)

	// The api-server-endpoint is delivered to the operator via the
	// unbounded-operator-config ConfigMap (envFrom), not a Deployment arg, so
	// install and the apply-only path share one config surface.
	if obj.GetKind() == "ConfigMap" && obj.GetName() == "unbounded-operator-config" {
		if h.operatorConfigData == nil {
			return fmt.Errorf("operator config must be prepared before manifest mutation")
		}

		data, found, err := unstructured.NestedStringMap(obj.Object, "data")
		if err != nil {
			return fmt.Errorf("get operator config data: %w", err)
		}

		if !found {
			return fmt.Errorf("operator config data not found")
		}

		for key, value := range h.operatorConfigData {
			data[key] = value
		}

		if err := unstructured.SetNestedStringMap(obj.Object, data, "data"); err != nil {
			return fmt.Errorf("set operator config data: %w", err)
		}

		h.operatorConfigData = data
	}

	if obj.GetKind() == "Deployment" && obj.GetName() == "unbounded-operator" {
		if err := setContainerImage(obj, "controller", h.operatorImage); err != nil {
			return err
		}

		for _, replacement := range []struct {
			prefix string
			value  string
		}{
			{prefix: "--leader-elect-namespace=", value: h.namespace},
			{prefix: "--namespace=", value: h.namespace},
			{prefix: "--metalman-image=", value: h.metalmanImage},
		} {
			if err := replaceContainerArg(obj, "controller", replacement.prefix, replacement.value); err != nil {
				return err
			}
		}

		if h.operatorConfigData == nil {
			return fmt.Errorf("operator ConfigMap must be processed before Deployment")
		}

		// Hash the complete final ConfigMap data. Canonical JSON matches sprig's
		// toJson in the deployment template and is independent of map order.
		if err := unstructured.SetNestedField(obj.Object, operatorConfigHash(h.operatorConfigData), "spec", "template", "metadata", "annotations", operatorConfigHashAnnotation); err != nil {
			return fmt.Errorf("set operator config hash annotation: %w", err)
		}

		if h.operatorRepairToken != "" {
			if err := unstructured.SetNestedField(obj.Object, h.operatorRepairToken, "spec", "template", "metadata", "annotations", operatorCRDRepairAnnotation); err != nil {
				return fmt.Errorf("set operator CRD repair annotation: %w", err)
			}
		}
	}

	return nil
}

// operatorConfigHash returns the SHA-256 hash of the config data's canonical
// JSON representation. encoding/json sorts map keys, matching sprig's toJson.
func operatorConfigHash(data map[string]string) string {
	canonical, err := json.Marshal(data)
	if err != nil {
		panic(fmt.Sprintf("marshal operator config data: %v", err))
	}

	sum := sha256.Sum256(canonical)

	return hex.EncodeToString(sum[:])
}

func (h *installHandler) prepareOperatorConfig(ctx context.Context) error {
	// The operator auto-discovers the API server endpoint from
	// kube-public/cluster-info at runtime, so the endpoint is only stored when
	// explicitly overridden via --api-server-endpoint. A previously stored
	// override is preserved across reinstalls (like the reaper flag) rather than
	// being cleared or replaced with the kubeconfig host.
	endpoint := ""
	reapLegacyResources := true
	configMap := &unstructured.Unstructured{}
	configMap.SetAPIVersion("v1")
	configMap.SetKind("ConfigMap")

	key := client.ObjectKey{Namespace: h.namespace, Name: "unbounded-operator-config"}
	if err := h.kubeResourcesCli.Get(ctx, key, configMap); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("inspect unbounded-operator-config ConfigMap: %w", err)
		}
	} else {
		data, _, err := unstructured.NestedStringMap(configMap.Object, "data")
		if err != nil {
			return fmt.Errorf("get existing unbounded-operator-config data: %w", err)
		}

		endpoint = data["UNBOUNDED_API_SERVER_ENDPOINT"]

		if value, found := data["UNBOUNDED_REAP_LEGACY_RESOURCES"]; found {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("parse existing UNBOUNDED_REAP_LEGACY_RESOURCES value %q: %w", value, err)
			}

			reapLegacyResources = parsed
		}
	}

	if h.apiServerEndpoint != "" {
		endpoint = h.apiServerEndpoint
	}

	h.operatorConfigData = map[string]string{
		"UNBOUNDED_API_SERVER_ENDPOINT":   endpoint,
		"UNBOUNDED_REAP_LEGACY_RESOURCES": strconv.FormatBool(reapLegacyResources),
	}

	return nil
}

func (h *installHandler) prepareOperatorRepair(ctx context.Context) error {
	allEstablished := true

	for _, name := range operator.RequiredCRDNames {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion("apiextensions.k8s.io/v1")
		obj.SetKind("CustomResourceDefinition")

		if err := h.kubeResourcesCli.Get(ctx, client.ObjectKey{Name: name}, obj); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("inspect customresourcedefinition %s: %w", name, err)
			}

			allEstablished = false

			continue
		}

		if !crdEstablished(obj) {
			allEstablished = false
		}
	}

	deploy := &unstructured.Unstructured{}
	deploy.SetAPIVersion("apps/v1")
	deploy.SetKind("Deployment")

	key := client.ObjectKey{Namespace: h.namespace, Name: "unbounded-operator"}
	if err := h.kubeResourcesCli.Get(ctx, key, deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("inspect unbounded-operator deployment: %w", err)
	}

	token, _, err := unstructured.NestedString(deploy.Object, "spec", "template", "metadata", "annotations", operatorCRDRepairAnnotation)
	if err != nil {
		return fmt.Errorf("get operator CRD repair annotation: %w", err)
	}

	if allEstablished {
		h.operatorRepairToken = token

		return nil
	}

	typedDeploy := &appsv1.Deployment{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(deploy.Object, typedDeploy); err != nil {
		return fmt.Errorf("convert unbounded-operator deployment: %w", err)
	}

	if !deploymentRolloutComplete(typedDeploy) {
		h.operatorRepairToken = token

		return nil
	}

	newRepairToken := h.newRepairToken
	if newRepairToken == nil {
		newRepairToken = randomRepairToken
	}

	token, err = newRepairToken()
	if err != nil {
		return fmt.Errorf("generate operator CRD repair token: %w", err)
	}

	h.operatorRepairToken = token

	return nil
}

func randomRepairToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}

	return hex.EncodeToString(value), nil
}

func (h *installHandler) waitForOperator(ctx context.Context) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	deploy := &appsv1.Deployment{}

	key := client.ObjectKey{Namespace: h.namespace, Name: "unbounded-operator"}
	for {
		if err := h.kubeResourcesCli.Get(deadlineCtx, key, deploy); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("get unbounded-operator deployment: %w", err)
			}
		} else if deploymentRolloutComplete(deploy) {
			return nil
		}

		select {
		case <-deadlineCtx.Done():
			return fmt.Errorf("waiting for unbounded-operator rollout: %w", deadlineCtx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func (h *installHandler) waitForCRDs(ctx context.Context) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	for {
		ready := true

		for _, name := range operator.RequiredCRDNames {
			obj := &unstructured.Unstructured{}
			obj.SetAPIVersion("apiextensions.k8s.io/v1")
			obj.SetKind("CustomResourceDefinition")

			if err := h.kubeResourcesCli.Get(deadlineCtx, client.ObjectKey{Name: name}, obj); err != nil {
				if !apierrors.IsNotFound(err) {
					return fmt.Errorf("get customresourcedefinition %s: %w", name, err)
				}

				ready = false

				break
			}

			if !crdEstablished(obj) {
				ready = false
				break
			}
		}

		if ready {
			return nil
		}

		select {
		case <-deadlineCtx.Done():
			return fmt.Errorf("waiting for CRDs to be established: %w", deadlineCtx.Err())
		case <-time.After(1 * time.Second):
		}
	}
}

func crdEstablished(obj *unstructured.Unstructured) bool {
	if obj.GetDeletionTimestamp() != nil {
		return false
	}

	conditions, ok, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !ok {
		return false
	}

	for _, condition := range conditions {
		conditionMap, ok := condition.(map[string]any)
		if !ok {
			continue
		}

		if conditionMap["type"] == "Established" && conditionMap["status"] == "True" {
			return true
		}
	}

	return false
}

// deploymentRolloutComplete reports whether a Deployment rollout has fully
// completed: the controller has observed the current generation and every
// replica is updated, present, and available. This is the same condition
// `kubectl rollout status` waits on.
//
// A weaker "available" check (AvailableReplicas >= desired) is not enough during
// an upgrade: with the default RollingUpdate strategy at replicas: 1 the old
// ReplicaSet stays Available while a new, possibly crash-looping, pod surges in,
// so install would report success against the old operator. Requiring
// Replicas == UpdatedReplicas == AvailableReplicas == desired rejects that case
// (old replicas gone, the new pod is the only one and is available).
func deploymentRolloutComplete(deploy *appsv1.Deployment) bool {
	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}

	return deploy.Status.ObservedGeneration >= deploy.Generation &&
		deploy.Status.UpdatedReplicas == desired &&
		deploy.Status.Replicas == desired &&
		deploy.Status.AvailableReplicas == desired
}

func applyManifestFS(ctx context.Context, logger *slog.Logger, k8sClient client.Client, manifests fs.FS, fieldManager string, mutate func(*unstructured.Unstructured) error) error {
	files, err := yamlFiles(manifests)
	if err != nil {
		return err
	}

	for _, file := range files {
		data, err := fs.ReadFile(manifests, file)
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", file, err)
		}

		if err := applyManifestData(ctx, logger, k8sClient, fieldManager, data, mutate); err != nil {
			return fmt.Errorf("apply manifest %s: %w", file, err)
		}
	}

	return nil
}

func applyManifestData(ctx context.Context, logger *slog.Logger, k8sClient client.Client, fieldManager string, data []byte, mutate func(*unstructured.Unstructured) error) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if err == io.EOF {
				break
			}

			return fmt.Errorf("decoding resource: %w", err)
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

		applyCfg := client.ApplyConfigurationFromUnstructured(obj)
		if err := k8sClient.Apply(ctx, applyCfg, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("applying %s %q: %w", obj.GetKind(), obj.GetName(), err)
		}

		logger.Info("resource applied", "kind", obj.GetKind(), "name", obj.GetName())
	}

	return nil
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

	rewriteStringValues(obj.Object, oldNamespace, newNamespace)
}

func rewriteStringValues(value any, oldValue, newValue string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, v := range typed {
			if str, ok := v.(string); ok && str == oldValue {
				typed[key] = newValue
				continue
			}

			rewriteStringValues(v, oldValue, newValue)
		}
	case []any:
		for i, v := range typed {
			if str, ok := v.(string); ok && str == oldValue {
				typed[i] = newValue
				continue
			}

			rewriteStringValues(v, oldValue, newValue)
		}
	}
}

func setContainerImage(obj *unstructured.Unstructured, containerName, image string) error {
	if image == "" {
		return nil
	}

	containers, ok, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("get deployment containers: %w", err)
	}

	if !ok {
		return nil
	}

	for i, container := range containers {
		containerMap, ok := container.(map[string]any)
		if !ok || containerMap["name"] != containerName {
			continue
		}

		containerMap["image"] = image

		containers[i] = containerMap
		if err := unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return fmt.Errorf("set deployment containers: %w", err)
		}

		return nil
	}

	return nil
}

func replaceContainerArg(obj *unstructured.Unstructured, containerName, prefix, value string) error {
	if value == "" {
		return nil
	}

	containers, ok, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("get deployment containers: %w", err)
	}

	if !ok {
		return nil
	}

	for i, container := range containers {
		containerMap, ok := container.(map[string]any)
		if !ok || containerMap["name"] != containerName {
			continue
		}

		args, ok := containerMap["args"].([]any)
		if !ok {
			return nil
		}

		for j, arg := range args {
			argString, ok := arg.(string)
			if !ok || !strings.HasPrefix(argString, prefix) {
				continue
			}

			args[j] = prefix + value
			containerMap["args"] = args

			containers[i] = containerMap
			if err := unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers"); err != nil {
				return fmt.Errorf("set deployment containers: %w", err)
			}

			return nil
		}
	}

	return nil
}
