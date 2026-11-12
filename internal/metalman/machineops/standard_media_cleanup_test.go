// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/redfish"
)

type standardMediaDiscoveryFailure struct {
	name   string
	path   string
	status int
	body   string
}

func standardMediaDiscoveryFailures() []standardMediaDiscoveryFailure {
	var failures []standardMediaDiscoveryFailure

	for _, path := range []string{"/redfish/v1/Managers/1", "/redfish/v1/Managers/1/VirtualMedia"} {
		for _, status := range []int{404, 405, 410, 501, 403} {
			failures = append(failures, standardMediaDiscoveryFailure{
				name: fmt.Sprintf("%s/%d", path, status), path: path, status: status,
			})
		}
	}

	return append(failures,
		standardMediaDiscoveryFailure{
			name: "license", path: "/redfish/v1/Managers/1/VirtualMedia", status: http.StatusForbidden,
			body: `{"error":{"code":"SMC.1.0.OemLicenseNotPassed"}}`,
		},
		standardMediaDiscoveryFailure{name: "unknown image"},
	)
}

func standardMediaCleanupClient(t *testing.T, failure standardMediaDiscoveryFailure) (*redfish.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()

	var discovery, mutations atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/redfish/v1/SessionService/Sessions" {
			http.NotFound(w, req)
			return
		}

		if req.Method != http.MethodGet {
			mutations.Add(1)
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		if req.URL.Path == failure.path {
			discovery.Add(1)
			w.WriteHeader(failure.status)
			_, _ = w.Write([]byte(failure.body))

			return
		}

		var body any

		switch req.URL.Path {
		case "/redfish/v1/Systems/1":
			body = map[string]any{
				"PowerState": "On",
				"Boot":       map[string]any{"BootSourceOverrideTarget@Redfish.AllowableValues": []string{"Cd"}},
				"Links":      map[string]any{"ManagedBy": []map[string]string{{"@odata.id": "/redfish/v1/Managers/1"}}},
			}
		case "/redfish/v1/Managers/1":
			body = map[string]any{"VirtualMedia": map[string]string{"@odata.id": "/redfish/v1/Managers/1/VirtualMedia"}}
		case "/redfish/v1/Managers/1/VirtualMedia":
			body = map[string]any{"Members": []map[string]string{{"@odata.id": "/redfish/v1/Managers/1/VirtualMedia/1"}}}
		case "/redfish/v1/Managers/1/VirtualMedia/1":
			discovery.Add(1)
			// An operator's remote-console mount has no Image or eject action.
			body = map[string]any{
				"MediaTypes": []string{"CD"}, "Inserted": true,
				"Actions": map[string]any{"#VirtualMedia.InsertMedia": map[string]string{
					"target": "/redfish/v1/Managers/1/VirtualMedia/1/Actions/VirtualMedia.InsertMedia",
				}},
			}
		default:
			http.NotFound(w, req)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	bmc, err := redfish.Dial(t.Context(), srv.URL, "", "admin", "secret", "1")
	require.NoError(t, err)
	t.Cleanup(bmc.Close)
	require.True(t, bmc.VirtualMediaStagingReadOnly())

	return bmc, &discovery, &mutations
}

func TestStandardVirtualMediaStagingFailureReleases(t *testing.T) {
	for _, failure := range standardMediaDiscoveryFailures() {
		t.Run(failure.name, func(t *testing.T) {
			bmc, discovery, mutations := standardMediaCleanupClient(t, failure)
			r, d, _, op := mediaFixture(t)
			r.PowerClients = standardMediaTestClient{Client: bmc}
			r.MaxAttempts = 1
			mediaStep(t, r, op.Name)
			current := mediaStep(t, r, op.Name)
			require.Equal(t, "Staging", current.Status.Targets[0].BootMedia.Phase)

			for range r.maxAttempts() + 1 {
				current = mediaStep(t, r, op.Name)
				if current.Status.IsTerminal() {
					break
				}
			}

			require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)
			require.Equal(t, "Staging", current.Status.Targets[0].BootMedia.Phase)
			require.Positive(t, discovery.Load())
			before := discovery.Load()

			for range 4 {
				current = mediaStep(t, r, op.Name)
			}

			require.Empty(t, current.Finalizers)
			require.True(t, current.Status.Targets[0].BootMedia.Cleaned)
			require.True(t, current.Status.Targets[0].BootMedia.StagedDeleted)
			require.True(t, current.Status.Targets[0].BootMedia.Released)
			require.Equal(t, []string{"prepare", "release"}, d.calls)
			require.Equal(t, before, discovery.Load())
			require.Zero(t, mutations.Load())
		})
	}
}

func TestStandardVirtualMediaCleanupPhaseBoundary(t *testing.T) {
	for _, failure := range standardMediaDiscoveryFailures() {
		for _, phase := range []string{"Staging", "PoweringOff", "Mounting", "SettingBoot", "PoweringOn", "Installing", "Replacing"} {
			for _, ending := range []string{"terminal", "deleted", "failed target"} {
				t.Run(failure.name+"/"+phase+"/"+ending, func(t *testing.T) {
					bmc, discovery, mutations := standardMediaCleanupClient(t, failure)
					r, d, _, op := mediaFixture(t)

					var dials atomic.Int32

					r.PowerClients = standardMediaTestClient{Client: bmc, calls: &dials}
					mediaStep(t, r, op.Name)
					current := mediaStep(t, r, op.Name)

					current.Status.Targets[0].BootMedia.Phase = phase

					switch ending {
					case "terminal":
						current.Status.Phase = v1alpha3.OperationPhaseFailed
					case "failed target":
						current.Status.Targets[0].Phase = v1alpha3.OperationPhaseFailed
					}

					require.NoError(t, r.Status().Update(t.Context(), current))

					if ending == "deleted" {
						require.NoError(t, r.Delete(t.Context(), current))
					}

					dials.Store(0)
					r = testReconciler(r.Client, standardMediaTestClient{Client: bmc, calls: &dials}, r.Site)
					r.BootMedia = d
					request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}

					if phase != "Staging" && phase != "PoweringOff" {
						for range 2 {
							_, err := r.Reconcile(t.Context(), request)
							require.Error(t, err)
						}

						require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
						require.Contains(t, current.Finalizers, mediaFinalizer)
						require.False(t, current.Status.Targets[0].BootMedia.Cleaned)
						require.False(t, current.Status.Targets[0].BootMedia.Released)
						require.Equal(t, []string{"prepare"}, d.calls)
						require.Positive(t, discovery.Load())

						return
					}

					// A fresh reconciler must classify the BMC even on deletion.
					// It still dials, but does not repeat boot/media discovery.
					for range 4 {
						_, err := r.Reconcile(t.Context(), request)
						require.NoError(t, err)
					}

					err := r.Get(t.Context(), request.NamespacedName, current)
					if ending == "deleted" {
						require.True(t, apierrors.IsNotFound(err))
					} else {
						require.NoError(t, err)
						require.Empty(t, current.Finalizers)
						require.True(t, current.Status.Targets[0].BootMedia.Released)
					}

					// Cleanup and staged deletion each obtain a client. Release is local.
					require.EqualValues(t, 2, dials.Load())
					require.Zero(t, discovery.Load())
					require.Zero(t, mutations.Load())
					require.Equal(t, []string{"prepare", "release"}, d.calls)
				})
			}
		}
	}
}

func TestStandardVirtualMediaPreMountCleanupRequiresClient(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	mediaStep(t, r, op.Name)
	current := mediaStep(t, r, op.Name)
	require.NoError(t, r.Delete(t.Context(), current))
	r.PowerClients = standardMediaTestClient{err: errors.New("dial failed")}
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)})
	require.ErrorContains(t, err, "dial failed")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), current))
	require.Contains(t, current.Finalizers, mediaFinalizer)
	require.False(t, current.Status.Targets[0].BootMedia.Cleaned)
	require.Equal(t, []string{"prepare"}, d.calls)
}

func TestStandardVirtualMediaCleanupRetainsPendingInsertion(t *testing.T) {
	for _, ending := range []string{"terminal", "deleted", "failed target"} {
		t.Run(ending, func(t *testing.T) {
			r, d, _, op := mediaFixture(t)
			mediaStep(t, r, op.Name)
			current := mediaStep(t, r, op.Name)
			image, err := d.SourceURL(current.Status.Targets[0].BootMedia.ID)
			require.NoError(t, err)
			// The BMC may still have the URL from before a serve-host change.
			image = strings.Replace(image, "10.0.0.1", "10.0.0.99", 1)

			var (
				state, ejects atomic.Int32
				pendingEject  atomic.Bool
			)
			pendingEject.Store(true)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body any

				switch req.URL.Path {
				case "/redfish/v1/Systems/1":
					body = map[string]any{
						"Boot":         map[string]string{"BootSourceOverrideTarget": "None", "BootSourceOverrideEnabled": "Disabled"},
						"VirtualMedia": map[string]string{"@odata.id": "/redfish/v1/Systems/1/VirtualMedia"},
					}
				case "/redfish/v1/Systems/1/VirtualMedia":
					body = map[string]any{"Members": []map[string]string{{"@odata.id": "/redfish/v1/Systems/1/VirtualMedia/1"}}}
				case "/redfish/v1/Systems/1/VirtualMedia/1":
					body = map[string]any{
						"MediaTypes": []string{"CD"}, "Image": image, "Inserted": state.Load() == 1,
						"Actions": map[string]any{"#VirtualMedia.EjectMedia": map[string]string{
							"target": "/redfish/v1/Systems/1/VirtualMedia/1/Actions/VirtualMedia.EjectMedia",
						}},
					}
				case "/redfish/v1/Systems/1/VirtualMedia/1/Actions/VirtualMedia.EjectMedia":
					require.Equal(t, http.MethodPost, req.Method)
					ejects.Add(1)

					if pendingEject.Load() {
						w.WriteHeader(http.StatusAccepted)
						return
					}

					// Firmware retains the image URL after a completed detach.
					state.Store(2)
					w.WriteHeader(http.StatusNoContent)

					return
				default:
					http.NotFound(w, req)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			t.Cleanup(srv.Close)
			bmc, err := redfish.Dial(t.Context(), srv.URL, "", "admin", "secret", "1")
			require.NoError(t, err)
			t.Cleanup(bmc.Close)
			r.PowerClients = standardMediaTestClient{Client: bmc}
			current.Status.Targets[0].BootMedia.Phase = "Mounting"

			switch ending {
			case "terminal":
				current.Status.Phase = v1alpha3.OperationPhaseFailed
			case "failed target":
				current.Status.Targets[0].Phase = v1alpha3.OperationPhaseFailed
			}

			require.NoError(t, r.Status().Update(t.Context(), current))

			if ending == "deleted" {
				require.NoError(t, r.Delete(t.Context(), current))
			}

			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}

			for range 2 {
				_, err = r.Reconcile(t.Context(), request)
				require.ErrorContains(t, err, "insertion may be pending")
			}

			require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
			require.Contains(t, current.Finalizers, mediaFinalizer)
			require.False(t, current.Status.Targets[0].BootMedia.Cleaned)
			require.False(t, current.Status.Targets[0].BootMedia.StagedDeleted)
			require.False(t, current.Status.Targets[0].BootMedia.Released)
			require.Equal(t, []string{"prepare"}, d.calls)
			require.EqualValues(t, 2, ejects.Load())

			state.Store(1)
			pendingEject.Store(false)

			base, ok := r.Client.(client.WithWatch)
			require.True(t, ok)

			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					return errors.New("save Cleaned failed")
				},
			})
			_, err = r.Reconcile(t.Context(), request)
			require.ErrorContains(t, err, "save Cleaned failed")
			require.EqualValues(t, 3, ejects.Load())
			require.EqualValues(t, 2, state.Load())
			require.NoError(t, base.Get(t.Context(), request.NamespacedName, current))
			require.Contains(t, current.Finalizers, mediaFinalizer)
			require.False(t, current.Status.Targets[0].BootMedia.Cleaned)
			require.False(t, current.Status.Targets[0].BootMedia.StagedDeleted)
			require.False(t, current.Status.Targets[0].BootMedia.Released)
			require.Equal(t, []string{"prepare"}, d.calls)

			// Neither client nor controller remembers the successful eject.
			bmc.Close()
			bmc, err = redfish.Dial(t.Context(), srv.URL, "", "admin", "secret", "1")
			require.NoError(t, err)
			t.Cleanup(bmc.Close)
			r = testReconciler(base, standardMediaTestClient{Client: bmc}, r.Site)
			r.BootMedia = d

			current = mediaStep(t, r, op.Name)
			require.True(t, current.Status.Targets[0].BootMedia.Cleaned)
			require.False(t, current.Status.Targets[0].BootMedia.Released)
			require.EqualValues(t, 4, ejects.Load())
			require.EqualValues(t, 2, state.Load())
			require.Equal(t, []string{"prepare"}, d.calls)
			current = mediaStep(t, r, op.Name)
			require.True(t, current.Status.Targets[0].BootMedia.StagedDeleted)
			require.False(t, current.Status.Targets[0].BootMedia.Released)
			current = mediaStep(t, r, op.Name)
			require.True(t, current.Status.Targets[0].BootMedia.Released)
			require.Equal(t, []string{"prepare", "release"}, d.calls)
		})
	}
}
