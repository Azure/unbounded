// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Azure/unbounded/cmd/centerpivot/internal/names"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/nodeinfo"
)

func nodeInfoCommand() *cobra.Command {
	publisher := &nodeinfo.Publisher{Annotation: names.AgentInfoAnnotation}

	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "node-info",
		Short: "Publish this node's Nebraska identity in its agent-info annotation",
		RunE: func(*cobra.Command, []string) error {
			if publisher.NodeName == "" {
				return errors.New("--node-name, or the NODE_NAME environment variable, is required")
			}

			ctrl.SetLogger(zap.New())

			publisher.Log = ctrl.Log.WithName("node-info")

			client, err := kubernetes.NewForConfig(ctrl.GetConfigOrDie())
			if err != nil {
				return fmt.Errorf("create Kubernetes clientset: %w", err)
			}

			publisher.Client = client

			return publisher.Run(ctrl.SetupSignalHandler(), interval)
		},
	}

	cmd.Flags().StringVar(&publisher.NodeName, "node-name", os.Getenv("NODE_NAME"), "The node's name")
	cmd.Flags().StringVar(&publisher.MachineIDPath, "machine-id-path", names.HostMachineIDPath,
		"The host's /etc/machine-id, as the node sees it")
	cmd.Flags().StringVar(&publisher.OSReleasePath, "os-release-path", names.HostOSReleasePath,
		"The host's os-release, as the node sees it")
	cmd.Flags().StringVar(&publisher.VersionKey, "version-key", names.VersionKey,
		"The os-release key trident-acl-agent reads its version from")
	cmd.Flags().DurationVar(&interval, "interval", time.Minute, "How often to check the host's files")

	return cmd
}
