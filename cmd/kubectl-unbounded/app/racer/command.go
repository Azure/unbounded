// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer implements `kubectl unbounded racer`.
package racer

import (
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/unbounded/internal/unbounded"
)

// env carries clients, streams, and timing so commands are testable.
type env struct {
	configFlags *genericclioptions.ConfigFlags

	// client and namespace override configFlags when set (tests).
	client    kubernetes.Interface
	namespace string

	in         io.Reader
	out        io.Writer
	errOut     io.Writer
	isTerminal func() bool

	pollInterval time.Duration
}

func newEnv() *env {
	return &env{
		configFlags:  genericclioptions.NewConfigFlags(true),
		in:           os.Stdin,
		out:          os.Stdout,
		errOut:       os.Stderr,
		isTerminal:   func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
		pollInterval: 2 * time.Second,
	}
}

func (e *env) kubeClient() (kubernetes.Interface, error) {
	if e.client != nil {
		return e.client, nil
	}

	cfg, err := e.configFlags.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	e.client = c

	return c, nil
}

// ns returns --namespace when given, else the Unbounded system namespace.
func (e *env) ns() (string, error) {
	if e.namespace != "" {
		return e.namespace, nil
	}

	ns, overridden, err := e.configFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return "", err
	}

	if overridden {
		return ns, nil
	}

	return unbounded.SystemNamespace(), nil
}

// Command returns the `racer` command group.
func Command() *cobra.Command {
	return newCommand(newEnv())
}

func newCommand(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "racer",
		Short: "Manage the Racer cache",
	}
	if e.configFlags != nil {
		e.configFlags.AddFlags(cmd.PersistentFlags())
	}

	cmd.AddCommand(disksCommand(e))

	return cmd
}

func disksCommand(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disks",
		Short: "List, approve, and revoke raw block devices Racer may use",
		Long: `Racer only uses a raw block device after an administrator approves it in the
Node annotation ` + "`racer.unbounded-cloud.io/disks`" + `. Approving a disk lets Racer erase
its first and last 1 MiB. Any change restarts the Racer pod on that Node,
which drops its cache.`,
	}
	cmd.AddCommand(listCommand(e), allowCommand(e), revokeCommand(e))

	return cmd
}
