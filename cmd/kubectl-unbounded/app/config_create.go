// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

func configCreateCommand() *cobra.Command {
	var (
		kubernetesVersion string
		agentImage        string
		nodeLabels        []string
		taints            []string
		updateStrategy    string
		priority          int32
	)

	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a MachineConfiguration",
		Long: `Create a MachineConfiguration resource that defines a configuration
profile for a class of machines. The machina controller automatically
creates a MachineConfigurationVersion (v1) from the provided settings.

Example:
  kubectl unbounded config create my-config \
    --kubernetes-version=v1.34.0 \
    --agent-image=ghcr.io/azure/unbounded-agent:latest \
    --node-labels=env=prod,tier=worker \
    --taints=dedicated=gpu:NoSchedule`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			return runConfigCreate(ctx, c, args[0], configCreateOpts{
				kubernetesVersion: kubernetesVersion,
				agentImage:        agentImage,
				nodeLabels:        nodeLabels,
				taints:            taints,
				updateStrategy:    updateStrategy,
				priority:          priority,
			})
		},
	}

	cmd.Flags().StringVar(&kubernetesVersion, "kubernetes-version", "", "Kubernetes version (e.g. v1.34.0)")
	cmd.Flags().StringVar(&agentImage, "agent-image", "", "Agent OCI image reference")
	cmd.Flags().StringSliceVar(&nodeLabels, "node-labels", nil, "Node labels as key=value pairs (comma-separated)")
	cmd.Flags().StringSliceVar(&taints, "taints", nil, "Taints as key=value:Effect (comma-separated)")
	cmd.Flags().StringVar(&updateStrategy, "update-strategy", "OnDelete", "Update strategy: OnDelete or RollingUpdate")
	cmd.Flags().Int32Var(&priority, "priority", 0, "Priority for machine selector ordering")

	return cmd
}

type configCreateOpts struct {
	kubernetesVersion string
	agentImage        string
	nodeLabels        []string
	taints            []string
	updateStrategy    string
	priority          int32
}

func runConfigCreate(ctx context.Context, c client.WithWatch, name string, opts configCreateOpts) error {
	mc := &v1alpha3.MachineConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1alpha3.MachineConfigurationSpec{
			Template: v1alpha3.MachineConfigurationTemplate{},
			UpdateStrategy: v1alpha3.MachineConfigurationUpdateStrategy{
				Type: v1alpha3.MachineConfigurationUpdateStrategyType(opts.updateStrategy),
			},
			Priority: opts.priority,
		},
	}

	// Build kubernetes config if any k8s fields are set.
	if opts.kubernetesVersion != "" || len(opts.nodeLabels) > 0 || len(opts.taints) > 0 {
		k8s := &v1alpha3.MachineConfigurationKubernetes{}

		if opts.kubernetesVersion != "" {
			k8s.Version = opts.kubernetesVersion
		}

		if len(opts.nodeLabels) > 0 {
			k8s.NodeLabels = parseKeyValuePairs(opts.nodeLabels)
		}

		if len(opts.taints) > 0 {
			k8s.RegisterWithTaints = opts.taints
		}

		mc.Spec.Template.Kubernetes = k8s
	}

	if opts.agentImage != "" {
		mc.Spec.Template.Agent = &v1alpha3.MachineConfigurationAgent{
			Image: opts.agentImage,
		}
	}

	if err := c.Create(ctx, mc); err != nil {
		return fmt.Errorf("creating MachineConfiguration: %w", err)
	}

	printStep(fmt.Sprintf("Created MachineConfiguration %s", name))

	if opts.kubernetesVersion != "" {
		printConfig("kubernetes", opts.kubernetesVersion)
	}

	if opts.agentImage != "" {
		printConfig("agent-image", opts.agentImage)
	}

	if len(opts.nodeLabels) > 0 {
		printConfig("node-labels", strings.Join(opts.nodeLabels, ", "))
	}

	if len(opts.taints) > 0 {
		printConfig("taints", strings.Join(opts.taints, ", "))
	}

	printConfig("update-strategy", opts.updateStrategy)
	fmt.Println()

	printStep("MachineConfigurationVersion v1 will be created by the controller")
	fmt.Println()

	return nil
}

// parseKeyValuePairs converts ["key=value", ...] into a map.
func parseKeyValuePairs(pairs []string) map[string]string {
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, _ := strings.Cut(p, "=")
		m[k] = v
	}

	return m
}
