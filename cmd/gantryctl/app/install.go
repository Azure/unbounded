// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	appstypedv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gantrystandalone "github.com/Azure/unbounded/deploy/gantry-standalone"
)

const (
	fieldManager            = "gantryctl"
	agentDaemonSetName      = "gantry-standalone"
	nodeConfigDaemonSetName = "gantry-standalone-node-config"
	agentConfigMapName      = "gantry-standalone-config"
	nodeRoutesConfigMapName = "gantry-standalone-node-routes"
	registryCredentialsName = "gantry-standalone-registry-credentials"
	nodeConfigManifestName  = "03-node-config.yaml.tmpl"
)

type installOptions struct {
	root  *rootOptions
	image string
	wait  bool
}

func newInstallCommand(root *rootOptions) *cobra.Command {
	options := &installOptions{root: root, image: defaultGantryImage(), wait: true}
	command := &cobra.Command{
		Use:   "install",
		Short: "Install a standalone, initially unconfigured Gantry agent",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return options.run(command.Context(), command.OutOrStdout())
		},
	}
	command.Flags().StringVar(&options.image, "image", options.image, "Gantry agent image")
	command.Flags().BoolVar(&options.wait, "wait", options.wait, "Wait for all Gantry agents to become ready")

	return command
}

func (o *installOptions) run(ctx context.Context, output io.Writer) (returnErr error) {
	clients, err := o.root.clusterClients()
	if err != nil {
		return err
	}

	created := make([]*unstructured.Unstructured, 0)
	rollbackCreated := true

	defer func() {
		if returnErr == nil || !rollbackCreated || len(created) == 0 {
			return
		}

		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.root.timeout)
		defer cancel()

		returnErr = errors.Join(returnErr, deleteInstallObjects(rollbackCtx, clients.resources, created))
	}()

	if err := ensureNoCompetingMirror(ctx, clients.resources, o.root.namespace); err != nil {
		return err
	}

	manifests, err := gantrystandalone.Render(gantrystandalone.Values{Namespace: o.root.namespace, Image: o.image})
	if err != nil {
		return err
	}

	for _, manifest := range manifests {
		if !baseInstallManifest(manifest.Name) {
			continue
		}

		if err := applyInstallManifestTracked(ctx, clients.resources, manifest.Data, &created); err != nil {
			return fmt.Errorf("apply %s: %w", manifest.Name, err)
		}
	}

	store, err := loadRegistryStore(ctx, clients.kube, o.root.namespace)
	if err != nil {
		return err
	}

	registryCount := len(store.agentConfig.UpstreamRegistries)

	nodeConfigExists := false

	nodeConfig, err := clients.kube.AppsV1().DaemonSets(o.root.namespace).Get(ctx, nodeConfigDaemonSetName, metav1.GetOptions{})
	if err == nil {
		if nodeConfig.Labels["app.kubernetes.io/managed-by"] != fieldManager {
			return fmt.Errorf("DaemonSet %s/%s is not owned by gantryctl", o.root.namespace, nodeConfigDaemonSetName)
		}

		nodeConfigExists = true
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get existing node-config DaemonSet: %w", err)
	}

	nodeConfigNeeded := nodeConfigRequired(nodeConfigExists, len(store.routes.Registries))
	if nodeConfigNeeded {
		if err := applyNodeConfigManifestTracked(ctx, clients.resources, o.root.namespace, o.image, &created); err != nil {
			return fmt.Errorf("apply node-config DaemonSet: %w", err)
		}
	}

	if o.wait {
		if err := waitForDaemonSet(ctx, clients.kube, o.root.namespace, agentDaemonSetName, o.root.timeout); err != nil {
			return err
		}

		if nodeConfigNeeded {
			if err := waitForDaemonSet(ctx, clients.kube, o.root.namespace, nodeConfigDaemonSetName, o.root.timeout); err != nil {
				return err
			}
		}
	}

	verb := "is ready"
	if !o.wait {
		verb = "was applied"
	}

	rollbackCreated = false

	if registryCount == 0 {
		return writeOutputf(output,
			"Standalone Gantry %s in namespace %s with no registry routes configured.\nNext: gantryctl registry add <registry-host> --auth delegated\n",
			verb, o.root.namespace,
		)
	} else {
		return writeOutputf(output, "Standalone Gantry %s in namespace %s with %d registry route(s) preserved.\n", verb, o.root.namespace, registryCount)
	}
}

func baseInstallManifest(name string) bool {
	return name != nodeConfigManifestName
}

func nodeConfigRequired(exists bool, routeCount int) bool {
	return exists || routeCount > 0
}

func ensureNoCompetingMirror(ctx context.Context, resourceClient client.Client, namespace string) error {
	var daemonSets appsv1.DaemonSetList
	if err := resourceClient.List(ctx, &daemonSets); err != nil {
		return fmt.Errorf("list DaemonSets for Gantry conflict preflight: %w", err)
	}

	for index := range daemonSets.Items {
		daemonSet := &daemonSets.Items[index]
		if daemonSet.Namespace == namespace && daemonSet.Name == agentDaemonSetName {
			if daemonSet.Labels["app.kubernetes.io/managed-by"] != fieldManager {
				return fmt.Errorf("DaemonSet %s/%s is not owned by gantryctl", daemonSet.Namespace, daemonSet.Name)
			}

			continue
		}

		for _, container := range daemonSet.Spec.Template.Spec.Containers {
			for _, port := range container.Ports {
				if port.HostPort == 5000 {
					return fmt.Errorf("DaemonSet %s/%s already reserves host port 5000; refusing to overlap standalone Gantry", daemonSet.Namespace, daemonSet.Name)
				}
			}
		}
	}

	return nil
}

func applyInstallManifest(ctx context.Context, resourceClient client.Client, data []byte) error {
	return applyInstallManifestTracked(ctx, resourceClient, data, nil)
}

func applyInstallManifestTracked(ctx context.Context, resourceClient client.Client, data []byte, created *[]*unstructured.Unstructured) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err != nil {
			if err == io.EOF {
				return nil
			}

			return fmt.Errorf("decode manifest: %w", err)
		}

		if object.Object == nil {
			continue
		}

		key := types.NamespacedName{Namespace: object.GetNamespace(), Name: object.GetName()}
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(object.GroupVersionKind())

		err := resourceClient.Get(ctx, key, existing)
		objectMissing := apierrors.IsNotFound(err)
		if err == nil {
			owned := existing.GetLabels()["app.kubernetes.io/managed-by"] == fieldManager
			if object.GetKind() == "Namespace" && !owned {
				continue
			}

			if !owned {
				return fmt.Errorf("%s %s is not owned by gantryctl", object.GetKind(), key)
			}

			if object.GetKind() == "ConfigMap" {
				continue
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get %s %s: %w", object.GetKind(), key, err)
		}

		configuration := client.ApplyConfigurationFromUnstructured(object)
		if err := resourceClient.Apply(ctx, configuration, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply %s %s: %w", object.GetKind(), object.GetName(), err)
		}

		if objectMissing && created != nil {
			createdObject := &unstructured.Unstructured{}
			createdObject.SetGroupVersionKind(object.GroupVersionKind())
			createdObject.SetNamespace(object.GetNamespace())
			createdObject.SetName(object.GetName())
			*created = append(*created, createdObject)
		}
	}
}

func deleteInstallObjects(ctx context.Context, resourceClient client.Client, created []*unstructured.Unstructured) error {
	var errs []error

	for index := len(created) - 1; index >= 0; index-- {
		object := created[index].DeepCopy()
		if err := resourceClient.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("roll back created %s %s/%s: %w", object.GetKind(), object.GetNamespace(), object.GetName(), err))
		}
	}

	return errors.Join(errs...)
}

func applyNodeConfigManifest(ctx context.Context, resourceClient client.Client, namespace, image string) error {
	return applyNodeConfigManifestTracked(ctx, resourceClient, namespace, image, nil)
}

func applyNodeConfigManifestTracked(ctx context.Context, resourceClient client.Client, namespace, image string, created *[]*unstructured.Unstructured) error {
	manifests, err := gantrystandalone.Render(gantrystandalone.Values{Namespace: namespace, Image: image})
	if err != nil {
		return err
	}

	for _, manifest := range manifests {
		if manifest.Name == nodeConfigManifestName {
			return applyInstallManifestTracked(ctx, resourceClient, manifest.Data, created)
		}
	}

	return errors.New("standalone node-config manifest is missing")
}

func waitForDaemonSet(ctx context.Context, kubeClient interface {
	AppsV1() appstypedv1.AppsV1Interface
}, namespace, name string, timeout time.Duration,
) error {
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		daemonSet, err := kubeClient.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, fmt.Errorf("DaemonSet %s/%s not found", namespace, name)
		}

		if err != nil {
			return false, err
		}

		return daemonSetReady(daemonSet), nil
	})
}
