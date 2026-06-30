// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// ANSI color/style codes for terminal output.
const (
	bold  = "\033[1m"
	dim   = "\033[2m"
	reset = "\033[0m"
	green = "\033[32m"
	cyan  = "\033[36m"
)

func buildScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(v1alpha3.AddToScheme(s))

	return s
}

func printStep(msg string) {
	fmt.Printf("  %s-->%s %s\n", cyan, reset, msg)
}

func printConfig(key, value string) {
	fmt.Printf("  %s%-18s%s %s\n", dim, key, reset, value)
}

func printReady() {
	fmt.Printf("\n  %s%sready%s\n\n", green, bold, reset)
}

type conditionState struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

func conditionChanged(cond metav1.Condition, seen map[string]conditionState) bool {
	state := conditionState{
		Status:  cond.Status,
		Reason:  cond.Reason,
		Message: cond.Message,
	}

	last, ok := seen[cond.Type]
	if ok && last == state {
		return false
	}

	seen[cond.Type] = state

	return true
}

func reportConditionTransitions(conditions []metav1.Condition, seen map[string]conditionState) {
	ordered := append([]metav1.Condition(nil), conditions...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Type < ordered[j].Type
	})

	for _, cond := range ordered {
		if !conditionChanged(cond, seen) {
			continue
		}

		printStep(fmt.Sprintf("Condition %s: %s", cond.Type, formatConditionState(cond)))
	}
}

func formatConditionState(cond metav1.Condition) string {
	parts := []string{string(cond.Status)}
	if cond.Reason != "" {
		parts = append(parts, cond.Reason)
	}

	state := strings.Join(parts, "/")
	if cond.Message == "" {
		return state
	}

	return fmt.Sprintf("%s - %s", state, cond.Message)
}

// newMachineClient creates a controller-runtime client configured for Machine resources.
func newMachineClient() (client.WithWatch, error) {
	c, err := client.NewWithWatch(ctrl.GetConfigOrDie(), client.Options{Scheme: buildScheme()})
	if err != nil {
		return nil, fmt.Errorf("creating client: %w", err)
	}

	return c, nil
}

// getMachine fetches a Machine by name.
func getMachine(ctx context.Context, c client.WithWatch, name string) (*v1alpha3.Machine, error) {
	key := client.ObjectKey{Name: name}

	var machine v1alpha3.Machine
	if err := c.Get(ctx, key, &machine); err != nil {
		return nil, fmt.Errorf("getting Machine: %w", err)
	}

	if machine.Spec.Operations == nil {
		machine.Spec.Operations = &v1alpha3.OperationsSpec{}
	}

	return &machine, nil
}

// watchReboot watches a Machine for reboot completion by tracking the RebootCounter
// and power state transitions.
func watchReboot(ctx context.Context, c client.WithWatch, name string, target int64) error {
	watcher, err := c.Watch(ctx, &v1alpha3.MachineList{},
		client.MatchingFields{"metadata.name": name})
	if err != nil {
		return fmt.Errorf("watching Machine: %w", err)
	}
	defer watcher.Stop()

	var lastReason string
	seenConditions := map[string]conditionState{}

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

		reportConditionTransitions(m.Status.Conditions, seenConditions)

		cond := meta.FindStatusCondition(m.Status.Conditions, "PoweredOff")

		reason := ""
		if cond != nil {
			reason = cond.Reason
		}

		if reason != lastReason {
			switch reason {
			case "PoweringOff":
				printStep("Powering off...")
			case "ForceOff":
				printStep("Powered off")
			case "PoweringOn":
				printStep("Powering on...")
			case "":
				if lastReason != "" {
					printStep("Powered on")
				}
			}

			lastReason = reason
		}

		if m.Status.Operations != nil && m.Status.Operations.RebootCounter >= target {
			printReady()
			return nil
		}
	}

	// Watch channel closed unexpectedly; do a final check.
	key := client.ObjectKey{Name: name}

	var node v1alpha3.Machine
	if err := c.Get(ctx, key, &node); err != nil {
		return fmt.Errorf("final check: %w", err)
	}

	if node.Status.Operations != nil && node.Status.Operations.RebootCounter >= target {
		printReady()
		return nil
	}

	var observed int64
	if node.Status.Operations != nil {
		observed = node.Status.Operations.RebootCounter
	}

	return fmt.Errorf("watch closed before reboot completed (observedReboots=%d, target=%d)", observed, target)
}
