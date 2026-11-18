// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/controller"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/drain"
	"github.com/Azure/unbounded/internal/unbounded"
)

// schedulerInterval is how often the scheduler hands out update slots.
const schedulerInterval = 30 * time.Second

// Config is the controller's configuration file.
type Config struct {
	MetricsAddr          string `yaml:"metricsAddr"`
	ProbeAddr            string `yaml:"probeAddr"`
	EnableLeaderElection bool   `yaml:"enableLeaderElection"`
}

// LoadConfig reads the configuration file at path, over the defaults.
func LoadConfig(path string) (Config, error) {
	cfg := Config{MetricsAddr: ":8080", ProbeAddr: ":8081"}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config file: %w", err)
	}

	return cfg, nil
}

func controllerCommand() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the controller that updates the nodes",
		RunE: func(*cobra.Command, []string) error {
			cfg, err := LoadConfig(configPath)
			if err != nil {
				return err
			}

			return runController(ctrl.SetupSignalHandler(), cfg)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "Path to the configuration file (required)")

	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(fmt.Sprintf("mark flag required: %v", err))
	}

	return cmd
}

func runController(ctx context.Context, cfg Config) error {
	ctrl.SetLogger(zap.New())

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(unboundedv1alpha3.AddToScheme(scheme))

	restCfg := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress: cfg.ProbeAddr,
		LeaderElection:         cfg.EnableLeaderElection,
		LeaderElectionID:       "centerpivot-controller",
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create Kubernetes clientset: %w", err)
	}

	if err := mgr.GetFieldIndexer().IndexField(ctx, &unboundedv1alpha3.Machine{}, controller.MachineNodeIndex, controller.IndexMachineNode); err != nil {
		return fmt.Errorf("index Machines by Node: %w", err)
	}

	if err := (&controller.Reconciler{
		Client:    mgr.GetClient(),
		Reader:    mgr.GetAPIReader(),
		Namespace: unbounded.SystemNamespace(),
		Nebraska:  &controller.HTTPNebraska{},
		Drainer:   &drain.Drainer{Client: clientset},
		Clock:     clock.RealClock{},
		Recorder:  mgr.GetEventRecorder("centerpivot"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the node reconciler: %w", err)
	}

	if err := mgr.Add(&controller.Scheduler{Client: mgr.GetClient(), Clock: clock.RealClock{}, Interval: schedulerInterval}); err != nil {
		return fmt.Errorf("add the scheduler: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	ctrl.Log.Info("Starting manager")

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("start manager: %w", err)
	}

	return nil
}
