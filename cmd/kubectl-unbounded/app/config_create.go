// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func configCreateCommand() *cobra.Command {
	var (
		kubernetesVersion string
		agentImage        string
		nodeLabels        []string
		taints            []string
		updateStrategy    string
		priority          int32
		nvidiaIMEXMode    string
		nvidiaHostProfile string
		nvidiaProfileVer  string
		nvidiaProfileHash string
	)

	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a MachineConfiguration",
		Long: `Create a MachineConfiguration resource that defines a configuration
profile for a class of machines. The machina controller automatically
creates a MachineConfigurationVersion (v1) from the provided settings.

Example:
  kubectl unbounded machine config create my-config \
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
				nvidiaIMEXMode:    nvidiaIMEXMode,
				nvidiaHostProfile: nvidiaHostProfile,
				nvidiaProfileVer:  nvidiaProfileVer,
				nvidiaProfileHash: nvidiaProfileHash,
			}, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&kubernetesVersion, "kubernetes-version", "", "Kubernetes version (e.g. v1.34.0)")
	cmd.Flags().StringVar(&agentImage, "agent-image", "", "Agent OCI image reference")
	cmd.Flags().StringSliceVar(&nodeLabels, "node-labels", nil, "Node labels as key=value pairs (comma-separated)")
	cmd.Flags().StringSliceVar(&taints, "taints", nil, "Taints as key=value:Effect (comma-separated)")
	cmd.Flags().StringVar(&updateStrategy, "update-strategy", "OnDelete", "Update strategy: OnDelete or RollingUpdate")
	cmd.Flags().Int32Var(&priority, "priority", 0, "Priority for machine selector ordering")
	cmd.Flags().StringVar(&nvidiaIMEXMode, "nvidia-imex-mode", "", "NVIDIA IMEX lifecycle mode: DRA")
	cmd.Flags().StringVar(&nvidiaHostProfile, "nvidia-host-profile", "", "Prequalified NVIDIA host profile name")
	cmd.Flags().StringVar(&nvidiaProfileVer, "nvidia-host-profile-version", "", "Prequalified NVIDIA host profile version")
	cmd.Flags().StringVar(&nvidiaProfileHash, "nvidia-host-profile-digest", "", "sha256 digest of the RFC 8785 canonical host qualification document")

	return cmd
}

type configCreateOpts struct {
	kubernetesVersion string
	agentImage        string
	nodeLabels        []string
	taints            []string
	updateStrategy    string
	priority          int32
	nvidiaIMEXMode    string
	nvidiaHostProfile string
	nvidiaProfileVer  string
	nvidiaProfileHash string
}

func runConfigCreate(ctx context.Context, c client.WithWatch, name string, opts configCreateOpts, out io.Writer) error {
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
			taints, err := parseTaints(opts.taints)
			if err != nil {
				return fmt.Errorf("parsing taints: %w", err)
			}

			k8s.RegisterWithTaints = taints
		}

		mc.Spec.Template.Kubernetes = k8s
	}

	if opts.nvidiaIMEXMode != "" && opts.nvidiaIMEXMode != string(v1alpha3.NVIDIAIMEXModeDRA) {
		return fmt.Errorf("invalid NVIDIA IMEX mode %q: supported value is %q", opts.nvidiaIMEXMode, v1alpha3.NVIDIAIMEXModeDRA)
	}

	if opts.nvidiaIMEXMode == string(v1alpha3.NVIDIAIMEXModeDRA) {
		switch {
		case strings.TrimSpace(opts.nvidiaHostProfile) == "":
			return fmt.Errorf("--nvidia-host-profile is required with --nvidia-imex-mode=DRA")
		case strings.TrimSpace(opts.nvidiaProfileVer) == "":
			return fmt.Errorf("--nvidia-host-profile-version is required with --nvidia-imex-mode=DRA")
		case strings.TrimSpace(opts.nvidiaProfileHash) == "":
			return fmt.Errorf("--nvidia-host-profile-digest is required with --nvidia-imex-mode=DRA")
		}
	} else if opts.nvidiaHostProfile != "" || opts.nvidiaProfileVer != "" || opts.nvidiaProfileHash != "" {
		return fmt.Errorf("NVIDIA host qualification flags require --nvidia-imex-mode=DRA")
	}

	if opts.agentImage != "" || opts.nvidiaIMEXMode != "" {
		mc.Spec.Template.Agent = &v1alpha3.MachineConfigurationAgent{
			Image: opts.agentImage,
		}
		if opts.nvidiaIMEXMode != "" {
			mc.Spec.Template.Agent.NVIDIA = &v1alpha3.MachineConfigurationNVIDIA{
				IMEX: &v1alpha3.MachineConfigurationNVIDIAIMEX{
					Mode:               v1alpha3.NVIDIAIMEXMode(opts.nvidiaIMEXMode),
					HostProfile:        opts.nvidiaHostProfile,
					HostProfileVersion: opts.nvidiaProfileVer,
					HostProfileDigest:  opts.nvidiaProfileHash,
				},
			}
		}
	}

	if err := c.Create(ctx, mc); err != nil {
		return fmt.Errorf("creating MachineConfiguration: %w", err)
	}

	printStep(out, fmt.Sprintf("Created MachineConfiguration %s", name))

	if opts.kubernetesVersion != "" {
		printConfig(out, "kubernetes", opts.kubernetesVersion)
	}

	if opts.agentImage != "" {
		printConfig(out, "agent-image", opts.agentImage)
	}

	if opts.nvidiaIMEXMode != "" {
		printConfig(out, "nvidia-imex-mode", opts.nvidiaIMEXMode)
		printConfig(out, "nvidia-host-profile", opts.nvidiaHostProfile)
		printConfig(out, "nvidia-host-profile-version", opts.nvidiaProfileVer)
		printConfig(out, "nvidia-host-profile-digest", opts.nvidiaProfileHash)
	}

	if len(opts.nodeLabels) > 0 {
		printConfig(out, "node-labels", strings.Join(opts.nodeLabels, ", "))
	}

	if len(opts.taints) > 0 {
		printConfig(out, "taints", strings.Join(opts.taints, ", "))
	}

	printConfig(out, "update-strategy", opts.updateStrategy)
	fprintln(out)

	printStep(out, "MachineConfigurationVersion v1 will be created by the controller")
	fprintln(out)

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
