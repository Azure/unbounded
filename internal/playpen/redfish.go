// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

const maxRedfishRequestBody = 1 << 20

type RedfishHandler struct {
	vm       *VMManager
	cfg      Config
	sessions map[string]struct{}
	mu       sync.RWMutex
}

func NewRedfishHandler(vm *VMManager, cfg Config) *RedfishHandler {
	return &RedfishHandler{vm: vm, cfg: cfg, sessions: map[string]struct{}{}}
}

func (h *RedfishHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}

	if r.Method == http.MethodGet && path == "/redfish/v1" {
		writeJSON(w, http.StatusOK, map[string]any{
			"@odata.id":      "/redfish/v1/",
			"Systems":        map[string]string{"@odata.id": "/redfish/v1/Systems"},
			"SessionService": map[string]string{"@odata.id": "/redfish/v1/SessionService"},
		})

		return
	}

	if r.Method == http.MethodPost && path == "/redfish/v1/SessionService/Sessions" {
		h.createSession(w, r)

		return
	}

	if !h.authenticated(r) {
		writeRedfishError(w, http.StatusUnauthorized, "Base.1.0.InsufficientPrivilege", "invalid Redfish credentials")

		return
	}

	if r.Method == http.MethodDelete && strings.HasPrefix(path, "/redfish/v1/SessionService/Sessions/") {
		h.deleteSession(w, path)

		return
	}

	systemPath := "/redfish/v1/Systems/" + h.cfg.BMCDeviceID

	switch {
	case r.Method == http.MethodGet && path == "/redfish/v1/Systems":
		writeJSON(w, http.StatusOK, map[string]any{
			"@odata.id":           "/redfish/v1/Systems",
			"Members":             []map[string]string{{"@odata.id": systemPath}},
			"Members@odata.count": 1,
		})
	case r.Method == http.MethodGet && path == systemPath:
		h.getSystem(w)
	case r.Method == http.MethodPatch && path == systemPath:
		h.patchSystem(w, r)
	case r.Method == http.MethodPost && path == systemPath+"/Actions/ComputerSystem.Reset":
		h.reset(w, r)
	default:
		writeRedfishError(w, http.StatusNotFound, "Base.1.0.ResourceMissingAtURI", "Redfish resource not found")
	}
}

func (h *RedfishHandler) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"UserName"`
		Password string `json:"Password"`
	}

	if err := decodeJSONBody(w, r, &body); err != nil {
		return
	}

	if !h.credentialsMatch(body.Username, body.Password) {
		writeRedfishError(w, http.StatusUnauthorized, "Base.1.0.InsufficientPrivilege", "invalid Redfish credentials")

		return
	}

	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		writeRedfishError(w, http.StatusInternalServerError, "Base.1.0.InternalError", "could not create session")

		return
	}

	token := hex.EncodeToString(tokenBytes[:])

	h.mu.Lock()
	h.sessions[token] = struct{}{}
	h.mu.Unlock()

	location := "/redfish/v1/SessionService/Sessions/" + token
	w.Header().Set("X-Auth-Token", token)
	w.Header().Set("Location", location)
	writeJSON(w, http.StatusCreated, map[string]string{"@odata.id": location, "Id": token})
}

func (h *RedfishHandler) deleteSession(w http.ResponseWriter, path string) {
	token := strings.TrimPrefix(path, "/redfish/v1/SessionService/Sessions/")

	h.mu.Lock()
	delete(h.sessions, token)
	h.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func (h *RedfishHandler) authenticated(r *http.Request) bool {
	if token := r.Header.Get("X-Auth-Token"); token != "" {
		h.mu.RLock()
		_, ok := h.sessions[token]
		h.mu.RUnlock()

		if ok {
			return true
		}
	}

	username, password, ok := r.BasicAuth()

	return ok && h.credentialsMatch(username, password)
}

func (h *RedfishHandler) credentialsMatch(username, password string) bool {
	return subtle.ConstantTimeCompare([]byte(username), []byte(h.cfg.BMCUsername)) == 1 &&
		subtle.ConstantTimeCompare([]byte(password), []byte(h.cfg.BMCPassword)) == 1
}

func (h *RedfishHandler) getSystem(w http.ResponseWriter) {
	boot := h.vm.BootConfig()
	systemPath := "/redfish/v1/Systems/" + h.cfg.BMCDeviceID

	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.id":   systemPath,
		"@odata.type": "#ComputerSystem.v1_20_0.ComputerSystem",
		"Id":          h.cfg.BMCDeviceID,
		"Name":        h.cfg.Name,
		"PowerState":  h.vm.PowerState(),
		"Boot": map[string]any{
			"BootSourceOverrideTarget":                          boot.Target,
			"BootSourceOverrideEnabled":                         boot.Enabled,
			"BootSourceOverrideTarget@Redfish.AllowableValues":  []BootTarget{BootTargetPxe, BootTargetHdd},
			"BootSourceOverrideEnabled@Redfish.AllowableValues": []BootEnabled{BootOnce, BootContinuous, BootDisabled},
		},
		"Actions": map[string]any{
			"#ComputerSystem.Reset": map[string]any{
				"target":                            systemPath + "/Actions/ComputerSystem.Reset",
				"ResetType@Redfish.AllowableValues": []ResetType{ResetOn, ResetForceOff},
			},
		},
	})
}

func (h *RedfishHandler) patchSystem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Boot struct {
			Target  BootTarget  `json:"BootSourceOverrideTarget"`
			Enabled BootEnabled `json:"BootSourceOverrideEnabled"`
		} `json:"Boot"`
	}

	if err := decodeJSONBody(w, r, &body); err != nil {
		return
	}

	if err := h.vm.SetBootConfig(BootConfig{Target: body.Boot.Target, Enabled: body.Boot.Enabled}); err != nil {
		writeRedfishError(w, http.StatusBadRequest, "Base.1.0.PropertyValueNotInList", err.Error())

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *RedfishHandler) reset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResetType ResetType `json:"ResetType"`
	}

	if err := decodeJSONBody(w, r, &body); err != nil {
		return
	}

	if err := h.vm.Reset(r.Context(), body.ResetType); err != nil {
		writeRedfishError(w, http.StatusBadRequest, "Base.1.0.ActionParameterValueTypeError", err.Error())

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxRedfishRequestBody))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		writeRedfishError(w, http.StatusBadRequest, "Base.1.0.MalformedJSON", "invalid JSON request body")

		return err
	}

	return nil
}

func writeRedfishError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	json.NewEncoder(w).Encode(value) //nolint:errcheck // The response has already been committed.
}
