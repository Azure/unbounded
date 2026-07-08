// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	operationOutputName = "name"
	operationOutputYAML = "yaml"
	operationOutputJSON = "json"

	dryRunNone   = "none"
	dryRunClient = "client"
	dryRunServer = "server"
)

func newMachineOperationCommandGroup(rt *machineCommandRuntime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "operation",
		Aliases: []string{"operations"},
		Short:   "Create and watch MachineOperation resources",
	}

	cmd.AddCommand(
		newMachineOperationCreateCommand(rt),
		newMachineOperationWaitCommand(rt),
	)

	return cmd
}

func addMachineOperationOwnerReference(ctx context.Context, c client.WithWatch, op *v1alpha3.MachineOperation, machineName string) error {
	machine, err := getMachine(ctx, c, machineName)
	if err != nil {
		return err
	}

	op.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: v1alpha3.GroupVersion.String(),
			Kind:       "Machine",
			Name:       machine.Name,
			UID:        machine.UID,
		},
	}

	return nil
}

func printMachineOperation(w io.Writer, op *v1alpha3.MachineOperation, output string) error {
	ensureMachineOperationTypeMeta(op)

	switch output {
	case "", operationOutputName:
		_, err := fmt.Fprintf(w, "machineoperations/%s created\n", op.Name)
		return err
	case operationOutputYAML:
		p := printers.YAMLPrinter{}
		return p.PrintObj(op, w)
	case operationOutputJSON:
		p := printers.JSONPrinter{}
		return p.PrintObj(op, w)
	default:
		return fmt.Errorf("unsupported output format %q", output)
	}
}

func ensureMachineOperationTypeMeta(op *v1alpha3.MachineOperation) {
	if op.APIVersion == "" {
		op.APIVersion = v1alpha3.GroupVersion.String()
	}

	if op.Kind == "" {
		op.Kind = "MachineOperation"
	}
}

func contextWithOptionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, timeout)
}

func isSupportedOperationKind(kind v1alpha3.OperationKind) bool {
	switch kind {
	case v1alpha3.OperationNodeReboot,
		v1alpha3.OperationAgentUpgrade,
		v1alpha3.OperationAgentReset,
		v1alpha3.OperationHostReboot,
		v1alpha3.OperationHostPowerOff,
		v1alpha3.OperationHostPowerOn,
		v1alpha3.OperationHostReplace:
		return true
	default:
		return false
	}
}

func generateMachineOperationName(machineName, operationNamePart string, now time.Time) string {
	suffix := fmt.Sprintf("-%s-%s", operationNamePart, now.UTC().Format("20060102-150405"))
	maxPrefixLen := validation.DNS1123SubdomainMaxLength - len(suffix)
	prefix := strings.Trim(machineName, "-")

	if len(prefix) > maxPrefixLen {
		prefix = strings.TrimRight(prefix[:maxPrefixLen], "-")
	}

	if prefix == "" {
		prefix = "machine"
	}

	return prefix + suffix
}

func newMachineClientWithKubeconfig(kubeconfig string) (client.WithWatch, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	c, err := client.NewWithWatch(config, client.Options{Scheme: buildScheme()})
	if err != nil {
		return nil, fmt.Errorf("creating client: %w", err)
	}

	return c, nil
}
