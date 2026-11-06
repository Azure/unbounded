// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	osSignal "os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
	"github.com/Azure/unbounded/internal/signaladapter"
	"github.com/Azure/unbounded/internal/version"
)

const adapterName = "project-signal-adapter"

type config struct {
	signaladapter.Request
	PhaseIndex int
	StageIndex int
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
	flags.StringVar(&cfg.RunName, "run-name", "", "Immutable CertificationRun name")
	flags.StringVar(&cfg.ExecutionName, "execution-name", "", "Check-specific backend execution name")
	flags.Var(uidValue{target: &cfg.RunUID}, "run-uid", "CertificationRun UID")
	flags.IntVar(&cfg.CheckIndex, "check-index", -1, "Zero-based check position in the CertificationRun suite")
	flags.IntVar(&cfg.PhaseIndex, "phase-index", -1, "Zero-based phase position in the resolved CertificationRun plan")
	flags.IntVar(&cfg.StageIndex, "stage-index", -1, "Zero-based stage position in the resolved CertificationRun phase")
	flags.StringVar(&cfg.ResultConfigMap, "result-configmap", "", "Immutable check result ConfigMap name")

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
	}

	if cfg.PhaseIndex >= 0 || cfg.StageIndex >= 0 {
		if cfg.CheckIndex >= 0 || cfg.PhaseIndex < 0 || cfg.StageIndex < 0 {
			return errors.New("stage mode requires phase-index and stage-index and excludes check-index")
		}

		request := signaladapter.StageRequest{
			Namespace:       cfg.Namespace,
			WorkflowName:    cfg.WorkflowName,
			RunName:         cfg.RunName,
			ExecutionName:   cfg.ExecutionName,
			RunUID:          cfg.RunUID,
			PhaseIndex:      cfg.PhaseIndex,
			StageIndex:      cfg.StageIndex,
			ResultConfigMap: cfg.ResultConfigMap,
		}

		execution, resolveErr := runner.ResolveStageRequest(ctx, request)
		if resolveErr != nil {
			return resolveErr
		}

		executor := &signaladapter.KubernetesStageExecutor{
			Runner:        runner,
			Namespace:     cfg.Namespace,
			WorkflowName:  cfg.WorkflowName,
			ExecutionName: cfg.ExecutionName,
		}
		stageRunner := signaladapter.StageRunner{
			Executor: executor,
			Evidence: func(
				evidenceCtx context.Context,
				evidenceExecution signaladapter.StageExecutionRequest,
				envelopes []signalv1.InvocationResultEnvelope,
			) error {
				return runner.PersistInvocationEvidence(
					evidenceCtx,
					request,
					evidenceExecution,
					envelopes,
				)
			},
		}

		summary, executeErr := stageRunner.Execute(ctx, execution)
		if executeErr != nil {
			return executeErr
		}

		return runner.PersistStageResult(ctx, request, execution, summary)
	}

	resolved, check, err := runner.ResolveRequest(ctx, cfg.Request)
	if err != nil {
		return err
	}

	output, err := runner.Execute(ctx, resolved)
	if err != nil {
		return err
	}

	return runner.PersistResult(ctx, cfg.Request, check, output)
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
