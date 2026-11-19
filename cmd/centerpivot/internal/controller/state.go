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
	"github.com/Azure/unbounded/cmd/centerpivot/internal/omaha"
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
	// Grant is the Nebraska grant the update holds, until the agent reports
	// the update finished.
	Grant *Grant `json:"grant,omitempty"`
	// EndGrant is a grant the service still has to end, by reporting its
	// update failed. Every update that stops without finishing leaves one.
	// The node gets no slot until it is ended.
	EndGrant *Grant `json:"endGrant,omitempty"`
	// Request is the request the node's agent is asked to carry out.
	Request *contract.Request `json:"request,omitempty"`
	// RequestedAt is when Request was written, for the status deadlines.
	RequestedAt time.Time `json:"requestedAt,omitzero"`
	// Workloads are the workloads whose pods the drain moved.
	Workloads []drain.Workload `json:"workloads,omitempty"`
	// RecoveredAt is when the moved workloads recovered.
	RecoveredAt time.Time `json:"recoveredAt,omitzero"`
	// KeepTaint keeps a held node tainted, because it may start from the new
	// version at its next restart.
	KeepTaint bool `json:"keepTaint,omitempty"`
	// HeldApp is the Nebraska app ID a hold stops in every Site, because the
	// node restarted into a version that may be bad. A hold without it holds
	// only its node.
	HeldApp string `json:"heldApp,omitempty"`
	// Reason says why the node is held, or what its last check found.
	Reason string `json:"reason,omitempty"`
}

// Grant is a Nebraska grant of an update to the node: the check that took it,
// which the report that ends it repeats, and when. Nebraska's update timeout
// counts from then.
type Grant struct {
	Server    string    `json:"server"`
	AppID     string    `json:"appId"`
	Track     string    `json:"track"`
	MachineID string    `json:"machineId"`
	Version   string    `json:"version"`
	Arch      string    `json:"arch"`
	At        time.Time `json:"at"`
}

func grantOf(check omaha.Check, at time.Time) *Grant {
	return &Grant{
		Server: check.Server, AppID: check.AppID, Track: check.Track,
		MachineID: check.MachineID, Version: check.Version, Arch: check.Arch, At: at,
	}
}

// check returns the check that took the grant.
func (g Grant) check() omaha.Check {
	return omaha.Check{Server: g.Server, AppID: g.AppID, Track: g.Track, MachineID: g.MachineID, Version: g.Version, Arch: g.Arch}
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

// idle returns the state with the node back in Idle, its slot given back. A
// grant the update still holds is left to end.
func (s State) idle(now time.Time, reason string) State {
	return State{
		Phase:         PhaseIdle,
		Since:         now,
		LastCheck:     s.LastCheck,
		TargetVersion: s.TargetVersion,
		Attempts:      s.Attempts,
		EndGrant:      s.grantToEnd(),
		Reason:        reason,
	}
}

// held returns the state with only the node held, until a person clears it:
// its update stopped before it restarted, so nothing says the version is bad.
// A grant the update still holds is left to end.
func (s State) held(now time.Time, reason string, keepTaint bool) State {
	s.Phase = PhaseHeld
	s.Since = now
	s.Reason = reason
	s.KeepTaint = keepTaint
	s.HeldApp = ""
	s.EndGrant, s.Grant = s.grantToEnd(), nil

	return s
}

// heldApp holds the node, and stops its app in every Site: the node restarted
// into the new version, which may be bad.
func (s State) heldApp(now time.Time, reason string) State {
	s = s.held(now, reason, false)
	if s.Request != nil {
		s.HeldApp = s.Request.AppID
	}

	return s
}

func (s State) grantToEnd() *Grant {
	if s.Grant != nil {
		return s.Grant
	}

	return s.EndGrant
}

// grantedAt returns when the update's grant was taken, or the zero time,
// which leaves it no time, if it has none.
func (s State) grantedAt() time.Time {
	if s.Grant == nil {
		return time.Time{}
	}

	return s.Grant.At
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
