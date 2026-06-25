// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	netmanifests "github.com/Azure/unbounded/deploy/net"
	operatormanifests "github.com/Azure/unbounded/deploy/unbounded-operator"
	"github.com/Azure/unbounded/internal/kube"
)

const (
	defaultInstallTimeout = 5 * time.Minute
	netNamespace          = "unbounded-net"
)

type installHandler struct {
	kubeconfigPath string
	namespace      string
	netNamespace   string

	operatorImage          string
	netControllerImage     string
	netNodeImage           string
	machinaImage           string
	metalmanImage          string
	storageSupervisorImage string
	apiServerEndpoint      string

	skipCRDs bool
	wait     bool
	timeout  time.Duration

	kubeCli          kubernetes.Interface
	kubeResourcesCli client.Client
	restConfig       *rest.Config
	logger           *slog.Logger
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
	cmd.Flags().StringVar(&handler.namespace, "namespace", machinaNamespace, "Namespace for unbounded-operator and default components")
	cmd.Flags().StringVar(&handler.netNamespace, "net-namespace", netNamespace, "Namespace for unbounded-net components")
	cmd.Flags().StringVar(&handler.operatorImage, "operator-image", "", "unbounded-operator image override")
	cmd.Flags().StringVar(&handler.netControllerImage, "net-controller-image", "", "unbounded-net controller image override")
	cmd.Flags().StringVar(&handler.netNodeImage, "net-node-image", "", "unbounded-net node image override")
	cmd.Flags().StringVar(&handler.machinaImage, "machina-image", "", "machina controller image override")
	cmd.Flags().StringVar(&handler.metalmanImage, "metalman-image", "", "metalman image override")
	cmd.Flags().StringVar(&handler.storageSupervisorImage, "storage-supervisor-image", "", "unbounded-storage supervisor image override")
	cmd.Flags().StringVar(&handler.apiServerEndpoint, "api-server-endpoint", "", "Kubernetes API server endpoint advertised to provisioned machines; defaults to the kubeconfig server")
	cmd.Flags().BoolVar(&handler.skipCRDs, "skip-crds", false, "Skip applying CRDs")
	cmd.Flags().BoolVar(&handler.wait, "wait", true, "Wait for unbounded-operator rollout")
	cmd.Flags().DurationVar(&handler.timeout, "timeout", defaultInstallTimeout, "Timeout for rollout waits")
}

func (h *installHandler) execute(ctx context.Context) error {
	if h.logger == nil {
		h.logger = slog.Default()
	}

	if h.namespace == "" {
		h.namespace = machinaNamespace
	}

	if h.netNamespace == "" {
		h.netNamespace = netNamespace
	}

	if h.timeout == 0 {
		h.timeout = defaultInstallTimeout
	}

	if h.kubeResourcesCli == nil {
		if err := h.initializeClients(); err != nil {
			return err
		}
	}

	if h.apiServerEndpoint == "" && h.restConfig != nil {
		h.apiServerEndpoint = h.restConfig.Host
	}

	if !h.skipCRDs {
		if err := applyManifestFS(ctx, h.logger, h.kubeResourcesCli, machinamanifests.Manifests, fieldManagerID, mutateCRDOnly); err != nil {
			return fmt.Errorf("applying machina CRDs: %w", err)
		}

		if err := applyManifestFS(ctx, h.logger, h.kubeResourcesCli, netmanifests.Manifests, fieldManagerID, mutateCRDOnly); err != nil {
			return fmt.Errorf("applying net CRDs: %w", err)
		}

		if h.wait {
			if err := h.waitForCRDs(ctx); err != nil {
				return err
			}
		}
	}

	if err := applyManifestFS(ctx, h.logger, h.kubeResourcesCli, operatormanifests.Manifests, fieldManagerID, h.mutateOperatorObject); err != nil {
		return fmt.Errorf("applying unbounded-operator manifests: %w", err)
	}

	if h.wait {
		if err := h.waitForOperator(ctx); err != nil {
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

	rewriteNamespace(obj, machinaNamespace, h.namespace)
	setNamespace(obj, h.namespace)

	if obj.GetKind() == "Deployment" && obj.GetName() == "unbounded-operator" {
		if err := setContainerImage(obj, "controller", h.operatorImage); err != nil {
			return err
		}

		for _, replacement := range []struct {
			prefix string
			value  string
		}{
			{prefix: "--leader-elect-namespace=", value: h.namespace},
			{prefix: "--default-namespace=", value: h.namespace},
			{prefix: "--net-namespace=", value: h.netNamespace},
			{prefix: "--net-controller-image=", value: h.netControllerImage},
			{prefix: "--net-node-image=", value: h.netNodeImage},
			{prefix: "--machina-image=", value: h.machinaImage},
			{prefix: "--metalman-image=", value: h.metalmanImage},
			{prefix: "--storage-supervisor-image=", value: h.storageSupervisorImage},
			{prefix: "--api-server-endpoint=", value: h.apiServerEndpoint},
		} {
			if err := replaceContainerArg(obj, "controller", replacement.prefix, replacement.value); err != nil {
				return err
			}
		}
	}

	return nil
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
		} else if deploymentAvailable(deploy) {
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

	crds := []string{
		"sites.unbounded-cloud.io",
		"gatewaypools.net.unbounded-cloud.io",
		"gatewaypoolnodes.net.unbounded-cloud.io",
		"gatewaypoolpeerings.net.unbounded-cloud.io",
		"sitegatewaypoolassignments.net.unbounded-cloud.io",
		"sitenodeslices.net.unbounded-cloud.io",
		"sitepeerings.net.unbounded-cloud.io",
	}

	for {
		ready := true

		for _, name := range crds {
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

func deploymentAvailable(deploy *appsv1.Deployment) bool {
	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}

	return deploy.Status.ObservedGeneration >= deploy.Generation && deploy.Status.AvailableReplicas >= desired
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

func mutateCRDOnly(obj *unstructured.Unstructured) error {
	if obj.GetKind() != "CustomResourceDefinition" {
		obj.Object = nil
	}

	return nil
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
