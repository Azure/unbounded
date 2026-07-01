// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runner

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

type RedfishHandler struct {
	vm       *VMManager
	cfg      RedfishConfig
	sessions map[string]struct{}
	mu       sync.Mutex
}

func NewRedfishHandler(vm *VMManager, cfg RedfishConfig) *RedfishHandler {
	return &RedfishHandler{
		vm:       vm,
		cfg:      cfg,
		sessions: map[string]struct{}{},
	}
}

func (h *RedfishHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}

	if r.Method == http.MethodGet && (path == "/redfish/v1" || path == "/redfish/v1/") {
		writeJSON(w, http.StatusOK, map[string]any{
			"@odata.id": "/redfish/v1/",
			"Systems":   map[string]string{"@odata.id": "/redfish/v1/Systems"},
		})

		return
	}

	if r.Method == http.MethodPost && path == "/redfish/v1/SessionService/Sessions" {
		h.createSession(w, r)

		return
	}

	if r.Method == http.MethodDelete && strings.HasPrefix(path, "/redfish/v1/SessionService/Sessions/") {
		h.deleteSession(w, r)

		return
	}

	if !h.authenticated(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	switch {
	case r.Method == http.MethodGet && path == "/redfish/v1/Systems":
		writeJSON(w, http.StatusOK, map[string]any{
			"Members": []map[string]string{{"@odata.id": "/redfish/v1/Systems/" + h.cfg.DeviceID}},
		})
	case r.Method == http.MethodGet && path == "/redfish/v1/Systems/"+h.cfg.DeviceID:
		h.getSystem(w)
	case r.Method == http.MethodPatch && path == "/redfish/v1/Systems/"+h.cfg.DeviceID:
		h.patchSystem(w, r)
	case r.Method == http.MethodPost && path == "/redfish/v1/Systems/"+h.cfg.DeviceID+"/Actions/ComputerSystem.Reset":
		h.reset(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *RedfishHandler) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserName string `json:"UserName"`
		Password string `json:"Password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	if !h.credentialsMatch(body.UserName, body.Password) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	token, err := randomToken()
	if err != nil {
		http.Error(w, "create token", http.StatusInternalServerError)

		return
	}

	h.mu.Lock()
	h.sessions[token] = struct{}{}
	h.mu.Unlock()

	w.Header().Set("X-Auth-Token", token)
	w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/"+token)
	writeJSON(w, http.StatusCreated, map[string]string{"Id": token})
}

func (h *RedfishHandler) deleteSession(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(strings.TrimRight(r.URL.Path, "/"), "/redfish/v1/SessionService/Sessions/")
	h.mu.Lock()
	delete(h.sessions, token)
	h.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (h *RedfishHandler) authenticated(r *http.Request) bool {
	if token := r.Header.Get("X-Auth-Token"); token != "" {
		h.mu.Lock()
		_, ok := h.sessions[token]
		h.mu.Unlock()

		if ok {
			return true
		}
	}

	user, pass, ok := r.BasicAuth()
	return ok && h.credentialsMatch(user, pass)
}

func (h *RedfishHandler) credentialsMatch(user, pass string) bool {
	return subtle.ConstantTimeCompare([]byte(user), []byte(h.cfg.Username)) == 1 &&
		subtle.ConstantTimeCompare([]byte(pass), []byte(h.cfg.Password)) == 1
}

func (h *RedfishHandler) getSystem(w http.ResponseWriter) {
	boot := h.vm.BootConfig()
	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.id":  "/redfish/v1/Systems/" + h.cfg.DeviceID,
		"Id":         h.cfg.DeviceID,
		"PowerState": h.vm.PowerState(),
		"Boot": map[string]string{
			"BootSourceOverrideTarget":  string(boot.Target),
			"BootSourceOverrideEnabled": string(boot.Enabled),
		},
	})
}

func (h *RedfishHandler) patchSystem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Boot struct {
			BootSourceOverrideTarget  BootTarget  `json:"BootSourceOverrideTarget"`
			BootSourceOverrideEnabled BootEnabled `json:"BootSourceOverrideEnabled"`
		} `json:"Boot"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	h.vm.SetBootConfig(BootConfig{
		Target:  body.Boot.BootSourceOverrideTarget,
		Enabled: body.Boot.BootSourceOverrideEnabled,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *RedfishHandler) reset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResetType ResetType `json:"ResetType"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	if err := h.vm.Reset(r.Context(), body.ResetType); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(b[:]), nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		fmt.Fprintln(w, err.Error()) //nolint:errcheck // Best effort after response write.
	}
}
