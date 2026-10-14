// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rackPowerOnPath is the slot power-on action target that rackIdentity advertises.
const rackPowerOnPath = rackManagerPath + "/Actions/Oem/Vendor.SystemPowerOn"

const rackIdentity = `{"Id":"RackManager","@odata.id":"/redfish/v1/Managers/RackManager","Actions":{"Oem":{` +
	`"Vendor.v1_0_0#SystemPower.Off":{"target":"/redfish/v1/Managers/RackManager/Actions/Oem/Vendor.SystemPowerOff"},` +
	`"Vendor.v1_0_0#SystemPower.On":{"target":"` + rackPowerOnPath + `"}}}}`

func completionBody(code, description string, nested bool) string {
	message := map[string]any{"@Message.ExtendedInfo": []any{map[string]any{
		"Oem": map[string]any{"Vendor": map[string]string{"CompletionCode": code, "Description": description}},
	}}}
	if nested {
		message = map[string]any{"error": message}
	}

	data, _ := json.Marshal(message)

	return string(data)
}

func rackServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "admin", user)
		assert.Equal(t, "secret", pass)
		assert.Empty(t, r.Header.Get("X-Auth-Token"))
		assert.NotContains(t, r.URL.Path, "SessionService")

		if r.URL.Path == rackManagerPath {
			assert.Equal(t, http.MethodGet, r.Method)

			_, _ = w.Write([]byte(rackIdentity))

			return
		}

		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func dialRack(t *testing.T, srv *httptest.Server, slot string) *Client {
	t.Helper()
	c, err := Dial(t.Context(), srv.URL+"/"+slot, tlsServerFingerprint(srv), "admin", "secret", "ignored-device-id")
	require.NoError(t, err)
	require.NotNil(t, c.rack)
	require.Same(t, c.session.httpClient, c.rack.root.httpClient)
	require.Equal(t, 3*time.Minute, c.session.httpClient.Timeout)
	require.Equal(t, 3*time.Minute, c.rack.root.httpClient.Timeout)
	require.Empty(t, c.session.token)
	require.Equal(t, slot, c.deviceID)
	t.Cleanup(c.Close)

	return c
}

func TestRackManagerRequests(t *testing.T) {
	var requests []string

	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet {
			assert.Equal(t, "/7"+rackSystemPath, r.URL.Path)

			_, _ = w.Write([]byte(`{"PowerState":"Off","Boot":{"BootSourceOverrideTarget":"Pxe","BootSourceOverrideMode":"UEFI"}}`))

			return
		}

		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var body map[string]json.RawMessage
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Len(t, body, 1)

		if r.Method == http.MethodPatch {
			assert.Equal(t, "/7"+rackSystemPath, r.URL.Path)

			var boot map[string]string
			assert.NoError(t, json.Unmarshal(body["Boot"], &boot))
			assert.Len(t, boot, 1)
			assert.Contains(t, []string{"Pxe", "Hdd", "None"}, boot["BootSourceOverrideTarget"])
		} else {
			assert.Equal(t, "/7"+rackSystemPath+"/Actions/ComputerSystem.Reset", r.URL.Path)
			assert.Contains(t, []string{`"On"`, `"ForceOff"`}, string(body["ResetType"]))
		}

		_, _ = w.Write([]byte(completionBody("Success", "", true)))
	})
	c := dialRack(t, srv, "7")
	state, err := c.PowerState(t.Context())
	require.NoError(t, err)
	require.Equal(t, PowerOff, state)
	config, err := c.GetBootConfig(t.Context())
	require.NoError(t, err)
	require.Equal(t, BootConfig{Target: BootTargetPxe, Mode: BootModeUEFI}, config)
	require.NoError(t, c.SetBootOverride(t.Context(), BootTargetPxe, BootContinuous))
	require.NoError(t, c.SetBootOverride(t.Context(), BootTargetHdd, BootOnce))
	require.NoError(t, c.DisableBootOverride(t.Context()))
	require.NoError(t, c.Reset(t.Context(), ResetForceOff))
	require.NoError(t, c.Reset(t.Context(), ResetOn))
	require.Len(t, requests, 7)

	require.ErrorIs(t, c.Reset(t.Context(), ResetForceRestart), ErrUnsupported)
	require.ErrorIs(t, c.SetHTTPBootOverride(t.Context(), "http://boot"), ErrUnsupported)
	require.ErrorIs(t, c.SetBootOverride(t.Context(), BootTargetUefiHTTP, BootContinuous), ErrUnsupported)
	require.ErrorIs(t, c.SetBootOverride(t.Context(), BootTargetPxe, "unknown"), ErrUnsupported)
	require.ErrorIs(t, c.SetBIOSHTTPBootURI(t.Context(), "http://boot"), ErrUnsupported)
	_, err = c.GetBIOSHTTPBootURI(t.Context())
	require.ErrorIs(t, err, ErrUnsupported)
	require.ErrorIs(t, c.SetStaticIPv4(t.Context(), StaticIPv4Config{}), ErrUnsupported)
	require.ErrorIs(t, c.SetBIOSStaticIPv4(t.Context(), StaticIPv4Config{}), ErrUnsupported)
	require.Len(t, requests, 7)
}

func TestRackManagerDetection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		path      string
		status    int
		body      string
		wantRack  bool
		wantError bool
	}{
		{"slot1", "/1", 200, rackIdentity, true, false},
		{"slot48", "/48/", 200, rackIdentity, true, false},
		{"zero", "/0", 200, rackIdentity, false, true},
		{"largeSlot", "/100", 200, rackIdentity, true, false},
		{"negative", "/-1", 200, rackIdentity, false, true},
		{"multipleSegments", "/7/8", 200, rackIdentity, false, true},
		{"name", "/blade", 200, rackIdentity, false, true},
		{"leadingZero", "/07", 200, rackIdentity, false, true},
		{"escaped", "/%37", 200, rackIdentity, false, true},
		{"query", "/7?x=1", 200, rackIdentity, false, true},
		{"arbitrary200", "/proxy", 200, `{}`, false, false},
		{"idOnly", "/proxy", 200, `{"Id":"RackManager"}`, false, false},
		{"noPowerOnAction", "/7", 200, `{"Id":"RackManager","@odata.id":"/redfish/v1/Managers/RackManager"}`, false, false},
		{"powerOnOutsideManager", "/7", 200, `{"Id":"RackManager","@odata.id":"/redfish/v1/Managers/RackManager",` +
			`"Actions":{"Oem":{"Vendor#SystemPower.On":{"target":"/redfish/v1/Systems/Actions/SystemPowerOn"}}}}`, false, false},
		{"missing", "/proxy", 404, ``, false, false},
		{"notImplemented", "/proxy", 501, ``, false, false},
		{"unauthorized", "/7", 401, ``, false, false},
		{"forbidden", "/7", 403, ``, false, false},
		{"unavailable", "/7", 503, ``, false, false},
		{"malformed", "/7", 200, `{`, false, false},
		{"failure", "/7", 200, completionBody("Failure", "", false), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sessions atomic.Int64

			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, pass, ok := r.BasicAuth()
				assert.True(t, ok)
				assert.Equal(t, "admin", user)
				assert.Equal(t, "secret", pass)

				if r.URL.Path == rackManagerPath {
					assert.Equal(t, http.MethodGet, r.Method)
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))

					return
				}

				if r.URL.Path == tc.path+"/redfish/v1/Systems/normal" {
					assert.Contains(t, []string{"unauthorized", "forbidden", "unavailable", "malformed", "failure"}, tc.name)
					assert.Equal(t, http.MethodGet, r.Method)

					_, _ = w.Write([]byte(`{"Id":"normal","PowerState":"On"}`))

					return
				}

				assert.Equal(t, tc.path+"/redfish/v1/SessionService/Sessions", r.URL.Path)
				sessions.Add(1)
				w.WriteHeader(http.StatusNotImplemented)
			}))
			defer srv.Close()

			c, err := Dial(t.Context(), srv.URL+tc.path, tlsServerFingerprint(srv), "admin", "secret", "normal")
			if tc.wantError {
				require.Error(t, err)
				require.Nil(t, c)
				require.Zero(t, sessions.Load())

				return
			}

			require.NoError(t, err)

			defer c.Close()

			require.Equal(t, tc.wantRack, c.rack != nil)

			if !tc.wantRack {
				require.Equal(t, int64(1), sessions.Load())
				require.Equal(t, "normal", c.deviceID)
				require.Equal(t, 30*time.Second, c.session.httpClient.Timeout)
			} else {
				require.Equal(t, 3*time.Minute, c.session.httpClient.Timeout)
				require.Equal(t, 3*time.Minute, c.rack.root.httpClient.Timeout)
			}
		})
	}
}

func TestRackManagerNoPathDoesNotProbe(t *testing.T) {
	for _, suffix := range []string{"", "/"} {
		t.Run(suffix, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.NotEqual(t, rackManagerPath, r.URL.Path)

				switch r.URL.Path {
				case "/redfish/v1/SessionService/Sessions":
					w.WriteHeader(http.StatusNotImplemented)
				case "/redfish/v1/Systems":
					_, _ = w.Write([]byte(`{"Members":[{"@odata.id":"/redfish/v1/Systems/normal"}]}`))
				case "/redfish/v1/Systems/normal":
					_, _ = w.Write([]byte(`{"PowerState":"On"}`))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer srv.Close()

			c, err := Dial(t.Context(), srv.URL+suffix, tlsServerFingerprint(srv), "admin", "secret", "")
			require.NoError(t, err)

			defer c.Close()

			require.Nil(t, c.rack)
			state, err := c.PowerState(t.Context())
			require.NoError(t, err)
			require.Equal(t, PowerOn, state)
		})
	}
}

func TestRackManagerDetectionPreservesTransport(t *testing.T) {
	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	})

	original := newHTTPClient(tlsServerFingerprint(srv))
	defer original.CloseIdleConnections()

	blade := &bmcSession{httpClient: original, baseURL: srv.URL + "/7", user: "admin", pass: "secret"}
	rack, inconclusive, err := detectRackManager(t.Context(), blade)
	require.NoError(t, err)
	require.False(t, inconclusive)
	require.NotNil(t, rack)
	require.NotSame(t, original, blade.httpClient)
	require.Equal(t, 30*time.Second, original.Timeout)
	require.Equal(t, 3*time.Minute, blade.httpClient.Timeout)
	require.Equal(t, 3*time.Minute, rack.root.httpClient.Timeout)
	require.Same(t, original.Transport, blade.httpClient.Transport)
	require.Same(t, original.Transport, rack.root.httpClient.Transport)
}

func TestRackManagerHardwareUnavailableRecovery(t *testing.T) {
	for _, description := range []string{"Failed to query system IPMI interface.", "Failed to discover system control interface."} {
		for _, state := range []string{"Off", "On", "--", "Failure"} {
			t.Run(description+"/"+state, func(t *testing.T) {
				var restores atomic.Int64

				srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case rackManagerPath + "/SystemPower":
						_, _ = fmt.Fprintf(w, `{"System Power States":{"7":%q}}`, state)
					case rackPowerOnPath:
						restores.Add(1)
						w.WriteHeader(http.StatusNoContent)
					default:
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = w.Write([]byte(completionBody("Failure", description, true)))
					}
				})
				c := dialRack(t, srv, "7")
				err := c.Reset(t.Context(), ResetOn)
				require.Error(t, err)

				if state == "Off" {
					require.ErrorIs(t, err, ErrNotReady)
					require.Equal(t, int64(1), restores.Load())
				} else {
					require.NotErrorIs(t, err, ErrNotReady)
					require.Zero(t, restores.Load())
					require.Equal(t, recoveryNone, c.rack.pending)
				}
			})
		}
	}
}

func TestRackManagerDetectionFallsBackToWorkingPrefixedBMC(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		deviceID string
	}{
		{"unauthorized", http.StatusUnauthorized, "root requires other credentials", ""},
		{"unavailable", http.StatusServiceUnavailable, "root unavailable", ""},
		{"html", http.StatusOK, "<html>Management proxy</html>", ""},
		{"unauthorizedWithDeviceID", http.StatusUnauthorized, "root requires other credentials", "normal"},
		{"forbiddenWithDeviceID", http.StatusForbidden, "root requires other credentials", "normal"},
		{"unavailableWithDeviceID", http.StatusServiceUnavailable, "root unavailable", "normal"},
		{"htmlWithDeviceID", http.StatusOK, "<html>Management proxy</html>", "normal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				probes atomic.Int64
				resets atomic.Int64
			)

			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case rackManagerPath:
					user, pass, ok := r.BasicAuth()
					assert.True(t, ok)
					assert.Equal(t, "admin", user)
					assert.Equal(t, "secret", pass)
					probes.Add(1)
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				case "/proxy/redfish/v1/SessionService/Sessions":
					assert.Equal(t, http.MethodPost, r.Method)
					w.Header().Set("X-Auth-Token", testToken)
					w.WriteHeader(http.StatusCreated)
				case "/proxy/redfish/v1/Systems":
					assert.Equal(t, testToken, r.Header.Get("X-Auth-Token"))

					_, _ = w.Write([]byte(`{"Members":[{"@odata.id":"/redfish/v1/Systems/normal"}]}`))
				case "/proxy/redfish/v1/Systems/normal":
					assert.Equal(t, testToken, r.Header.Get("X-Auth-Token"))

					_, _ = w.Write([]byte(`{"Id":"normal","PowerState":"On"}`))
				case "/proxy/redfish/v1/Systems/normal/Actions/ComputerSystem.Reset":
					assert.Equal(t, testToken, r.Header.Get("X-Auth-Token"))
					assert.Equal(t, http.MethodPost, r.Method)
					resets.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			c, err := Dial(t.Context(), srv.URL+"/proxy", tlsServerFingerprint(srv), "admin", "secret", tc.deviceID)
			require.NoError(t, err)

			defer c.Close()

			require.Nil(t, c.rack)
			require.Equal(t, "normal", c.deviceID)
			state, err := c.PowerState(t.Context())
			require.NoError(t, err)
			require.Equal(t, PowerOn, state)
			require.NoError(t, c.Reset(t.Context(), ResetForceRestart))
			require.Equal(t, int64(1), probes.Load())
			require.Equal(t, int64(1), resets.Load())
		})
	}
}

func TestValidateFallbackSystemAcceptsResourceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"idOnly", `{"Id":"normal"}`},
		{"odataIDOnly", `{"@odata.id":"/redfish/v1/Systems/normal"}`},
		{"both", `{"Id":"normal","@odata.id":"/redfish/v1/Systems/normal","PowerState":"Off"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/redfish/v1/Systems/normal", r.URL.Path)

				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			httpClient := newHTTPClient(tlsServerFingerprint(srv))
			defer httpClient.CloseIdleConnections()

			session := &bmcSession{httpClient: httpClient, baseURL: srv.URL}
			require.NoError(t, validateFallbackSystem(t.Context(), session, "normal"))
		})
	}
}

func TestRackManagerDetectionRetriesAfterInvalidFallback(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           int
		body             string
		standardRecovery bool
	}{
		{"missing", http.StatusNotFound, "", false},
		{"unavailable", http.StatusServiceUnavailable, "", false},
		{"html", http.StatusOK, "<html>Management proxy</html>", false},
		{"null", http.StatusOK, "null", false},
		{"emptyObject", http.StatusOK, `{}`, false},
		{"emptyObjectStandardRecovery", http.StatusOK, `{}`, true},
		{"emptyIdentity", http.StatusOK, `{"Id":"","@odata.id":" "}`, false},
		{"invalidIdentityType", http.StatusOK, `{"Id":1}`, false},
		{"nullIdentity", http.StatusOK, `{"Id":null,"@odata.id":null}`, false},
		{"powerStateOnly", http.StatusOK, `{"PowerState":"On"}`, false},
		{"error", http.StatusOK, `{"error":{"code":"GeneralError"}}`, false},
		{"errorWithIdentity", http.StatusOK, `{"Id":"1","error":{"code":"GeneralError"}}`, false},
		{"completionFailure", http.StatusOK, completionBody("Failure", "BMC unavailable", false), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				recovered atomic.Bool
				probes    atomic.Int64
				systems   atomic.Int64
				closed    atomic.Int64
			)

			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case rackManagerPath:
					probes.Add(1)

					if !recovered.Load() || tc.standardRecovery {
						w.WriteHeader(http.StatusServiceUnavailable)

						return
					}

					_, _ = w.Write([]byte(rackIdentity))
				case "/28/redfish/v1/SessionService/Sessions":
					assert.Equal(t, http.MethodPost, r.Method)
					w.Header().Set("X-Auth-Token", testToken)
					w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/test")
					w.WriteHeader(http.StatusCreated)
				case "/28/redfish/v1/Systems/1":
					assert.Equal(t, http.MethodGet, r.Method)
					assert.Equal(t, testToken, r.Header.Get("X-Auth-Token"))
					systems.Add(1)

					if recovered.Load() {
						_, _ = w.Write([]byte(`{"Id":"1","PowerState":"On"}`))
						return
					}

					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				case "/28/redfish/v1/SessionService/Sessions/test":
					assert.Equal(t, http.MethodDelete, r.Method)
					assert.Equal(t, testToken, r.Header.Get("X-Auth-Token"))
					closed.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			url := srv.URL + "/28"
			fingerprint := tlsServerFingerprint(srv)
			c, err := Dial(t.Context(), url, fingerprint, "admin", "secret", "1")
			require.ErrorContains(t, err, "validating standard BMC after inconclusive rack detection")
			require.Nil(t, c)
			require.Equal(t, int64(1), probes.Load())

			pool := NewPool()
			defer pool.Close()

			c, err = pool.Get(t.Context(), url, fingerprint, "admin", "secret", "1")
			require.ErrorContains(t, err, "validating standard BMC after inconclusive rack detection")
			require.Nil(t, c)
			require.Empty(t, pool.clients)
			require.Equal(t, int64(2), probes.Load())
			require.Equal(t, int64(2), systems.Load())
			require.Equal(t, int64(2), closed.Load())

			recovered.Store(true)

			c, err = pool.Get(t.Context(), url, fingerprint, "admin", "secret", "1")
			require.NoError(t, err)
			require.Equal(t, int64(3), probes.Load())

			if tc.standardRecovery {
				require.Nil(t, c.rack)
				require.Equal(t, "1", c.deviceID)
				require.Equal(t, int64(3), systems.Load())
			} else {
				require.NotNil(t, c.rack)
				require.Equal(t, "28", c.deviceID)
				require.True(t, c.SupportsVirtualMedia())
				require.Equal(t, int64(2), systems.Load())
			}

			cached, err := pool.Get(t.Context(), url, fingerprint, "admin", "secret", "1")
			require.NoError(t, err)
			require.Same(t, c, cached)
			require.Equal(t, int64(3), probes.Load())
		})
	}
}

func TestRackManagerDetectionPreservesCancellation(t *testing.T) {
	var sessions atomic.Int64

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == rackManagerPath {
			cancel()
			<-r.Context().Done()

			return
		}

		sessions.Add(1)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	defer srv.Close()

	c, err := Dial(ctx, srv.URL+"/proxy", tlsServerFingerprint(srv), "admin", "secret", "normal")
	require.Nil(t, c)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, sessions.Load())
}

func TestRackManagerDetectionProbeTimeoutFallsBack(t *testing.T) {
	var probes atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case rackManagerPath:
			probes.Add(1)
			<-r.Context().Done()
		case "/proxy/redfish/v1/Systems/normal":
			_, _ = w.Write([]byte(`{"Id":"normal","PowerState":"On"}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	httpClient := newHTTPClient(tlsServerFingerprint(srv))

	httpClient.Timeout = 100 * time.Millisecond
	defer httpClient.CloseIdleConnections()

	session := &bmcSession{httpClient: httpClient, baseURL: srv.URL + "/proxy", user: "admin", pass: "secret"}
	rack, inconclusive, err := detectRackManager(t.Context(), session)
	require.NoError(t, err)
	require.True(t, inconclusive)
	require.Nil(t, rack)
	require.NoError(t, t.Context().Err())
	require.Equal(t, int64(1), probes.Load())

	require.NoError(t, validateFallbackSystem(t.Context(), session, "normal"))

	c := &Client{session: session, deviceID: "normal"}
	state, err := c.PowerState(t.Context())
	require.NoError(t, err)
	require.Equal(t, PowerOn, state)
}

func TestRackManagerPowerFallback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		bladeStatus int
		bladeBody   string
		rackBody    string
		want        PowerState
		fallback    bool
	}{
		{"off", 503, "", `{"System Power States":{"7":"Off"}}`, PowerOff, true},
		{"onRequiresBlade", 503, "", `{"System Power States":{"7":"On"}}`, "", true},
		{"missing", 503, "", `{"System Power States":{"8":"Off"}}`, "", true},
		{"unknown", 503, "", `{"System Power States":{"7":"--"}}`, "", true},
		{"failureState", 503, "", `{"System Power States":{"7":"Failure"}}`, "", true},
		{"wrongType", 503, "", `{"System Power States":{"7":7}}`, "", true},
		{"malformedRack", 503, "", `{`, "", true},
		{"rackFailure", 503, "", completionBody("Failure", "", true), "", true},
		{"bladeFailure", 200, completionBody("Failure", "BMC unavailable", false), `{"System Power States":{"7":"Off"}}`, PowerOff, true},
		{"bladeQueryFailure", 500, completionBody("Failure", "Failed to query system IPMI interface.", true), `{"System Power States":{"7":"Off"}}`, PowerOff, true},
		{"bladeDiscoveryFailure", 500, completionBody("Failure", "Failed to discover system control interface.", true), `{"System Power States":{"7":"Off"}}`, PowerOff, true},
		{"bladeAuth", 401, "", "", "", false},
		{"bladePermission", 403, "", "", "", false},
		{"bladeMalformed", 200, `{`, "", "", false},
		{"bladeMissingPower", 200, `{}`, "", "", false},
		{"bladeInvalidPower", 200, `{"PowerState":"--"}`, "", "", false},
		{"bladeOn", 200, `{"PowerState":"On"}`, "", PowerOn, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fallback atomic.Int64

			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)

				if r.URL.Path == rackManagerPath+"/SystemPower" {
					fallback.Add(1)

					_, _ = w.Write([]byte(tc.rackBody))

					return
				}

				assert.Equal(t, "/7"+rackSystemPath, r.URL.Path)
				w.WriteHeader(tc.bladeStatus)
				_, _ = w.Write([]byte(tc.bladeBody))
			})
			c := dialRack(t, srv, "7")
			state, err := c.PowerState(t.Context())
			require.Equal(t, tc.want, state)

			if tc.want == "" {
				require.Error(t, err)
				require.NotErrorIs(t, err, ErrNotReady)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, tc.fallback, fallback.Load() > 0)
		})
	}
}

func TestRackManagerRecoveryBounded(t *testing.T) {
	var (
		acCalls    atomic.Int64
		bladeReady atomic.Bool
		rackOn     atomic.Bool
	)

	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case rackManagerPath + "/SystemPower":
			state := "Off"
			if rackOn.Load() {
				state = "On"
			}

			_, _ = fmt.Fprintf(w, `{"System Power States":{"7":%q}}`, state)
		case rackPowerOnPath:
			assert.Equal(t, http.MethodPost, r.Method)

			var body map[string]string
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, map[string]string{"system_id": "7"}, body)
			acCalls.Add(1)

			_, _ = w.Write([]byte(completionBody("Success", "", true)))
		case "/7" + rackSystemPath:
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"PowerState":"Off","Boot":{"BootSourceOverrideTarget":"Hdd"}}`))
				return
			}

			if bladeReady.Load() {
				w.WriteHeader(204)
				return
			}

			_, _ = w.Write([]byte(completionBody("Failure", "BMC unavailable", true)))
		default:
			w.WriteHeader(503)
		}
	})
	c := dialRack(t, srv, "7")
	now := time.Unix(100, 0)
	c.rack.now = func() time.Time { return now }
	boot := func() error { return c.DisableBootOverride(t.Context()) }
	require.ErrorIs(t, boot(), ErrNotReady)
	require.Equal(t, int64(1), acCalls.Load())

	deadline := c.rack.deadline
	for range 3 {
		_, err := c.PowerState(t.Context())
		require.NoError(t, err)
		_, err = c.GetBootConfig(t.Context())
		require.NoError(t, err)
		require.ErrorIs(t, boot(), ErrNotReady)
	}

	require.Equal(t, deadline, c.rack.deadline)
	require.Equal(t, int64(1), acCalls.Load())
	rackOn.Store(true)

	now = now.Add(9 * time.Minute)

	require.ErrorIs(t, boot(), ErrNotReady)

	now = deadline
	err := boot()
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotReady)
	require.Contains(t, err.Error(), "grace expired")
	rackOn.Store(false)

	for range 3 {
		require.NotErrorIs(t, boot(), ErrNotReady)
	}

	require.Equal(t, int64(1), acCalls.Load())
	bladeReady.Store(true)
	require.NoError(t, boot())
	require.Equal(t, recoveryNone, c.rack.pending)
	bladeReady.Store(false)
	require.ErrorIs(t, boot(), ErrNotReady)
	require.Equal(t, int64(2), acCalls.Load())
}

func TestRackManagerRecoveryTwoOutagesOnCachedClient(t *testing.T) {
	var (
		acCalls    atomic.Int64
		bladeReady atomic.Bool
		rackOff    atomic.Bool
	)

	rackOff.Store(true)

	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case rackManagerPath + "/SystemPower":
			state := "On"
			if rackOff.Load() {
				state = "Off"
			}

			_, _ = fmt.Fprintf(w, `{"System Power States":{"7":%q}}`, state)
		case rackPowerOnPath:
			acCalls.Add(1)
			rackOff.Store(false)
			w.WriteHeader(http.StatusNoContent)
		case "/7" + rackSystemPath:
			if !bladeReady.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}

			if r.Method == http.MethodPatch {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			_, _ = w.Write([]byte(`{"PowerState":"On","Boot":{"BootSourceOverrideTarget":"Pxe"}}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	pool := NewPool()
	defer pool.Close()

	c, err := pool.Get(t.Context(), srv.URL+"/7", tlsServerFingerprint(srv), "admin", "secret", "")
	require.NoError(t, err)

	now := time.Unix(100, 0)
	c.rack.now = func() time.Time { return now }
	boot := func() error { return c.DisableBootOverride(t.Context()) }
	require.ErrorIs(t, boot(), ErrNotReady)

	deadline := c.rack.deadline
	now = now.Add(time.Minute)

	bladeReady.Store(true)
	require.NoError(t, boot())
	state, err := c.PowerState(t.Context())
	require.NoError(t, err)
	require.Equal(t, PowerOn, state)
	require.True(t, c.rack.pendingCompleted)
	require.Equal(t, deadline, c.rack.deadline)

	// No successful call occurs after the first window before the next outage.
	now = deadline.Add(time.Minute)

	bladeReady.Store(false)
	rackOff.Store(true)

	again, err := pool.Get(t.Context(), srv.URL+"/7", tlsServerFingerprint(srv), "admin", "secret", "")
	require.NoError(t, err)
	require.Same(t, c, again)
	require.ErrorIs(t, again.DisableBootOverride(t.Context()), ErrNotReady)
	require.Equal(t, int64(2), acCalls.Load())
	require.False(t, c.rack.pendingCompleted)
	require.Equal(t, now.Add(rackRecoveryGrace), c.rack.deadline)

	// The second, incomplete episode must not rearm just because its window ends.
	now = c.rack.deadline

	rackOff.Store(true)

	for range 3 {
		err := boot()
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNotReady)

		now = now.Add(rackRecoveryGrace)
	}

	require.Equal(t, int64(2), acCalls.Load())
}

func TestRackManagerPowerReadCompletesOnlyResetOnRecovery(t *testing.T) {
	for _, tc := range []struct {
		name         string
		op           recoveryOp
		bladeState   PowerState
		rackState    PowerState
		wantState    PowerState
		wantComplete bool
	}{
		{"resetOnBladeOn", recoveryResetOn, PowerOn, PowerOn, PowerOn, true},
		{"resetOnBladeOff", recoveryResetOn, PowerOff, PowerOn, PowerOff, false},
		{"resetOnRackOn", recoveryResetOn, "", PowerOn, "", false},
		{"resetOnRackOff", recoveryResetOn, "", PowerOff, PowerOff, false},
		{"clearOverrideBladeOn", recoveryClearOverride, PowerOn, PowerOn, PowerOn, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				acCalls    atomic.Int64
				bladeReady atomic.Bool
				rackOn     atomic.Bool
			)

			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case rackManagerPath + "/SystemPower":
					state := PowerOff
					if rackOn.Load() {
						state = PowerOn
					}

					_, _ = fmt.Fprintf(w, `{"System Power States":{"7":%q}}`, state)
				case rackPowerOnPath:
					assert.Equal(t, http.MethodPost, r.Method)
					acCalls.Add(1)
					w.WriteHeader(http.StatusNoContent)
				case "/7" + rackSystemPath:
					if r.Method == http.MethodGet && bladeReady.Load() {
						_, _ = fmt.Fprintf(w, `{"PowerState":%q}`, tc.bladeState)
						return
					}

					w.WriteHeader(http.StatusServiceUnavailable)
				case "/7" + rackSystemPath + "/Actions/ComputerSystem.Reset":
					assert.Equal(t, http.MethodPost, r.Method)
					w.WriteHeader(http.StatusServiceUnavailable)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			})
			c := dialRack(t, srv, "7")
			now := time.Unix(100, 0)
			c.rack.now = func() time.Time { return now }
			operation := func() error {
				if tc.op == recoveryClearOverride {
					return c.DisableBootOverride(t.Context())
				}

				return c.Reset(t.Context(), ResetOn)
			}
			require.ErrorIs(t, operation(), ErrNotReady)
			require.Equal(t, int64(1), acCalls.Load())
			require.False(t, c.rack.pendingCompleted)

			deadline := c.rack.deadline
			now = now.Add(time.Minute)

			bladeReady.Store(tc.bladeState != "")
			rackOn.Store(tc.rackState == PowerOn)

			for range 2 {
				state, err := c.PowerState(t.Context())
				require.Equal(t, tc.wantState, state)

				if tc.wantState == "" {
					require.ErrorIs(t, err, ErrNotReady)
				} else {
					require.NoError(t, err)
				}

				require.Equal(t, tc.wantComplete, c.rack.pendingCompleted)
				require.Equal(t, tc.op, c.rack.pending)
				require.Equal(t, deadline, c.rack.deadline)
			}

			bladeReady.Store(false)
			rackOn.Store(false)

			now = deadline.Add(-time.Second)

			require.ErrorIs(t, operation(), ErrNotReady)
			require.Equal(t, int64(1), acCalls.Load())
			require.Equal(t, deadline, c.rack.deadline)

			now = deadline.Add(time.Second)

			err := operation()
			if tc.wantComplete {
				require.ErrorIs(t, err, ErrNotReady)
				require.Equal(t, int64(2), acCalls.Load())
				require.Equal(t, now.Add(rackRecoveryGrace), c.rack.deadline)
			} else {
				require.ErrorContains(t, err, "grace expired")
				require.NotErrorIs(t, err, ErrNotReady)
				require.Equal(t, int64(1), acCalls.Load())
				require.Equal(t, deadline, c.rack.deadline)
			}

			require.Equal(t, tc.op, c.rack.pending)
			require.False(t, c.rack.pendingCompleted)
		})
	}
}

func TestRackManagerRecoveryOperationsAndConcurrency(t *testing.T) {
	for _, op := range []string{"boot", "disable", "on", "off", "read"} {
		t.Run(op, func(t *testing.T) {
			var acCalls atomic.Int64

			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case rackManagerPath + "/SystemPower":
					_, _ = w.Write([]byte(`{"System Power States":{"7":"Off"}}`))
				case rackPowerOnPath:
					acCalls.Add(1)
					w.WriteHeader(204)
				default:
					w.WriteHeader(503)
				}
			})
			c := dialRack(t, srv, "7")

			var wg sync.WaitGroup

			errs := make(chan error, 8)

			for range 8 {
				wg.Go(func() {
					switch op {
					case "boot":
						errs <- c.SetBootOverride(t.Context(), BootTargetPxe, BootOnce)
					case "disable":
						errs <- c.DisableBootOverride(t.Context())
					case "on":
						errs <- c.Reset(t.Context(), ResetOn)
					case "off":
						errs <- c.Reset(t.Context(), ResetForceOff)
					case "read":
						_, err := c.PowerState(t.Context())
						errs <- err
					}
				})
			}

			wg.Wait()
			close(errs)

			for err := range errs {
				switch op {
				case "off", "boot":
					require.Error(t, err)
					require.NotErrorIs(t, err, ErrNotReady)
				case "read":
					require.NoError(t, err)
				default:
					require.ErrorIs(t, err, ErrNotReady)
				}
			}

			want := int64(1)
			if op == "read" || op == "off" || op == "boot" {
				want = 0
			}

			require.Equal(t, want, acCalls.Load())
		})
	}
}

func TestRackManagerRecoveryDoesNotHideErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"auth", 401, ""},
		{"permission", 403, ""},
		{"serverPermission", 500, completionBody("Failure", "Permission denied", true)},
		{"serverAuth", 503, `{"error":{"message":"Authentication failed"}}`},
		{"badRequest", 400, ""},
		{"unsupported", 405, ""},
		{"malformed", 200, `{`},
		{"null", 200, `null`},
		{"permissionCompletion", 200, completionBody("Failure", "Permission denied", true)},
		{"authCompletion", 200, completionBody("Failure", "Authentication failed", false)},
		{"invalidCompletion", 200, completionBody("Failure", "Invalid parameter", true)},
		{"queryDenied", 500, completionBody("Failure", "Failed to query: permission denied", true)},
		{"discoveryInvalid", 500, completionBody("Failure", "Failed to discover: invalid parameter", true)},
		{"unknownFailure", 200, completionBody("Failure", "Unexpected hardware error", true)},
		{"genericError", 200, `{"error":{"message":"no permission"}}`},
		{"malformedCompletion", 200, `{"@Message.ExtendedInfo":[{"Oem":{"Vendor":{"CompletionCode":4}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/7"+rackSystemPath, r.URL.Path)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			c := dialRack(t, srv, "7")
			c.rack.pending = recoveryClearOverride
			c.rack.deadline = time.Now().Add(time.Minute)
			err := c.DisableBootOverride(t.Context())
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrNotReady)
		})
	}
}

func TestRackManagerACFailureIsLatched(t *testing.T) {
	for _, response := range []struct {
		status int
		body   string
	}{
		{403, "denied"}, {200, completionBody("Failure", "Power fault", true)}, {200, `{`},
	} {
		t.Run(fmt.Sprint(response.status, response.body), func(t *testing.T) {
			var acCalls atomic.Int64

			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case rackManagerPath + "/SystemPower":
					_, _ = w.Write([]byte(`{"System Power States":{"7":"Off"}}`))
				case rackPowerOnPath:
					acCalls.Add(1)
					w.WriteHeader(response.status)
					_, _ = w.Write([]byte(response.body))
				default:
					w.WriteHeader(503)
				}
			})

			c := dialRack(t, srv, "7")
			now := time.Unix(100, 0)

			c.rack.now = func() time.Time { return now }
			for range 3 {
				err := c.Reset(t.Context(), ResetOn)
				require.Error(t, err)
				require.NotErrorIs(t, err, ErrNotReady)
				require.Contains(t, err.Error(), "restoring AC power")

				now = now.Add(2 * rackRecoveryGrace)
			}

			require.Equal(t, int64(1), acCalls.Load())
		})
	}
}

func TestRackManagerPoolSlotsAndPinning(t *testing.T) {
	var calls sync.Map

	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Store(r.URL.Path, true)

		_, _ = w.Write([]byte(`{"PowerState":"On"}`))
	})

	p := NewPool()
	defer p.Close()

	var clients []*Client

	for _, slot := range []string{"7", "8"} {
		c, err := p.Get(t.Context(), srv.URL+"/"+slot, tlsServerFingerprint(srv), "admin", "secret", "ignored")
		require.NoError(t, err)
		again, err := p.Get(t.Context(), srv.URL+"/"+slot, tlsServerFingerprint(srv), "admin", "secret", "different")
		require.NoError(t, err)
		require.Same(t, c, again)
		require.Equal(t, 3*time.Minute, c.session.httpClient.Timeout)
		require.Equal(t, 3*time.Minute, c.rack.root.httpClient.Timeout)
		clients = append(clients, c)
		_, err = c.PowerState(t.Context())
		require.NoError(t, err)

		_, ok := calls.Load("/" + slot + rackSystemPath)
		require.True(t, ok)
	}

	require.NotSame(t, clients[0], clients[1])
	_, err := Dial(t.Context(), srv.URL+"/7", strings.Repeat("00:", 31)+"00", "admin", "secret", "")
	require.ErrorContains(t, err, "TLS cert SHA256 mismatch")
}

func TestCompletionChecksNormalClient(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, code := range []string{"Success", "Failure"} {
			t.Run(fmt.Sprint(nested, code), func(t *testing.T) {
				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(completionBody(code, "test result", nested)))
				}))
				defer srv.Close()

				c := &Client{session: &bmcSession{httpClient: srv.Client(), baseURL: srv.URL}, deviceID: "normal"}

				operations := []func(context.Context) error{
					func(ctx context.Context) error { return c.Reset(ctx, ResetOn) },
					func(ctx context.Context) error { return c.SetBootOverride(ctx, BootTargetPxe, BootOnce) },
					c.DisableBootOverride,
					func(ctx context.Context) error { return c.SetHTTPBootOverride(ctx, "http://boot") },
					func(ctx context.Context) error { return c.SetBIOSHTTPBootURI(ctx, "http://boot") },
					func(ctx context.Context) error { _, err := c.PowerState(ctx); return err },
					func(ctx context.Context) error { _, err := c.GetBootConfig(ctx); return err },
					func(ctx context.Context) error { _, err := c.GetBIOSHTTPBootURI(ctx); return err },
				}
				for _, op := range operations {
					err := op(t.Context())
					if code == "Failure" {
						require.ErrorContains(t, err, "Failure")
					} else {
						require.NoError(t, err)
					}
				}
			})
		}
	}
}

func TestRackManagerReadGraceAndSlotIsolation(t *testing.T) {
	var (
		acCalls sync.Map
		rackOn  atomic.Bool
	)

	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case rackManagerPath + "/SystemPower":
			state := "Off"
			if rackOn.Load() {
				state = "On"
			}

			_, _ = fmt.Fprintf(w, `{"System Power States":{"7":%q,"8":%q}}`, state, state)
		case rackPowerOnPath:
			var body map[string]string
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			_, loaded := acCalls.LoadOrStore(body["system_id"], true)
			assert.False(t, loaded)
			w.WriteHeader(204)
		default:
			w.WriteHeader(503)
		}
	})
	first := dialRack(t, srv, "7")
	second := dialRack(t, srv, "8")
	now := time.Unix(100, 0)
	first.rack.now = func() time.Time { return now }
	second.rack.now = func() time.Time { return now }

	require.ErrorIs(t, first.DisableBootOverride(t.Context()), ErrNotReady)
	require.Equal(t, recoveryNone, second.rack.pending)
	require.ErrorIs(t, second.Reset(t.Context(), ResetOn), ErrNotReady)
	rackOn.Store(true)

	for _, c := range []*Client{first, second} {
		state, err := c.PowerState(t.Context())
		require.Empty(t, state)
		require.ErrorIs(t, err, ErrNotReady)
		_, err = c.GetBootConfig(t.Context())
		require.ErrorIs(t, err, ErrNotReady)
	}

	now = now.Add(rackRecoveryGrace)

	for _, c := range []*Client{first, second} {
		_, err := c.PowerState(t.Context())
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNotReady)
		_, err = c.GetBootConfig(t.Context())
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNotReady)
	}
}

func TestRackManagerUnrelatedMutationDoesNotEndGrace(t *testing.T) {
	var off atomic.Bool
	off.Store(true)

	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case rackManagerPath + "/SystemPower":
			state := "On"
			if off.Load() {
				state = "Off"
			}

			_, _ = fmt.Fprintf(w, `{"System Power States":{"7":%q}}`, state)
		case rackPowerOnPath:
			off.Store(false)
			w.WriteHeader(204)
		case "/7" + rackSystemPath + "/Actions/ComputerSystem.Reset":
			w.WriteHeader(204)
		default:
			w.WriteHeader(503)
		}
	})
	c := dialRack(t, srv, "7")
	require.ErrorIs(t, c.DisableBootOverride(t.Context()), ErrNotReady)
	require.NoError(t, c.Reset(t.Context(), ResetOn))
	require.ErrorIs(t, c.DisableBootOverride(t.Context()), ErrNotReady)
}

func TestRackManagerPartialAvailabilityKeepsFixedGrace(t *testing.T) {
	for _, initial := range []string{"boot", "on"} {
		t.Run(initial, func(t *testing.T) {
			var (
				acCalls    atomic.Int64
				bootReady  atomic.Bool
				onReady    atomic.Bool
				readsReady atomic.Bool
				rackOff    atomic.Bool
			)

			rackOff.Store(true)

			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case rackManagerPath + "/SystemPower":
					state := "On"
					if rackOff.Load() {
						state = "Off"
					}

					_, _ = fmt.Fprintf(w, `{"System Power States":{"7":%q}}`, state)
				case rackPowerOnPath:
					acCalls.Add(1)
					rackOff.Store(false)
					w.WriteHeader(http.StatusNoContent)
				case "/7" + rackSystemPath:
					if r.Method == http.MethodPatch && bootReady.Load() {
						w.WriteHeader(http.StatusNoContent)
						return
					}

					if r.Method == http.MethodGet && readsReady.Load() {
						_, _ = w.Write([]byte(`{"PowerState":"Off","Boot":{"BootSourceOverrideTarget":"Pxe"}}`))
						return
					}

					w.WriteHeader(http.StatusServiceUnavailable)
				case "/7" + rackSystemPath + "/Actions/ComputerSystem.Reset":
					var body map[string]ResetType
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))

					if body["ResetType"] == ResetForceOff || onReady.Load() {
						w.WriteHeader(http.StatusNoContent)
						return
					}

					w.WriteHeader(http.StatusServiceUnavailable)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			})
			c := dialRack(t, srv, "7")
			now := time.Unix(100, 0)
			c.rack.now = func() time.Time { return now }
			operation := func() error {
				if initial == "boot" {
					return c.DisableBootOverride(t.Context())
				}

				return c.Reset(t.Context(), ResetOn)
			}
			require.ErrorIs(t, operation(), ErrNotReady)

			deadline := c.rack.deadline

			if initial == "boot" {
				bootReady.Store(true)
				require.NoError(t, operation())
			} else {
				require.NoError(t, c.Reset(t.Context(), ResetForceOff))
				require.ErrorIs(t, operation(), ErrNotReady)
			}

			_, err := c.PowerState(t.Context())
			require.ErrorIs(t, err, ErrNotReady)
			_, err = c.GetBootConfig(t.Context())
			require.ErrorIs(t, err, ErrNotReady)
			require.Equal(t, deadline, c.rack.deadline)
			require.NotEqual(t, recoveryNone, c.rack.pending)

			// Even healthy reads during grace do not rearm repeated AC requests.
			readsReady.Store(true)

			_, err = c.PowerState(t.Context())
			require.NoError(t, err)
			readsReady.Store(false)
			bootReady.Store(false)
			rackOff.Store(true)

			now = deadline.Add(-time.Second)

			require.ErrorIs(t, operation(), ErrNotReady)
			require.Equal(t, int64(1), acCalls.Load())
			require.Equal(t, deadline, c.rack.deadline)
			now = deadline
			err = operation()
			require.Error(t, err)

			if initial == "boot" {
				require.ErrorIs(t, err, ErrNotReady)
				require.Equal(t, int64(2), acCalls.Load())
			} else {
				require.NotErrorIs(t, err, ErrNotReady)
				require.Equal(t, int64(1), acCalls.Load())
			}

			require.NoError(t, c.Reset(t.Context(), ResetForceOff))
			require.NotEqual(t, recoveryNone, c.rack.pending)
			bootReady.Store(true)
			onReady.Store(true)
			require.NoError(t, operation())

			if initial == "boot" {
				require.NotEqual(t, recoveryNone, c.rack.pending)
				now = c.rack.deadline

				require.NoError(t, operation())
			}

			require.Equal(t, recoveryNone, c.rack.pending)
		})
	}
}

func TestCompletionCheckerConservative(t *testing.T) {
	for _, body := range []string{
		"", "not json", `{}`, `{"error":{"message":"legacy"}}`,
		`{"CompletionCode":"Failure"}`, `{"Other":{"CompletionCode":"Failure"}}`,
		completionBody("Success", "Failure is just text", true),
	} {
		require.NoError(t, checkCompletion([]byte(body)), body)
	}

	for _, nested := range []bool{true, false} {
		err := checkCompletion([]byte(completionBody("Failure", "diagnostic", nested)))
		require.ErrorContains(t, err, "diagnostic")
	}

	require.Error(t, checkCompletion([]byte(`{"@Message.ExtendedInfo":[{"Oem":{"Vendor":{"CompletionCode":"Success"}}},{"Oem":{"Other":{"CompletionCode":"Failure"}}}]}`)))
}

func TestRackManagerRecoveryRequiresMatchingOperation(t *testing.T) {
	for _, initial := range []recoveryOp{recoveryResetOn, recoveryClearOverride} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			var ready bool

			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case rackManagerPath + "/SystemPower":
					_, _ = w.Write([]byte(`{"System Power States":{"7":"Off"}}`))
				case rackPowerOnPath:
					w.WriteHeader(http.StatusNoContent)
				default:
					if ready {
						w.WriteHeader(http.StatusNoContent)
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
				}
			})
			c := dialRack(t, srv, "7")
			now := time.Unix(100, 0)
			c.rack.now = func() time.Time { return now }
			matching := func() error { return c.Reset(t.Context(), ResetOn) }

			other := func() error { return c.DisableBootOverride(t.Context()) }
			if initial == recoveryClearOverride {
				matching, other = other, matching
			}

			require.ErrorIs(t, matching(), ErrNotReady)
			require.Equal(t, initial, c.rack.pending)

			ready = true

			require.NoError(t, other())
			require.False(t, c.rack.pendingCompleted)
			now = c.rack.deadline

			require.NoError(t, other())
			require.Equal(t, initial, c.rack.pending)
			require.False(t, c.rack.pendingCompleted)
			require.NoError(t, matching())
			require.Equal(t, recoveryNone, c.rack.pending)
		})
	}
}

func TestSessionCreationChecksCompletion(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Auth-Token", "must-not-use")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(completionBody("Failure", "session failed", true)))
	}))
	defer srv.Close()

	token, _, err := createSession(t.Context(), srv.Client(), srv.URL, "admin", "secret")
	require.Empty(t, token)
	require.ErrorContains(t, err, "session failed")
}
