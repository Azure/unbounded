// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPhaseController(t *testing.T) {
	controller, err := newPhaseController("run-20260714")
	if err != nil {
		t.Fatalf("newPhaseController: %v", err)
	}

	if got := controller.snapshot(); got.RunID != "run-20260714" || got.Phase != phaseSetup {
		t.Fatalf("initial snapshot = %+v", got)
	}

	if err := controller.set(phaseBaseline); err != nil {
		t.Fatalf("set baseline: %v", err)
	}

	if got := controller.snapshot(); got.RunID != "run-20260714" || got.Phase != phaseBaseline {
		t.Fatalf("baseline snapshot = %+v", got)
	}

	if err := controller.set("unknown"); err == nil {
		t.Fatal("set unknown phase succeeded")
	}

	if got := controller.snapshot(); got.Phase != phaseBaseline {
		t.Fatalf("invalid update changed phase to %q", got.Phase)
	}
}

func TestPhaseControllerRequiresRunID(t *testing.T) {
	if _, err := newPhaseController(""); err == nil {
		t.Fatal("newPhaseController accepted an empty run ID")
	}
}

func TestPhaseControllerRejectsTransitionWithInflightRequest(t *testing.T) {
	controller, err := newPhaseController("run-1")
	if err != nil {
		t.Fatalf("newPhaseController: %v", err)
	}

	attribution := controller.begin()
	if err := controller.set(phaseBaseline); err == nil {
		t.Fatal("phase transition succeeded with a setup request in flight")
	}

	if got := controller.snapshot().Phase; got != phaseSetup {
		t.Fatalf("phase changed to %q, want %q", got, phaseSetup)
	}

	controller.finish(attribution)

	if err := controller.set(phaseBaseline); err != nil {
		t.Fatalf("phase transition after request completion: %v", err)
	}
}

func TestPhaseControlHandler(t *testing.T) {
	controller, err := newPhaseController("run-1")
	if err != nil {
		t.Fatalf("newPhaseController: %v", err)
	}

	handler := phaseControlHandler(controller)

	request := httptest.NewRequest(http.MethodPut, "/control/phase", bytes.NewBufferString(`{"phase":"gantry_cold"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %q", response.Code, response.Body.String())
	}

	var got phaseSnapshot
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if got.RunID != "run-1" || got.Phase != phaseGantryCold {
		t.Fatalf("PUT snapshot = %+v", got)
	}

	request = httptest.NewRequest(http.MethodPut, "/control/phase", bytes.NewBufferString(`{"phase":"warm"}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid PUT status = %d, want %d", response.Code, http.StatusBadRequest)
	}

	if got := controller.snapshot(); got.Phase != phaseGantryCold {
		t.Fatalf("invalid PUT changed phase to %q", got.Phase)
	}
}

func TestRequireControlToken(t *testing.T) {
	handler := requireControlToken("secret", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/control/phase", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", response.Code, http.StatusUnauthorized)
	}

	request = httptest.NewRequest(http.MethodGet, "/control/phase", nil)
	request.Header.Set("Authorization", "Bearer secret")

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("authenticated status = %d, want %d", response.Code, http.StatusNoContent)
	}
}
