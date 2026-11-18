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
	"github.com/Azure/unbounded/cmd/centerpivot/internal/names"
)

// Scheduler hands out update slots (designs/centerpivot.md, section 4.2). It
// is the only writer of the Checking phase; the Reconciler takes the node from
// there. It runs only on the leader.
type Scheduler struct {
	Client   client.Client
	Clock    clock.PassiveClock
	Interval time.Duration
	// Wake runs a pass before the next Interval, when a slot frees.
	Wake Wakeup
}

// Wakeup asks the scheduler for a pass. Asks made while one is pending are
// merged.
type Wakeup chan struct{}

// NewWakeup returns a Wakeup for one scheduler.
func NewWakeup() Wakeup { return make(Wakeup, 1) }

// Signal asks for a pass, without waiting. A nil Wakeup does nothing.
func (w Wakeup) Signal() {
	select {
	case w <- struct{}{}:
	default:
	}
}

// Start runs a pass every Interval, and when woken, until ctx ends.
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
		case <-s.Wake:
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
	settings Settings
	// invalid says why the Site's settings give no slots.
	invalid     error
	managed     int
	unavailable int
	updating    int
	due         []candidate
	// heldBack is set when a due node waits for a hold on its app.
	heldBack bool
}

// Schedule gives each Site's free slots to its nodes due for a check. A hold
// after a node restarted into a new version stops that app in every Site.
func (s *Scheduler) Schedule(ctx context.Context) error {
	now := s.Clock.Now().UTC().Truncate(time.Second)

	sites, err := s.sites(ctx)
	if err != nil {
		return err
	}

	nodes, err := s.nodesByName(ctx)
	if err != nil {
		return err
	}

	var machines unboundedv1alpha3.MachineList
	if err := s.Client.List(ctx, &machines); err != nil {
		return fmt.Errorf("list Machines: %w", err)
	}

	busy, err := s.busyMachines(ctx)
	if err != nil {
		return err
	}

	held := 0
	heldApps := map[string]string{}

	for i := range machines.Items {
		machine := &machines.Items[i]

		state := StateOf(machine)
		if state.Phase != PhaseHeld || sites[siteOf(nodes[machine.NodeName()], machine)] == nil {
			continue
		}

		held++

		if state.HeldApp != "" {
			heldApps[state.HeldApp] = machine.Name
		}
	}

	heldGauge.Set(float64(held))
	nodesGauge.Reset()
	updatingGauge.Reset()
	blockedGauge.Reset()

	for i := range machines.Items {
		machine := &machines.Items[i]
		// node is nil while the Machine's Node doesn't exist.
		node := nodes[machine.NodeName()]
		state := StateOf(machine)
		siteName := siteOf(node, machine)

		slots := sites[siteName]
		if slots == nil || !managed(node, machine, state) {
			continue
		}

		slots.managed++

		if state.Phase.Updating() {
			slots.updating++

			updatingGauge.WithLabelValues(siteName, string(state.Phase)).Inc()
		}

		// Each node unavailable for any reason uses one slot, including a
		// Machine whose Node doesn't exist.
		unavailable := node == nil || state.Phase.Updating() || state.Tainted() || !ready(node) || node.Spec.Unschedulable || busy[machine.Name]
		if unavailable {
			slots.unavailable++
		}

		if node == nil {
			continue
		}

		app, appErr := slots.settings.AppFor(node.Labels)

		if info, err := agentInfoOf(node); err == nil {
			nodesGauge.WithLabelValues(siteName, app.Name, info.CurrentVersion).Inc()
		}

		// A grant left to end must end before the node's next check.
		if unavailable || state.Phase != PhaseIdle || state.EndGrant != nil || node.Labels[names.PausedLabel] == "true" || !due(node, state, slots.settings, now) {
			continue
		}

		// A node without an app gets its slot, and Checking says why it
		// can't be checked.
		if appErr == nil && heldApps[app.AppID] != "" {
			slots.heldBack = true

			continue
		}

		slots.due = append(slots.due, candidate{machine: machine, state: state, canary: node.Labels[names.CanaryLabel] == "true", name: node.Name})
	}

	for app, machine := range heldApps {
		log.FromContext(ctx).Info("No update of an app starts while a node is held", "appID", app, "machine", machine)
	}

	for siteName, slots := range sites {
		if slots.heldBack {
			blockedGauge.WithLabelValues(siteName, "held").Set(1)
		}

		if slots.invalid != nil {
			blockedGauge.WithLabelValues(siteName, "invalid_settings").Set(1)
			log.FromContext(ctx).Error(slots.invalid, "No update starts in a Site whose settings don't fit Nebraska's update timeout", "site", siteName)

			continue
		}

		free := slots.settings.Slots(slots.managed) - slots.unavailable
		if free <= 0 || len(slots.due) == 0 {
			// Nodes that aren't updating use every slot, so nothing frees one.
			if len(slots.due) > 0 && slots.updating == 0 {
				blockedGauge.WithLabelValues(siteName, "unavailable").Set(1)
			}

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

// managed reports whether centerpivot updates the node: as its Node's label
// says, or, while the Node doesn't exist, the label the Machine gives
// kubelet. A node with an update under way counts until the update ends.
func managed(node *corev1.Node, machine *unboundedv1alpha3.Machine, state State) bool {
	if state.Phase.Updating() {
		return true
	}

	if node != nil {
		return node.Labels[names.ManagedLabel] == "true"
	}

	return machine.Spec.Kubernetes != nil && machine.Spec.Kubernetes.NodeLabels[names.ManagedLabel] == "true"
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
			sites[list.Items[i].Name] = &site{settings: settings, invalid: settings.Validate()}
		}
	}

	return sites, nil
}

func (s *Scheduler) nodesByName(ctx context.Context) (map[string]*corev1.Node, error) {
	var list corev1.NodeList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list Nodes: %w", err)
	}

	nodes := make(map[string]*corev1.Node, len(list.Items))
	for i := range list.Items {
		nodes[list.Items[i].Name] = &list.Items[i]
	}

	return nodes, nil
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
