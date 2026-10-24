// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osSignal "os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/internal/signaladapter"
	"github.com/Azure/unbounded/internal/version"
)

const adapterName = "project-signal-adapter"

type config struct {
	signaladapter.Config
	outputDirectory string
}

func main() {
	var cfg config

	command := &cobra.Command{
		Use:     adapterName,
		Short:   "Run participant-aware Project Signal backend workloads",
		Version: version.Version + " (commit: " + version.GitCommit + ")",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := osSignal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return run(ctx, cfg)
		},
	}

	flags := command.Flags()
	flags.StringVar(&cfg.Namespace, "namespace", "", "Namespace for backend resources")
	flags.StringVar(&cfg.WorkflowName, "workflow-name", "", "Owning Argo Workflow name")
	flags.StringVar(&cfg.RunName, "run-name", "", "CertificationRun name")
	flags.Var(uidValue{target: &cfg.RunUID}, "run-uid", "CertificationRun UID")
	flags.StringVar(&cfg.ParticipantsJSON, "participants-json", "", "Ordered participant snapshot JSON")
	flags.IntVar(&cfg.ParticipantCount, "participant-count", 0, "Declared participant count")
	flags.StringVar(&cfg.ScriptConfigMap, "script-configmap", "", "Immutable script ConfigMap")
	flags.StringVar(&cfg.ScriptKey, "script-key", "", "Script ConfigMap key")
	flags.StringVar(
		&cfg.ScriptDigest,
		"script-digest",
		"",
		"algorithm-qualified SHA-256 digest of the script bytes (sha256:<64 lowercase hex>)",
	)
	flags.StringVar(&cfg.RunnerImage, "runner-image", "", "Digest-pinned runner image")
	flags.StringVar(&cfg.BackendKind, "backend-kind", "", "Backend kind")
	flags.Int64Var(
		&cfg.BackendDeadlineSeconds,
		"backend-active-deadline-seconds",
		0,
		"Active deadline for participant backend workloads",
	)
	flags.StringVar(&cfg.ScriptEnvironmentJSON, "script-environment", "{}", "JSON-encoded script environment")
	flags.StringVar(&cfg.WorkloadPolicyJSON, "workload-policy", "{}", "JSON-encoded participant workload policy")
	flags.StringVar(&cfg.GPUResourceCount, "gpu-resource-count", "0", "GPU resource limit per participant")
	flags.StringVar(&cfg.outputDirectory, "output-directory", "/outputs", "Argo output directory")

	command.CompletionOptions.DisableDefaultCmd = true

	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config) error {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster Kubernetes configuration: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create dynamic Kubernetes client: %w", err)
	}

	kubernetesClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	runner := &signaladapter.Runner{
		Dynamic:    dynamicClient,
		Kubernetes: kubernetesClient,
		PersistBackendEvidence: func(content string) error {
			if err := signaladapter.ValidateBackendEvidence(content); err != nil {
				return err
			}

			if err := os.MkdirAll(cfg.outputDirectory, 0o750); err != nil {
				return fmt.Errorf("create output directory: %w", err)
			}

			return writeOutput(cfg.outputDirectory, "backend-evidence", content)
		},
	}

	output, err := runner.Execute(ctx, cfg.Config)
	if err != nil {
		return err
	}

	return writeOutputs(cfg.outputDirectory, output)
}

func writeOutputs(directory string, output signaladapter.AggregateOutput) error {
	if directory == "" {
		return errors.New("output-directory is required")
	}

	if err := signaladapter.ValidateBackendEvidence(output.BackendEvidence); err != nil {
		return err
	}

	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	outputs := map[string]string{
		"verdict":           output.Verdict,
		"reason":            output.Reason,
		"measurements":      output.Measurements,
		"backend-resources": output.BackendJSON,
		"backend-evidence":  output.BackendEvidence,
	}
	for name, value := range outputs {
		if err := writeOutput(directory, name, value); err != nil {
			return err
		}
	}

	evidence, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("encode adapter result: %w", err)
	}

	fmt.Println(string(evidence))

	return nil
}

func writeOutput(directory, name, value string) error {
	if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600); err != nil {
		return fmt.Errorf("write %s output: %w", name, err)
	}

	return nil
}

type uidValue struct {
	target *types.UID
}

func (v uidValue) String() string {
	if v.target == nil {
		return ""
	}

	return string(*v.target)
}

func (v uidValue) Set(value string) error {
	if v.target == nil {
		return errors.New("UID target is nil")
	}

	*v.target = types.UID(value)

	return nil
}

func (uidValue) Type() string {
	return "uid"
}
