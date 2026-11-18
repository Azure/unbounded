// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// Scheduler hands out update slots (designs/centerpivot.md, section 4.2). It
// is the only writer of the Checking phase; the Reconciler takes the node from
// there. It runs only on the leader.
type Scheduler struct {
	Client   client.Client
	Clock    clock.PassiveClock
	Interval time.Duration
}

// Start runs a pass every Interval until ctx ends.
func (s *Scheduler) Start(ctx context.Context) error {
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	for {
		if err := s.Schedule(ctx); err != nil {
			log.FromContext(ctx).Error(err, "Scheduling updates failed")
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// candidate is a node due for a check.
type candidate struct {
	machine *unboundedv1alpha3.Machine
	state   State
	canary  bool
	name    string
}

// site is one Site's slots.
type site struct {
	settings    Settings
	managed     int
	unavailable int
	due         []candidate
}

// Schedule gives each Site's free slots to its nodes due for a check, unless a
// node is held.
func (s *Scheduler) Schedule(ctx context.Context) error {
	now := s.Clock.Now().UTC().Truncate(time.Second)

	sites, err := s.sites(ctx)
	if err != nil {
		return err
	}

	var nodes corev1.NodeList
	if err := s.Client.List(ctx, &nodes); err != nil {
		return fmt.Errorf("list Nodes: %w", err)
	}

	machines, err := s.machinesByNode(ctx)
	if err != nil {
		return err
	}

	busy, err := s.busyMachines(ctx)
	if err != nil {
		return err
	}

	var held []string

	for nodeName, machine := range machines {
		if StateOf(machine).Phase == PhaseHeld {
			held = append(held, nodeName)
		}
	}

	nodesGauge.Reset()
	updatingGauge.Reset()

	for i := range nodes.Items {
		node := &nodes.Items[i]

		machine := machines[node.Name]
		if machine == nil {
			continue
		}

		state := StateOf(machine)
		siteName := siteOf(node, machine)

		slots := sites[siteName]
		if slots == nil || (node.Labels[ManagedLabel] != "true" && !state.Phase.Updating()) {
			continue
		}

		slots.managed++

		if info, err := agentInfoOf(node); err == nil {
			app, _ := slots.settings.AppFor(node.Labels) //nolint:errcheck // A node without an app is counted under none.
			nodesGauge.WithLabelValues(siteName, app.Name, info.CurrentVersion).Inc()
		}

		if state.Phase.Updating() {
			updatingGauge.WithLabelValues(siteName, string(state.Phase)).Inc()
		}

		// Each node unavailable for any reason uses one slot.
		if state.Phase.Updating() || state.Tainted() || !ready(node) || node.Spec.Unschedulable || busy[machine.Name] {
			slots.unavailable++

			continue
		}

		if state.Phase == PhaseIdle && node.Labels[PausedLabel] != "true" && due(node, state, slots.settings, now) {
			slots.due = append(slots.due, candidate{machine: machine, state: state, canary: node.Labels[CanaryLabel] == "true", name: node.Name})
		}
	}

	heldGauge.Set(float64(len(held)))

	if len(held) > 0 {
		slices.Sort(held)
		blockedGauge.WithLabelValues("held").Set(1)
		log.FromContext(ctx).Info("No updates start while a node is held", "held", held)

		return nil
	}

	blockedGauge.WithLabelValues("held").Set(0)

	for _, slots := range sites {
		free := slots.settings.Slots(slots.managed) - slots.unavailable
		if free <= 0 || len(slots.due) == 0 {
			continue
		}

		slices.SortFunc(slots.due, func(a, b candidate) int {
			if a.canary != b.canary {
				if a.canary {
					return -1
				}

				return 1
			}

			return cmp.Or(a.state.LastCheck.Compare(b.state.LastCheck), cmp.Compare(a.name, b.name))
		})

		for _, c := range slots.due[:min(free, len(slots.due))] {
			state := c.state
			state.Phase = PhaseChecking
			state.Since = now

			err := writeState(ctx, s.Client, c.machine, state)
			if apierrors.IsConflict(err) {
				continue
			}

			if err != nil {
				return err
			}

			log.FromContext(ctx).Info("Gave an update slot", "node", c.name)
		}
	}

	return nil
}

// sites returns the slots of every Site that enables centerpivot.
func (s *Scheduler) sites(ctx context.Context) (map[string]*site, error) {
	var list unboundedv1alpha3.SiteList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list Sites: %w", err)
	}

	sites := map[string]*site{}

	for i := range list.Items {
		if settings, ok := SettingsFor(&list.Items[i]); ok {
			sites[list.Items[i].Name] = &site{settings: settings}
		}
	}

	return sites, nil
}

func (s *Scheduler) machinesByNode(ctx context.Context) (map[string]*unboundedv1alpha3.Machine, error) {
	var list unboundedv1alpha3.MachineList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list Machines: %w", err)
	}

	machines := make(map[string]*unboundedv1alpha3.Machine, len(list.Items))
	for i := range list.Items {
		machines[list.Items[i].NodeName()] = &list.Items[i]
	}

	return machines, nil
}

// busyMachines returns the Machines with an unfinished MachineOperation: other
// maintenance may restart or replace them.
func (s *Scheduler) busyMachines(ctx context.Context) (map[string]bool, error) {
	var list unboundedv1alpha3.MachineOperationList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list MachineOperations: %w", err)
	}

	busy := map[string]bool{}

	for _, operation := range list.Items {
		if operation.Spec.MachineRef != "" && !operation.Status.IsTerminal() {
			busy[operation.Spec.MachineRef] = true
		}
	}

	return busy, nil
}

// due reports whether the node is due for a check: checked longer ago than the
// check interval, with a version to check from.
func due(node *corev1.Node, state State, settings Settings, now time.Time) bool {
	if !state.LastCheck.IsZero() && now.Sub(state.LastCheck) < settings.CheckInterval {
		return false
	}

	_, err := agentInfoOf(node)

	return err == nil
}
