// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/contract"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/drain"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/names"
)

// Phase is where a node is in its update (designs/centerpivot.md, section
// 4.5).
type Phase string

// The phases. A node without a state is Idle.
const (
	PhaseIdle       Phase = "Idle"
	PhaseChecking   Phase = "Checking"
	PhaseStaging    Phase = "Staging"
	PhaseDraining   Phase = "Draining"
	PhaseFinalizing Phase = "Finalizing"
	PhaseRestarting Phase = "Restarting"
	PhaseRecovering Phase = "Recovering"
	PhaseHeld       Phase = "Held"
)

// Updating reports whether a node in the phase holds an update slot.
func (p Phase) Updating() bool {
	return p != PhaseIdle && p != PhaseHeld
}

// maxAttempts is how many times an update to one version may fail on a node
// before the node is held.
const maxAttempts = 3

// State is a node's state, kept in its Machine's state annotation.
type State struct {
	Phase Phase `json:"phase"`
	// Since is when the node entered the phase.
	Since time.Time `json:"since,omitzero"`
	// LastCheck is when the node was last checked for an update.
	LastCheck time.Time `json:"lastCheck,omitzero"`
	// NodeUID is the Node the update started on. A Node rebuilt before its
	// restart ends the update.
	NodeUID types.UID `json:"nodeUid,omitempty"`
	// TargetVersion is the version granted, or last tried.
	TargetVersion string `json:"targetVersion,omitempty"`
	// Attempts counts the failed attempts at TargetVersion.
	Attempts int `json:"attempts,omitempty"`
	// GrantedAt is when Nebraska granted the update.
	GrantedAt time.Time `json:"grantedAt,omitzero"`
	// Request is the request the node's agent is asked to carry out.
	Request *contract.Request `json:"request,omitempty"`
	// RequestedAt is when Request was last written, and Writes how many
	// times, for the status deadline.
	RequestedAt time.Time `json:"requestedAt,omitzero"`
	Writes      int       `json:"writes,omitempty"`
	// Workloads are the workloads whose pods the drain moved.
	Workloads []drain.Workload `json:"workloads,omitempty"`
	// RecoveredAt is when the moved workloads recovered.
	RecoveredAt time.Time `json:"recoveredAt,omitzero"`
	// KeepTaint keeps a held node tainted, because it may start from the new
	// version at its next restart.
	KeepTaint bool `json:"keepTaint,omitempty"`
	// Reason says why the node is held, or what its last check found.
	Reason string `json:"reason,omitempty"`
}

// StateOf returns the Machine's state. A Machine without one is Idle; one
// whose state can't be read is held, so a person looks.
func StateOf(machine *unboundedv1alpha3.Machine) State {
	value, ok := machine.Annotations[names.StateAnnotation]
	if !ok {
		return State{Phase: PhaseIdle}
	}

	var state State
	if err := json.Unmarshal([]byte(value), &state); err != nil || state.Phase == "" {
		return State{Phase: PhaseHeld, Reason: fmt.Sprintf("the state annotation can't be read: %q", value)}
	}

	return state
}

// Tainted reports whether the node should have the updating taint.
func (s State) Tainted() bool {
	switch s.Phase {
	case PhaseDraining, PhaseFinalizing, PhaseRestarting:
		return true
	case PhaseHeld:
		return s.KeepTaint
	default:
		return false
	}
}

// RequestToWrite returns the request the node should carry, if its phase
// writes one.
func (s State) RequestToWrite() *contract.Request {
	if s.Phase == PhaseStaging || s.Phase == PhaseFinalizing {
		return s.Request
	}

	return nil
}

// idle returns the state with the node back in Idle, its slot given back.
func (s State) idle(now time.Time, reason string) State {
	return State{
		Phase:         PhaseIdle,
		Since:         now,
		LastCheck:     s.LastCheck,
		TargetVersion: s.TargetVersion,
		Attempts:      s.Attempts,
		Reason:        reason,
	}
}

// held returns the state with the node held until a person clears it.
func (s State) held(now time.Time, reason string, keepTaint bool) State {
	s.Phase = PhaseHeld
	s.Since = now
	s.Reason = reason
	s.KeepTaint = keepTaint

	return s
}

// failed counts a failed attempt at the target version, and holds the node
// after maxAttempts.
func (s State) failed(now time.Time, reason string) State {
	s.Attempts++
	if s.Attempts >= maxAttempts {
		return s.held(now, fmt.Sprintf("%s; %d attempts at %s failed", reason, s.Attempts, s.TargetVersion), false)
	}

	return s.idle(now, reason)
}

// request returns the state waiting for the agent to carry out request.
func (s State) request(phase Phase, now time.Time, request contract.Request) State {
	s.Phase = phase
	s.Since = now
	s.Request = &request
	s.RequestedAt = now
	s.Writes = 1

	return s
}

// Marshal returns the state as the annotation's value.
func (s State) Marshal() (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode state: %w", err)
	}

	return string(data), nil
}

// differs reports whether two states are written differently.
func differs(a, b State) (bool, error) {
	before, err := a.Marshal()
	if err != nil {
		return false, err
	}

	after, err := b.Marshal()
	if err != nil {
		return false, err
	}

	return before != after, nil
}

// writeState writes the state to the Machine, if it changed. The patch
// carries the Machine's resourceVersion, so two leaders can't both write.
func writeState(ctx context.Context, c client.Client, machine *unboundedv1alpha3.Machine, state State) error {
	value, err := state.Marshal()
	if err != nil {
		return err
	}

	if machine.Annotations[names.StateAnnotation] == value {
		return nil
	}

	patched := machine.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}

	patched.Annotations[names.StateAnnotation] = value

	if err := c.Patch(ctx, patched, client.MergeFromWithOptions(machine, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("write the state of Machine %s: %w", machine.Name, err)
	}

	machine.Annotations = patched.Annotations
	machine.ResourceVersion = patched.ResourceVersion

	return nil
}
