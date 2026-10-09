// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
	racercomponent "github.com/Azure/unbounded/internal/operator/components/racer"
	"github.com/Azure/unbounded/internal/operator/override"
)

const (
	namespace = "unbounded-system"
	imageTag  = "f9a088a22a29cf8549e1945af833f508a14199d1"
)

func main() {
	contextName := flag.String("context", "", "Required kubeconfig context")
	apply := flag.Bool("apply", false, "Apply only Racer resources; requires current Racer CRDs")
	overrides := flag.String("overrides", "deploy/racer-loadgen/zipf/racer-overrides.yaml", "Local Racer-only overrides, never the shared ConfigMap")

	flag.Parse()

	if err := run(*contextName, *apply, *overrides); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(contextName string, apply bool, path string) error {
	if contextName == "" || !apply {
		return fmt.Errorf("explicit --context and --apply required; install current Racer CRDs first")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	entries, problems, err := override.Parse(map[string]string{"local-racer.yaml": string(data)})
	if err != nil || len(problems) != 0 {
		return fmt.Errorf("override parse: %v %v", err, problems)
	}

	for _, entry := range entries {
		if entry.Entry.Component != "racer" {
			return fmt.Errorf("non-Racer override refused")
		}
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	if err != nil {
		return err
	}

	config.Timeout = 20 * time.Second

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}

	if err := racerv1.AddToScheme(scheme); err != nil {
		return err
	}

	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	env := &component.Env{Client: c, APIReader: c, Scheme: scheme, Namespace: namespace, Config: component.Config{ImageRegistry: "ghcr.io/azure", ImageTag: imageTag}}
	// This tool is only for coexistence with the known release without Racer.
	operator := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "unbounded-operator"}, operator); err != nil {
		return err
	}

	if len(operator.Spec.Template.Spec.Containers) != 1 || operator.Spec.Template.Spec.Containers[0].Image != "ghcr.io/azure/unbounded-operator:v0.8.0" {
		return fmt.Errorf("refusing an operator other than the verified Racer-free v0.8.0")
	}

	objects, err := env.DecodeManifestFiles(manifests.Manifests, []string{"create-restriction.yaml", "node-restriction.yaml"}, nil)
	if err != nil {
		return err
	}

	for _, obj := range objects {
		if err := env.ApplyObject(ctx, obj); err != nil {
			return err
		}
	}

	for _, obj := range objects {
		if obj.GetKind() != "ValidatingAdmissionPolicy" {
			continue
		}

		for {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(obj.GroupVersionKind())

			if err := c.Get(ctx, client.ObjectKey{Name: obj.GetName()}, live); err != nil {
				return err
			}

			observed, _, err := unstructured.NestedInt64(live.Object, "status", "observedGeneration")
			if err != nil {
				return fmt.Errorf("policy %s observed generation: %w", obj.GetName(), err)
			}

			warnings, _, err := unstructured.NestedSlice(live.Object, "status", "typeChecking", "expressionWarnings")
			if err != nil {
				return fmt.Errorf("policy %s type warnings: %w", obj.GetName(), err)
			}

			if observed >= live.GetGeneration() {
				if len(warnings) != 0 {
					return fmt.Errorf("policy %s type warnings: %v", obj.GetName(), warnings)
				}

				break
			}

			if err := pause(ctx); err != nil {
				return err
			}
		}
	}

	volume := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "racer-loadgen"}}

	volume.Spec.Type = "Cache"
	if err := c.Create(ctx, volume); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	for {
		plan, result, err := racercomponent.New().Plan(ctx, env, nil)
		if err != nil {
			return err
		}

		report := override.Apply(plan, entries, nil)
		if report.Failed() {
			return report.Err()
		}

		if err := scoped(plan); err != nil {
			return err
		}
		// Existing dataplanes belong to the parent after initial creation.
		filtered := plan.Operations[:0]
		for _, op := range plan.Operations {
			if op.Object.GetKind() == "DaemonSet" {
				existing := &appsv1.DaemonSet{}

				err := c.Get(ctx, client.ObjectKeyFromObject(op.Object), existing)
				if err == nil {
					continue
				}

				if !apierrors.IsNotFound(err) {
					return err
				}

				op.Kind = component.OpCreateIfAbsent
			}

			filtered = append(filtered, op)
		}

		plan.Operations = filtered
		if err := executePlan(ctx, env, plan); err != nil {
			return err
		}

		fmt.Fprintf(os.Stderr, "Racer phase: %s; operations=%d\n", result.Message, len(plan.Operations))

		controller := &appsv1.Deployment{}

		dp := &appsv1.DaemonSet{}
		if c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "racer-controller"}, controller) == nil && c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "racer-dataplane"}, dp) == nil {
			if workloadsReady(controller, dp) {
				fmt.Fprintf(os.Stderr, "Racer ready: controller=%d dataplanes=%d\n", controller.Status.AvailableReplicas, dp.Status.NumberReady)
				return nil
			}
		}

		if err := pause(ctx); err != nil {
			return err
		}
	}
}

func executePlan(ctx context.Context, env *component.Env, plan *component.Plan) error {
	result, err := env.Execute(ctx, plan)
	if err != nil {
		return err
	}

	return result.Err()
}

func workloadsReady(controller *appsv1.Deployment, dp *appsv1.DaemonSet) bool {
	return controller.Status.ObservedGeneration >= controller.Generation &&
		controller.Status.Replicas == 3 && controller.Status.UpdatedReplicas == 3 && controller.Status.AvailableReplicas == 3 &&
		dp.Status.ObservedGeneration >= dp.Generation && dp.Status.DesiredNumberScheduled > 0 &&
		dp.Status.UpdatedNumberScheduled == dp.Status.DesiredNumberScheduled && dp.Status.NumberReady == dp.Status.DesiredNumberScheduled
}

func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return nil
	}
}

func scoped(plan *component.Plan) error {
	for _, op := range plan.Operations {
		if op.Object == nil {
			return fmt.Errorf("operation without an object refused")
		}

		if op.Component != "racer" || op.Kind == component.OpDelete || (op.Object.GetNamespace() != "" && op.Object.GetNamespace() != namespace) {
			return fmt.Errorf("out-of-scope operation refused: %s", op.Ref())
		}

		if op.Object.GetName() == "unbounded-component-overrides" || op.Object.GetKind() == "Node" || op.Object.GetKind() == "Namespace" {
			return fmt.Errorf("protected resource refused: %s", op.Ref())
		}
	}

	return nil
}
