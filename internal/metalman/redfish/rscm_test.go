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

const rackIdentity = `{"Id":"RackManager","@odata.id":"/redfish/v1/Managers/RackManager","Oem":{"Microsoft":{"@odata.type":"#OcsRackManager.v1_8_0.RackManager"}}}`

func completionBody(code, description string, nested bool) string {
	message := map[string]any{"@Message.ExtendedInfo": []any{map[string]any{
		"Oem": map[string]any{"Microsoft": map[string]string{"CompletionCode": code, "Description": description}},
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
	require.NotNil(t, c.rscm)
	require.Same(t, c.session.httpClient, c.rscm.root.httpClient)
	require.Empty(t, c.session.token)
	require.Equal(t, slot, c.deviceID)
	t.Cleanup(c.Close)

	return c
}

func TestRackSCMRequests(t *testing.T) {
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

func TestRackSCMDetection(t *testing.T) {
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
		{"tooLarge", "/49", 200, rackIdentity, false, true},
		{"negative", "/-1", 200, rackIdentity, false, true},
		{"multipleSegments", "/7/8", 200, rackIdentity, false, true},
		{"name", "/blade", 200, rackIdentity, false, true},
		{"leadingZero", "/07", 200, rackIdentity, false, true},
		{"escaped", "/%37", 200, rackIdentity, false, true},
		{"query", "/7?x=1", 200, rackIdentity, false, true},
		{"arbitrary200", "/proxy", 200, `{}`, false, false},
		{"idOnly", "/proxy", 200, `{"Id":"RackManager"}`, false, false},
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

			require.Equal(t, tc.wantRack, c.rscm != nil)

			if !tc.wantRack {
				require.Equal(t, int64(1), sessions.Load())
				require.Equal(t, "normal", c.deviceID)
			}
		})
	}
}

func TestRackSCMNoPathDoesNotProbe(t *testing.T) {
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

			require.Nil(t, c.rscm)
			state, err := c.PowerState(t.Context())
			require.NoError(t, err)
			require.Equal(t, PowerOn, state)
		})
	}
}

func TestRackSCMDetectionFallsBackToWorkingPrefixedBMC(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, "root requires other credentials"},
		{"unavailable", http.StatusServiceUnavailable, "root unavailable"},
		{"html", http.StatusOK, "<html>Management proxy</html>"},
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

					_, _ = w.Write([]byte(`{"PowerState":"On"}`))
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

			c, err := Dial(t.Context(), srv.URL+"/proxy", tlsServerFingerprint(srv), "admin", "secret", "")
			require.NoError(t, err)

			defer c.Close()

			require.Nil(t, c.rscm)
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

func TestRackSCMDetectionPreservesCancellation(t *testing.T) {
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

func TestRackSCMDetectionProbeTimeoutFallsBack(t *testing.T) {
	var probes atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case rackManagerPath:
			probes.Add(1)
			<-r.Context().Done()
		case "/proxy/redfish/v1/Systems/normal":
			_, _ = w.Write([]byte(`{"PowerState":"On"}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	httpClient := newHTTPClient(tlsServerFingerprint(srv))

	httpClient.Timeout = 100 * time.Millisecond
	defer httpClient.CloseIdleConnections()

	session := &bmcSession{httpClient: httpClient, baseURL: srv.URL + "/proxy", user: "admin", pass: "secret"}
	rack, err := detectRackSCM(t.Context(), session)
	require.NoError(t, err)
	require.Nil(t, rack)
	require.NoError(t, t.Context().Err())
	require.Equal(t, int64(1), probes.Load())

	c := &Client{session: session, deviceID: "normal"}
	state, err := c.PowerState(t.Context())
	require.NoError(t, err)
	require.Equal(t, PowerOn, state)
}

func TestRackSCMPowerFallback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		bladeStatus int
		bladeBody   string
		rackBody    string
		want        PowerState
		fallback    bool
	}{
		{"off", 503, "", `{"System Power States":{"7":"Off"}}`, PowerOff, true},
		{"onIsNotHostOn", 503, "", `{"System Power States":{"7":"On"}}`, "", true},
		{"missing", 503, "", `{"System Power States":{"8":"Off"}}`, "", true},
		{"unknown", 503, "", `{"System Power States":{"7":"--"}}`, "", true},
		{"failureState", 503, "", `{"System Power States":{"7":"Failure"}}`, "", true},
		{"wrongType", 503, "", `{"System Power States":{"7":7}}`, "", true},
		{"malformedRack", 503, "", `{`, "", true},
		{"rackFailure", 503, "", completionBody("Failure", "", true), "", true},
		{"bladeFailure", 200, completionBody("Failure", "BMC unavailable", false), `{"System Power States":{"7":"Off"}}`, PowerOff, true},
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

func TestRackSCMRecoveryBounded(t *testing.T) {
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
		case rackManagerPath + "/Actions/Oem/Microsoft.SystemPowerOn":
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
	c.rscm.now = func() time.Time { return now }
	boot := func() error { return c.SetBootOverride(t.Context(), BootTargetPxe, BootContinuous) }
	require.ErrorIs(t, boot(), ErrNotReady)
	require.Equal(t, int64(1), acCalls.Load())

	deadline := c.rscm.deadline
	for range 3 {
		_, err := c.PowerState(t.Context())
		require.NoError(t, err)
		_, err = c.GetBootConfig(t.Context())
		require.NoError(t, err)
		require.ErrorIs(t, boot(), ErrNotReady)
	}

	require.Equal(t, deadline, c.rscm.deadline)
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
	require.False(t, c.rscm.attempted)
	bladeReady.Store(false)
	require.ErrorIs(t, boot(), ErrNotReady)
	require.Equal(t, int64(2), acCalls.Load())
}

func TestRackSCMRecoveryTwoOutagesOnCachedClient(t *testing.T) {
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
		case rackManagerPath + "/Actions/Oem/Microsoft.SystemPowerOn":
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
	c.rscm.now = func() time.Time { return now }
	boot := func() error { return c.SetBootOverride(t.Context(), BootTargetPxe, BootOnce) }
	require.ErrorIs(t, boot(), ErrNotReady)

	deadline := c.rscm.deadline
	now = now.Add(time.Minute)

	bladeReady.Store(true)
	require.NoError(t, boot())
	state, err := c.PowerState(t.Context())
	require.NoError(t, err)
	require.Equal(t, PowerOn, state)
	require.True(t, c.rscm.pendingCompleted)
	require.Equal(t, deadline, c.rscm.deadline)

	// No successful call occurs after the first window before the next outage.
	now = deadline.Add(time.Minute)

	bladeReady.Store(false)
	rackOff.Store(true)

	again, err := pool.Get(t.Context(), srv.URL+"/7", tlsServerFingerprint(srv), "admin", "secret", "")
	require.NoError(t, err)
	require.Same(t, c, again)
	require.ErrorIs(t, again.SetBootOverride(t.Context(), BootTargetPxe, BootOnce), ErrNotReady)
	require.Equal(t, int64(2), acCalls.Load())
	require.False(t, c.rscm.pendingCompleted)
	require.Equal(t, now.Add(rackRecoveryGrace), c.rscm.deadline)

	// The second, incomplete episode must not rearm just because its window ends.
	now = c.rscm.deadline

	rackOff.Store(true)

	for range 3 {
		err := boot()
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNotReady)

		now = now.Add(rackRecoveryGrace)
	}

	require.Equal(t, int64(2), acCalls.Load())
}

func TestRackSCMRecoveryOperationsAndConcurrency(t *testing.T) {
	for _, op := range []string{"boot", "disable", "on", "off", "read"} {
		t.Run(op, func(t *testing.T) {
			var acCalls atomic.Int64

			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case rackManagerPath + "/SystemPower":
					_, _ = w.Write([]byte(`{"System Power States":{"7":"Off"}}`))
				case rackManagerPath + "/Actions/Oem/Microsoft.SystemPowerOn":
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
				case "off":
					require.Error(t, err)
					require.NotErrorIs(t, err, ErrNotReady)
				case "read":
					require.NoError(t, err)
				default:
					require.ErrorIs(t, err, ErrNotReady)
				}
			}

			want := int64(1)
			if op == "read" || op == "off" {
				want = 0
			}

			require.Equal(t, want, acCalls.Load())
		})
	}
}

func TestRackSCMRecoveryDoesNotHideErrors(t *testing.T) {
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
		{"unknownFailure", 200, completionBody("Failure", "Unexpected hardware error", true)},
		{"genericError", 200, `{"error":{"message":"no permission"}}`},
		{"malformedCompletion", 200, `{"@Message.ExtendedInfo":[{"Oem":{"Microsoft":{"CompletionCode":4}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/7"+rackSystemPath, r.URL.Path)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			c := dialRack(t, srv, "7")
			c.rscm.attempted = true
			c.rscm.deadline = time.Now().Add(time.Minute)
			err := c.DisableBootOverride(t.Context())
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrNotReady)
		})
	}
}

func TestRackSCMACFailureIsLatched(t *testing.T) {
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
				case rackManagerPath + "/Actions/Oem/Microsoft.SystemPowerOn":
					acCalls.Add(1)
					w.WriteHeader(response.status)
					_, _ = w.Write([]byte(response.body))
				default:
					w.WriteHeader(503)
				}
			})

			c := dialRack(t, srv, "7")
			now := time.Unix(100, 0)

			c.rscm.now = func() time.Time { return now }
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

func TestRackSCMPoolSlotsAndPinning(t *testing.T) {
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

func TestRackSCMReadGraceAndSlotIsolation(t *testing.T) {
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
		case rackManagerPath + "/Actions/Oem/Microsoft.SystemPowerOn":
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
	first.rscm.now = func() time.Time { return now }
	second.rscm.now = func() time.Time { return now }

	require.ErrorIs(t, first.DisableBootOverride(t.Context()), ErrNotReady)
	require.False(t, second.rscm.attempted)
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

func TestRackSCMUnrelatedMutationDoesNotEndGrace(t *testing.T) {
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
		case rackManagerPath + "/Actions/Oem/Microsoft.SystemPowerOn":
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

func TestRackSCMPartialAvailabilityKeepsFixedGrace(t *testing.T) {
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
				case rackManagerPath + "/Actions/Oem/Microsoft.SystemPowerOn":
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
			c.rscm.now = func() time.Time { return now }
			operation := func() error {
				if initial == "boot" {
					return c.SetBootOverride(t.Context(), BootTargetPxe, BootOnce)
				}

				return c.Reset(t.Context(), ResetOn)
			}
			require.ErrorIs(t, operation(), ErrNotReady)

			deadline := c.rscm.deadline

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
			require.Equal(t, deadline, c.rscm.deadline)
			require.True(t, c.rscm.attempted)

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
			require.Equal(t, deadline, c.rscm.deadline)
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
			require.True(t, c.rscm.attempted)
			bootReady.Store(true)
			onReady.Store(true)
			require.NoError(t, operation())

			if initial == "boot" {
				require.True(t, c.rscm.attempted)
				now = c.rscm.deadline

				require.NoError(t, operation())
			}

			require.False(t, c.rscm.attempted)
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

	require.Error(t, checkCompletion([]byte(`{"@Message.ExtendedInfo":[{"Oem":{"Microsoft":{"CompletionCode":"Success"}}},{"Oem":{"Microsoft":{"CompletionCode":"Failure"}}}]}`)))
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
