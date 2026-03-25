package commands

import (
	"context"
	"fmt"

	v1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RebootCmd returns a cobra.Command that reboots a Machine via Redfish.
func RebootCmd() *cobra.Command {
	var (
		reimage   bool
		namespace string
	)

	cmd := &cobra.Command{
		Use:   "reboot NAME",
		Short: "Reboot a Machine via Redfish",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			if namespace == "" {
				var err error

				namespace, _, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
					clientcmd.NewDefaultClientConfigLoadingRules(),
					&clientcmd.ConfigOverrides{},
				).Namespace()
				if err != nil {
					return fmt.Errorf("resolving namespace: %w", err)
				}
			}

			c, err := client.NewWithWatch(ctrl.GetConfigOrDie(), client.Options{Scheme: BuildScheme()})
			if err != nil {
				return fmt.Errorf("creating client: %w", err)
			}

			return runReboot(ctx, c, namespace, args[0], reimage)
		},
	}
	cmd.Flags().BoolVar(&reimage, "reimage", false, "Increment reimage counter before rebooting")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Namespace (defaults to kubeconfig current context)")

	return cmd
}

func runReboot(ctx context.Context, c client.WithWatch, namespace, name string, reimage bool) error {
	key := client.ObjectKey{Namespace: namespace, Name: name}

	var node v1alpha3.Machine
	if err := c.Get(ctx, key, &node); err != nil {
		return fmt.Errorf("getting Machine: %w", err)
	}

	if node.Spec.PXE == nil || node.Spec.PXE.Redfish == nil {
		return fmt.Errorf("machine %s has no redfish configuration; reboots require BMC access", name)
	}

	if node.Spec.Operations == nil {
		node.Spec.Operations = &v1alpha3.OperationsSpec{}
	}

	if reimage {
		node.Spec.Operations.ReimageCounter++
	}

	node.Spec.Operations.RebootCounter++
	target := node.Spec.Operations.RebootCounter

	if err := c.Update(ctx, &node); err != nil {
		return fmt.Errorf("updating Machine: %w", err)
	}

	PrintStep(fmt.Sprintf("Rebooting Machine %s/%s...", namespace, name))
	PrintConfig("target", fmt.Sprintf("%d", target))
	PrintConfig("reimage", fmt.Sprintf("%d", node.Spec.Operations.ReimageCounter))
	fmt.Println()

	watcher, err := c.Watch(ctx, &v1alpha3.MachineList{},
		client.InNamespace(namespace),
		client.MatchingFields{"metadata.name": name})
	if err != nil {
		return fmt.Errorf("watching Machine: %w", err)
	}
	defer watcher.Stop()

	var lastReason string

	for ev := range watcher.ResultChan() {
		if ev.Type == watch.Error {
			return fmt.Errorf("watch error: %v", ev.Object)
		}

		if ev.Type == watch.Deleted {
			return fmt.Errorf("machine %s was deleted", name)
		}

		m, ok := ev.Object.(*v1alpha3.Machine)
		if !ok {
			continue
		}

		cond := meta.FindStatusCondition(m.Status.Conditions, v1alpha3.MachineConditionProvisioned)

		reason := ""
		if cond != nil {
			reason = cond.Reason
		}

		if reason != lastReason {
			switch reason {
			case "PoweringOff":
				PrintStep("Powering off...")
			case "ForceOff":
				PrintStep("Powered off")
			case "PoweringOn":
				PrintStep("Powering on...")
			case "":
				if lastReason != "" {
					PrintStep("Powered on")
				}
			}

			lastReason = reason
		}

		if m.Status.Operations != nil && m.Status.Operations.RebootCounter >= target {
			PrintReady()
			return nil
		}
	}

	// Watch channel closed unexpectedly; do a final check.
	if err := c.Get(ctx, key, &node); err != nil {
		return fmt.Errorf("final check: %w", err)
	}

	if node.Status.Operations != nil && node.Status.Operations.RebootCounter >= target {
		PrintReady()
		return nil
	}

	var observed int64
	if node.Status.Operations != nil {
		observed = node.Status.Operations.RebootCounter
	}

	return fmt.Errorf("watch closed before reboot completed (observedReboots=%d, target=%d)", observed, target)
}
