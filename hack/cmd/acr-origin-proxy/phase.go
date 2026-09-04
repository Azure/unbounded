// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

var errPhaseInflight = errors.New("current phase has requests in flight")

type benchmarkPhase string

const (
	phaseSetup      benchmarkPhase = "setup"
	phaseBaseline   benchmarkPhase = "baseline"
	phaseGantryCold benchmarkPhase = "gantry_cold"
	phaseIdle       benchmarkPhase = "idle"
)

type phaseSnapshot struct {
	RunID string         `json:"run_id"`
	Phase benchmarkPhase `json:"phase"`
}

type phaseController struct {
	mu       sync.RWMutex
	runID    string
	phase    benchmarkPhase
	inflight map[benchmarkPhase]uint64
}

func newPhaseController(runID string) (*phaseController, error) {
	if runID == "" {
		return nil, fmt.Errorf("run ID is required")
	}

	return &phaseController{
		runID:    runID,
		phase:    phaseSetup,
		inflight: make(map[benchmarkPhase]uint64),
	}, nil
}

func (c *phaseController) snapshot() phaseSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return phaseSnapshot{RunID: c.runID, Phase: c.phase}
}

func (c *phaseController) set(phase benchmarkPhase) error {
	if !phase.valid() {
		return fmt.Errorf("invalid benchmark phase %q", phase)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if phase != c.phase && c.inflight[c.phase] != 0 {
		return fmt.Errorf("%w: phase %q has %d", errPhaseInflight, c.phase, c.inflight[c.phase])
	}

	c.phase = phase

	return nil
}

func (c *phaseController) begin() phaseSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot := phaseSnapshot{RunID: c.runID, Phase: c.phase}
	c.inflight[c.phase]++

	return snapshot
}

func (c *phaseController) finish(snapshot phaseSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.inflight[snapshot.Phase] > 0 {
		c.inflight[snapshot.Phase]--
	}
}

func (p benchmarkPhase) valid() bool {
	switch p {
	case phaseSetup, phaseBaseline, phaseGantryCold, phaseIdle:
		return true
	default:
		return false
	}
}

func phaseControlHandler(controller *phaseController) http.Handler {
	type phaseUpdate struct {
		Phase benchmarkPhase `json:"phase"`
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			if err := json.NewEncoder(w).Encode(controller.snapshot()); err != nil {
				http.Error(w, "encode phase state", http.StatusInternalServerError)
			}
		case http.MethodPut:
			var update phaseUpdate
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				http.Error(w, "invalid phase update", http.StatusBadRequest)

				return
			}

			if err := controller.set(update.Phase); err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, errPhaseInflight) {
					status = http.StatusConflict
				}

				http.Error(w, err.Error(), status)

				return
			}

			if err := json.NewEncoder(w).Encode(controller.snapshot()); err != nil {
				http.Error(w, "encode phase state", http.StatusInternalServerError)
			}
		default:
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func requireControlToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(w, r)
	})
}
