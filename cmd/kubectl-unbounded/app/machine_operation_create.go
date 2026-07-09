// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type machineOperationCreateOptions struct {
	name          string
	kind          v1alpha3.OperationKind
	machine       string
	selector      string
	parameterArgs []string
	parameters    map[string]string
	ttlSeconds    int32
	ttlSet        bool

	wait    bool
	timeout time.Duration

	output        string
	dryRun        string
	fieldManager  string
	kubeconfig    string
	clientFactory machineClientFactory

	out          io.Writer
	printCreated bool

	ownerReferenceMachine bool
}

func machineOperationCreateCommand() *cobra.Command {
	return newMachineOperationCreateCommand(newMachineCommandRuntime())
}

func newMachineOperationCreateCommand(rt *machineCommandRuntime) *cobra.Command {
	o := &machineOperationCreateOptions{
		ttlSeconds:    -1,
		output:        operationOutputName,
		dryRun:        dryRunNone,
		fieldManager:  fieldManagerID,
		clientFactory: rt.clientWithKubeconfig,
		printCreated:  true,
	}

	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a MachineOperation resource",
		Long: `Create a MachineOperation resource that targets one Machine or a
Machine selector. Use --wait to stream status until the operation reaches a
terminal phase.`,
		Example: `  kubectl unbounded machine operation create reboot-worker-01 \
    --kind HostReboot --machine worker-01

  kubectl unbounded machine operation create reboot-gpu \
    --kind NodeReboot --selector role=gpu --wait

  kubectl unbounded machine operation create upgrade-worker-01 \
    --kind AgentUpgrade --machine worker-01 \
    --param downloadURL=https://example.com/unbounded-agent-linux-amd64.tar.gz`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.name = args[0]
			o.out = cmd.OutOrStdout()
			o.ttlSet = cmd.Flags().Changed("ttl")

			ctx := rt.context(cmd.Context())
			return o.run(ctx)
		},
	}

	addMachineOperationCreateFlags(cmd, o)

	return cmd
}

func addMachineOperationCreateFlags(cmd *cobra.Command, o *machineOperationCreateOptions) {
	cmd.Flags().StringVar((*string)(&o.kind), "kind", "", "Operation kind: NodeReboot, AgentUpgrade, AgentReset, HostReboot, HostPowerOff, HostPowerOn, or HostReplace")
	cmd.Flags().StringVar(&o.machine, "machine", "", "Target Machine name")
	cmd.Flags().StringVarP(&o.selector, "selector", "l", "", "Machine label selector")
	cmd.Flags().StringArrayVar(&o.parameterArgs, "param", nil, "Operation parameter as key=value (repeatable)")
	cmd.Flags().Int32Var(&o.ttlSeconds, "ttl", -1, "Seconds after completion before cleanup (0 keeps indefinitely, default keeps indefinitely)")
	cmd.Flags().Lookup("ttl").DefValue = "unset"
	cmd.Flags().BoolVar(&o.wait, "wait", false, "Wait until the operation reaches Complete or Failed")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 0, "Time to wait for completion when --wait is set (0 waits indefinitely)")
	cmd.Flags().StringVarP(&o.output, "output", "o", operationOutputName, "Output format. One of: name|yaml|json")
	cmd.Flags().StringVar(&o.dryRun, "dry-run", dryRunNone, "Must be \"none\", \"client\", or \"server\"")
	cmd.Flags().StringVar(&o.fieldManager, "field-manager", fieldManagerID, "Name associated with managed fields for create requests")
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig file")

	_ = cmd.RegisterFlagCompletionFunc("kind", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return []string{
			string(v1alpha3.OperationNodeReboot),
			string(v1alpha3.OperationAgentUpgrade),
			string(v1alpha3.OperationAgentReset),
			string(v1alpha3.OperationHostReboot),
			string(v1alpha3.OperationHostPowerOff),
			string(v1alpha3.OperationHostPowerOn),
			string(v1alpha3.OperationHostReplace),
		}, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return []string{operationOutputName, operationOutputYAML, operationOutputJSON}, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("dry-run", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck
		return []string{dryRunNone, dryRunClient, dryRunServer}, cobra.ShellCompDirectiveNoFileComp
	})
}

func (o *machineOperationCreateOptions) run(ctx context.Context) error {
	if o.out == nil {
		o.out = os.Stdout
	}

	if err := o.validate(); err != nil {
		return err
	}

	op, err := o.build()
	if err != nil {
		return err
	}

	if o.dryRun == dryRunClient {
		return printMachineOperation(o.out, op, o.output)
	}

	clientFactory := o.clientFactory
	if clientFactory == nil {
		clientFactory = newMachineClientWithKubeconfig
	}

	c, err := clientFactory(o.kubeconfig)
	if err != nil {
		return err
	}

	return o.runWithClient(ctx, c, op)
}

func (o *machineOperationCreateOptions) runWithClient(ctx context.Context, c client.WithWatch, op *v1alpha3.MachineOperation) error {
	if o.ownerReferenceMachine {
		if err := addMachineOperationOwnerReference(ctx, c, op, o.machine); err != nil {
			return err
		}
	}

	createOpts := []client.CreateOption{&client.CreateOptions{FieldManager: o.fieldManager}}
	if o.dryRun == dryRunServer {
		createOpts = append(createOpts, client.DryRunAll)
	}

	if err := c.Create(ctx, op, createOpts...); err != nil {
		return fmt.Errorf("creating MachineOperation: %w", err)
	}

	ensureMachineOperationTypeMeta(op)

	if o.printCreated {
		if err := printMachineOperation(o.out, op, o.output); err != nil {
			return err
		}
	}

	if !o.wait {
		return nil
	}

	waitCtx, cancel := contextWithOptionalTimeout(ctx, o.timeout)
	defer cancel()

	return waitForMachineOperation(waitCtx, c, op.Name)
}

func (o *machineOperationCreateOptions) validate() error {
	if errs := validation.IsDNS1123Subdomain(o.name); len(errs) > 0 {
		return fmt.Errorf("invalid operation name %q: %s", o.name, strings.Join(errs, "; "))
	}

	if !isSupportedOperationKind(o.kind) {
		return fmt.Errorf("unsupported operation kind %q", o.kind)
	}

	if (o.machine == "") == (o.selector == "") {
		return fmt.Errorf("exactly one of --machine or --selector is required")
	}

	if o.ttlSeconds < 0 && (o.ttlSeconds != -1 || o.ttlSet) {
		return fmt.Errorf("--ttl must be 0 or greater")
	}

	switch o.output {
	case "", operationOutputName, operationOutputYAML, operationOutputJSON:
	default:
		return fmt.Errorf("unsupported output %q, expected one of: name|yaml|json", o.output)
	}

	switch o.dryRun {
	case "", dryRunNone, dryRunClient, dryRunServer:
	default:
		return fmt.Errorf("unsupported dry-run mode %q, expected one of: none|client|server", o.dryRun)
	}

	if o.wait && o.dryRun != "" && o.dryRun != dryRunNone {
		return fmt.Errorf("--wait cannot be used with --dry-run")
	}

	if o.wait && (o.output == operationOutputYAML || o.output == operationOutputJSON) {
		return fmt.Errorf("--wait cannot be used with -o %s because progress output is not machine-readable", o.output)
	}

	parameters, err := o.operationParameters()
	if err != nil {
		return err
	}

	if o.kind == v1alpha3.OperationAgentUpgrade && parameters["downloadURL"] == "" {
		return fmt.Errorf("AgentUpgrade requires --param downloadURL=<url>")
	}

	return nil
}

func (o *machineOperationCreateOptions) build() (*v1alpha3.MachineOperation, error) {
	parameters, err := o.operationParameters()
	if err != nil {
		return nil, err
	}

	op := &v1alpha3.MachineOperation{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha3.GroupVersion.String(),
			Kind:       "MachineOperation",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: o.name,
		},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    o.machine,
			OperationKind: o.kind,
			Parameters:    parameters,
		},
	}

	if len(parameters) == 0 {
		op.Spec.Parameters = nil
	}

	if o.selector != "" {
		selector, err := metav1.ParseToLabelSelector(o.selector)
		if err != nil {
			return nil, fmt.Errorf("invalid selector %q: %w", o.selector, err)
		}

		op.Spec.MachineSelector = selector
	}

	if o.ttlSeconds > 0 {
		ttl := o.ttlSeconds
		op.Spec.TTLSecondsAfterFinished = &ttl
	}

	return op, nil
}

func (o *machineOperationCreateOptions) operationParameters() (map[string]string, error) {
	parameters := map[string]string{}
	for key, value := range o.parameters {
		parameters[key] = value
	}

	for _, parameter := range o.parameterArgs {
		key, value, ok := strings.Cut(parameter, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid --param value %q, expected key=value", parameter)
		}

		parameters[key] = value
	}

	return parameters, nil
}
