// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import "math/rand/v2"

// State is a simulated process's lifecycle state.
type State uint8

const (
	// Down means the process has never started or has finished leaving.
	Down State = iota
	// Starting means the process has started but not yet reported ready.
	Starting
	// Up means the process is running.
	Up
	// Paused means the process is frozen (stop the world).
	Paused
	// Leaving means the process is draining before a graceful exit.
	Leaving
	// Crashed means the process died without cleanup.
	Crashed
)

// String returns the state name.
func (s State) String() string {
	switch s {
	case Down:
		return "down"
	case Starting:
		return "starting"
	case Up:
		return "up"
	case Paused:
		return "paused"
	case Leaving:
		return "leaving"
	case Crashed:
		return "crashed"
	default:
		return "unknown"
	}
}

// Alive reports whether the process is running (possibly paused).
func (s State) Alive() bool { return s == Starting || s == Up || s == Paused || s == Leaving }

// Hooks are called by Processes on lifecycle transitions. The harness uses
// them to build or tear down the member under test.
type Hooks interface {
	// OnStart is called when member starts with a new incarnation.
	OnStart(member uint32, incarnation uint64)
	// OnCrash is called when member dies without cleanup.
	OnCrash(member uint32)
	// OnLeave is called when member begins a graceful exit.
	OnLeave(member uint32)
	// OnPause and OnResume freeze and unfreeze member.
	OnPause(member uint32)
	OnResume(member uint32)
}

// Process is one member's lifecycle record.
type Process struct {
	State       State
	Incarnation uint64
	// Starts counts how many times the process started.
	Starts uint32
	// Since is the simulated time of the last transition.
	Since int64
	// pausedFrom is the state to return to on Resume.
	pausedFrom State
}

// Processes is a table of simulated processes keyed by member index.
type Processes struct {
	sim   *Sim
	rng   *rand.Rand
	hooks Hooks
	procs []Process
}

// NewProcesses returns a table of members processes, all Down.
func NewProcesses(sim *Sim, members int, hooks Hooks) *Processes {
	p := &Processes{sim: sim, rng: sim.Stream("process"), hooks: hooks}
	p.Grow(members)

	return p
}

// Grow makes room for members [0, members).
func (p *Processes) Grow(members int) {
	for len(p.procs) < members {
		p.procs = append(p.procs, Process{})
	}
}

// Len returns the number of member slots.
func (p *Processes) Len() int { return len(p.procs) }

// Get returns member's record.
func (p *Processes) Get(member uint32) Process { return p.procs[member] }

// State returns member's state.
func (p *Processes) State(member uint32) State { return p.procs[member].State }

func (p *Processes) set(m uint32, s State) {
	p.procs[m].State = s
	p.procs[m].Since = p.sim.Now()
}

// Start starts a Down or Crashed member with a fresh nonzero incarnation
// and calls OnStart. It reports whether the member started.
func (p *Processes) Start(member uint32) bool {
	pr := &p.procs[member]
	if pr.State != Down && pr.State != Crashed {
		return false
	}

	inc := p.rng.Uint64()
	for inc == 0 || inc == pr.Incarnation {
		inc = p.rng.Uint64()
	}

	pr.Incarnation = inc
	pr.Starts++

	p.set(member, Starting)

	if p.hooks != nil {
		p.hooks.OnStart(member, inc)
	}

	return true
}

// MarkUp moves a Starting member to Up.
func (p *Processes) MarkUp(member uint32) bool {
	if p.procs[member].State != Starting {
		return false
	}

	p.set(member, Up)

	return true
}

// Crash kills a live member and calls OnCrash.
func (p *Processes) Crash(member uint32) bool {
	if !p.procs[member].State.Alive() {
		return false
	}

	p.set(member, Crashed)

	if p.hooks != nil {
		p.hooks.OnCrash(member)
	}

	return true
}

// Restart crashes member if it is alive and starts it again with a new
// incarnation.
func (p *Processes) Restart(member uint32) bool {
	p.Crash(member)

	return p.Start(member)
}

// Leave begins a graceful exit of a Starting or Up member and calls
// OnLeave. Call Exited when it finishes.
func (p *Processes) Leave(member uint32) bool {
	s := p.procs[member].State
	if s != Up && s != Starting {
		return false
	}

	p.set(member, Leaving)

	if p.hooks != nil {
		p.hooks.OnLeave(member)
	}

	return true
}

// Exited moves a Leaving member to Down.
func (p *Processes) Exited(member uint32) bool {
	if p.procs[member].State != Leaving {
		return false
	}

	p.set(member, Down)

	return true
}

// Pause freezes a Starting, Up or Leaving member and calls OnPause.
func (p *Processes) Pause(member uint32) bool {
	pr := &p.procs[member]
	if pr.State != Up && pr.State != Starting && pr.State != Leaving {
		return false
	}

	pr.pausedFrom = pr.State

	p.set(member, Paused)

	if p.hooks != nil {
		p.hooks.OnPause(member)
	}

	return true
}

// Resume unfreezes a Paused member and calls OnResume.
func (p *Processes) Resume(member uint32) bool {
	pr := &p.procs[member]
	if pr.State != Paused {
		return false
	}

	p.set(member, pr.pausedFrom)

	if p.hooks != nil {
		p.hooks.OnResume(member)
	}

	return true
}

// Count returns how many members are in state s.
func (p *Processes) Count(s State) int {
	n := 0

	for i := range p.procs {
		if p.procs[i].State == s {
			n++
		}
	}

	return n
}

// Live appends the members that are alive (Starting, Up, Paused, or
// Leaving) to dst in index order and returns it.
func (p *Processes) Live(dst []uint32) []uint32 {
	for i := range p.procs {
		if p.procs[i].State.Alive() {
			dst = append(dst, uint32(i))
		}
	}

	return dst
}

// InState appends the members in state s to dst in index order.
func (p *Processes) InState(dst []uint32, s State) []uint32 {
	for i := range p.procs {
		if p.procs[i].State == s {
			dst = append(dst, uint32(i))
		}
	}

	return dst
}
