// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/metalman/bootmedia"
)

var testMedia = bootmedia.Source{Name: "ub-0123456789abcdef.iso", Server: "192.0.2.10"}

type mediaRequest struct {
	method string
	path   string
	body   string
}

func TestVirtualMediaRequests(t *testing.T) {
	want := []mediaRequest{
		{http.MethodPost, rackTftpPath + "Get", `{"Server":"192.0.2.10","File":"up-0123456789abcdef.iso"}`},
		{http.MethodPost, rackTftpPath + "Get", `{"Server":"192.0.2.10","File":"ub-0123456789abcdef.iso"}`},
		{http.MethodPost, "/7" + remoteDrivePath + "Mount", `{"ImageName":"up-0123456789abcdef.iso"}`},
		{http.MethodPost, "/7" + remoteDrivePath + "Mount", `{"ImageName":"ub-0123456789abcdef.iso"}`},
		{http.MethodPatch, "/7" + rackSystemPath, `{"Boot":{"BootSourceOverrideMode":"UEFI"}}`},
		{http.MethodPatch, "/7" + rackSystemPath, `{"Boot":{"BootSourceOverrideTarget":"RemoteDrive"}}`},
		{http.MethodPatch, "/7" + rackSystemPath, `{"Boot":{"BootSourceOverrideTarget":"Hdd"}}`},
		{http.MethodPost, "/7" + remoteDrivePath + "Unmount", ""},
		{http.MethodPost, rackTftpPath + "Delete", `{"File":"up-0123456789abcdef.iso"}`},
		{http.MethodPost, rackTftpPath + "Delete", `{"File":"ub-0123456789abcdef.iso"}`},
	}

	var seen int

	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !assert.Less(t, seen, len(want)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		expected := want[seen]
		seen++

		assert.Equal(t, expected.method, r.Method)
		assert.Equal(t, expected.path, r.URL.Path)
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)

		if expected.body == "" {
			assert.Empty(t, body)
			assert.Empty(t, r.Header.Get("Content-Type"))
		} else {
			assert.JSONEq(t, expected.body, string(body))
		}

		_, _ = w.Write([]byte(completionBody("Success", "", true)))
	})
	c := dialRack(t, srv, "7")
	require.True(t, c.SupportsVirtualMedia())
	require.NoError(t, c.StageVirtualMedia(t.Context(), testMedia))
	require.NoError(t, c.MountVirtualMedia(t.Context(), testMedia))
	require.NoError(t, c.SetVirtualMediaBoot(t.Context()))
	require.NoError(t, c.CleanupVirtualMedia(t.Context(), testMedia))
	require.Equal(t, len(want)-2, seen)
	require.NoError(t, c.DeleteVirtualMedia(t.Context(), testMedia))
	require.Equal(t, len(want), seen)
}

func TestVirtualMediaPrimerMountFailureStops(t *testing.T) {
	var mounts []string

	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)

		mounts = append(mounts, string(body))

		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(completionBody("Failure", "Image not found: up-0123456789abcdef.iso", true)))
	})
	c := dialRack(t, srv, "7")

	require.Error(t, c.MountVirtualMedia(t.Context(), testMedia))
	require.Equal(t, []string{`{"ImageName":"up-0123456789abcdef.iso"}`}, mounts)
}

func TestVirtualMediaUnsupported(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	}))
	defer srv.Close()

	c := &Client{session: &bmcSession{httpClient: srv.Client(), baseURL: srv.URL}}
	require.False(t, c.SupportsVirtualMedia())
	require.ErrorIs(t, c.StageVirtualMedia(t.Context(), testMedia), ErrUnsupported)
	require.ErrorIs(t, c.MountVirtualMedia(t.Context(), testMedia), ErrUnsupported)
	require.ErrorIs(t, c.SetVirtualMediaBoot(t.Context()), ErrUnsupported)
	require.ErrorIs(t, c.CleanupVirtualMedia(t.Context(), testMedia), ErrUnsupported)
	require.ErrorIs(t, c.DeleteVirtualMedia(t.Context(), testMedia), ErrUnsupported)
}

func TestVirtualMediaValidation(t *testing.T) {
	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	})
	c := dialRack(t, srv, "7")

	var invalid []bootmedia.Source
	for _, name := range []string{"", "other.iso", "ub-.iso", "ub-XYZ.iso", "ub-aa.ISO", "../ub-aa.iso", "/ub-aa.iso", "ub-aa.iso/other", "ub-aa.iso?x", "ub-aa.iso\n", "ub-" + strings.Repeat("a", 33) + ".iso"} {
		invalid = append(invalid, bootmedia.Source{Name: name, Server: testMedia.Server})
	}

	for _, server := range []string{"", "tftp.example.com", "http://192.0.2.10", "192.0.2.10:69", "::1", "::ffff:192.0.2.10", "192.0.2.10\n"} {
		invalid = append(invalid, bootmedia.Source{Name: testMedia.Name, Server: server})
	}

	for _, source := range invalid {
		require.Error(t, c.StageVirtualMedia(t.Context(), source), "%+v", source)
		require.Error(t, c.MountVirtualMedia(t.Context(), source), "%+v", source)
		require.Error(t, c.CleanupVirtualMedia(t.Context(), source), "%+v", source)
		require.Error(t, c.DeleteVirtualMedia(t.Context(), source), "%+v", source)
	}

	for _, length := range []int{1, 32} {
		require.NoError(t, validateVirtualMedia(bootmedia.Source{
			Name: "ub-" + strings.Repeat("a", length) + ".iso", Server: testMedia.Server,
		}))
	}
}

func TestVirtualMediaErrors(t *testing.T) {
	for _, operation := range []string{"stage", "mount", "mode", "target", "restore", "unmount", "delete"} {
		for _, response := range []struct {
			name   string
			status int
			body   string
		}{
			{"denied", 403, ""},
			{"failure", 200, completionBody("Failure", "permission denied", false)},
			{"nestedFailure", 200, completionBody("Failure", "invalid image", true)},
			{"malformed", 200, "{"},
			{"null", 200, "null"},
			{"error", 200, `{"error":{"message":"bad request"}}`},
		} {
			t.Run(operation+"/"+response.name, func(t *testing.T) {
				var paths []string

				srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
					paths = append(paths, r.URL.Path)
					body, _ := io.ReadAll(r.Body)

					fail := (operation == "stage" && r.URL.Path == rackTftpPath+"Get") ||
						(operation == "mount" && strings.HasSuffix(r.URL.Path, "RemoteDriveMount")) ||
						(operation == "mode" && strings.Contains(string(body), "UEFI")) ||
						(operation == "target" && strings.Contains(string(body), "RemoteDrive")) ||
						(operation == "restore" && strings.Contains(string(body), "Hdd")) ||
						(operation == "unmount" && strings.HasSuffix(r.URL.Path, "Unmount")) ||
						(operation == "delete" && r.URL.Path == rackTftpPath+"Delete")
					if fail {
						w.WriteHeader(response.status)
						_, _ = w.Write([]byte(response.body))

						return
					}

					w.WriteHeader(http.StatusNoContent)
				})
				c := dialRack(t, srv, "7")

				var err error

				switch operation {
				case "stage":
					err = c.StageVirtualMedia(t.Context(), testMedia)
				case "mount":
					err = c.MountVirtualMedia(t.Context(), testMedia)
				case "mode", "target":
					err = c.SetVirtualMediaBoot(t.Context())
				case "delete":
					err = c.DeleteVirtualMedia(t.Context(), testMedia)
				default:
					err = c.CleanupVirtualMedia(t.Context(), testMedia)
				}

				require.Error(t, err)
				require.NotErrorIs(t, err, ErrNotReady)

				expectedCount := map[string]int{"stage": 1, "mount": 1, "mode": 1, "target": 2, "restore": 1, "unmount": 2, "delete": 1}
				require.Len(t, paths, expectedCount[operation])
			})
		}
	}
}

func TestVirtualMediaRecovery(t *testing.T) {
	for _, operation := range []string{"mount", "mode", "target", "pxe", "hdd"} {
		t.Run(operation, func(t *testing.T) {
			var (
				ready    bool
				restores int
			)

			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case rackManagerPath + "/SystemPower":
					_, _ = w.Write([]byte(`{"System Power States":{"7":"Off"}}`))
				case rackPowerOnPath:
					restores++

					w.WriteHeader(http.StatusNoContent)
				case "/7" + rackSystemPath + "/Actions/ComputerSystem.Reset":
					if !ready {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}

					w.WriteHeader(http.StatusNoContent)
				default:
					body, _ := io.ReadAll(r.Body)
					if !ready && (operation != "target" || strings.Contains(string(body), "RemoteDrive")) {
						_, _ = w.Write([]byte(completionBody("Failure", "blade powered off", true)))
						return
					}

					w.WriteHeader(http.StatusNoContent)
				}
			})
			c := dialRack(t, srv, "7")
			now := time.Now()
			c.rack.now = func() time.Time { return now }

			invoke := func() error {
				if operation == "mount" {
					return c.MountVirtualMedia(t.Context(), testMedia)
				}

				if operation == "pxe" {
					return c.SetBootOverride(t.Context(), BootTargetPxe, BootOnce)
				}

				if operation == "hdd" {
					return c.SetBootOverride(t.Context(), BootTargetHdd, BootContinuous)
				}

				return c.SetVirtualMediaBoot(t.Context())
			}
			for range 2 {
				err := invoke()
				require.ErrorContains(t, err, "blade powered off")
				require.NotErrorIs(t, err, ErrNotReady)
			}

			require.Zero(t, restores)
			require.False(t, c.rack.attempted)
			require.True(t, c.rack.deadline.IsZero())
			require.Empty(t, c.rack.pending)
			require.ErrorIs(t, c.Reset(t.Context(), ResetOn), ErrNotReady)
			deadline := c.rack.deadline

			require.ErrorIs(t, invoke(), ErrNotReady)
			require.ErrorIs(t, invoke(), ErrNotReady)
			require.Equal(t, 1, restores)
			require.Equal(t, deadline, c.rack.deadline)

			now = now.Add(rackRecoveryGrace + time.Second)

			err := invoke()
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrNotReady)
			require.Equal(t, 1, restores)

			ready = true

			require.NoError(t, invoke())
			require.True(t, c.rack.attempted)
			require.NoError(t, c.Reset(t.Context(), ResetOn))
			require.False(t, c.rack.attempted)
		})
	}
}

func TestVirtualMediaCleanupUnavailable(t *testing.T) {
	for _, failUnmount := range []bool{false, true} {
		for _, power := range []string{"Off", "On", "invalid"} {
			t.Run(power+"/unmount="+map[bool]string{true: "true", false: "false"}[failUnmount], func(t *testing.T) {
				var paths []string

				srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
					paths = append(paths, r.URL.Path)
					switch r.URL.Path {
					case "/7" + rackSystemPath:
						if !failUnmount {
							w.WriteHeader(http.StatusServiceUnavailable)
						}
					case "/7" + remoteDrivePath + "Unmount":
						w.WriteHeader(http.StatusServiceUnavailable)
					case rackManagerPath + "/SystemPower":
						_, _ = w.Write([]byte(`{"System Power States":{"7":"` + power + `"}}`))
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
					}
				})
				c := dialRack(t, srv, "7")

				err := c.CleanupVirtualMedia(t.Context(), testMedia)
				require.Error(t, err)

				want := []string{"/7" + rackSystemPath}
				if failUnmount {
					want = append(want, "/7"+remoteDrivePath+"Unmount")
				}

				require.Equal(t, want, paths)
				require.NotContains(t, paths, rackTftpPath+"Delete")
			})
		}
	}
}

func TestVirtualMediaCleanupAbsence(t *testing.T) {
	for _, description := range []string{"no remote drive mounted", "no media mounted", "remote drive is not mounted", "permission denied"} {
		t.Run(description, func(t *testing.T) {
			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/7" + rackSystemPath:
					w.WriteHeader(http.StatusNoContent)
				case "/7" + remoteDrivePath + "Unmount":
					_, _ = w.Write([]byte(completionBody("Failure", description, true)))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			})
			c := dialRack(t, srv, "7")

			err := c.CleanupVirtualMedia(t.Context(), testMedia)
			require.Error(t, err)
		})
	}
}

func TestVirtualMediaCleanupNothingMounted(t *testing.T) {
	var unmounts int

	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/7" + rackSystemPath:
			w.WriteHeader(http.StatusNoContent)
		case "/7" + remoteDrivePath + "Unmount":
			unmounts++

			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(completionBody("Failure", "Error unmounting remote media: Remote media support is not enabled", true)))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})
	c := dialRack(t, srv, "7")

	require.NoError(t, c.CleanupVirtualMedia(t.Context(), testMedia))
	require.Equal(t, 1, unmounts)
}

func TestVirtualMediaCanceled(t *testing.T) {
	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	})
	c := dialRack(t, srv, "7")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, c.StageVirtualMedia(ctx, testMedia), context.Canceled)
	require.ErrorIs(t, c.MountVirtualMedia(ctx, testMedia), context.Canceled)
	require.ErrorIs(t, c.SetVirtualMediaBoot(ctx), context.Canceled)
	require.ErrorIs(t, c.CleanupVirtualMedia(ctx, testMedia), context.Canceled)
	require.ErrorIs(t, c.DeleteVirtualMedia(ctx, testMedia), context.Canceled)
}

func TestVirtualMediaCleanupAndDeleteRetries(t *testing.T) {
	for _, operation := range []string{"cleanup", "delete"} {
		for _, retry := range []struct {
			name      string
			status    int
			body      string
			wantError bool
		}{
			{"success", http.StatusNoContent, "", false},
			{"completionSuccess", http.StatusOK, completionBody("Success", "", true), false},
			{"alreadyDeleted", http.StatusInternalServerError, completionBody("Failure", "No such file or directory", true), operation != "delete"},
			{"alreadyDeletedMixedCase", http.StatusInternalServerError, completionBody("Failure", "Error: NO SUCH FILE OR DIRECTORY: ub-aa.iso", true), operation != "delete"},
			{"absenceWithoutCompletion", http.StatusInternalServerError, "No such file or directory", true},
			{"missingAction", http.StatusNotFound, "", true},
			{"denied", http.StatusForbidden, "", true},
			{"fileAbsent", http.StatusOK, completionBody("Failure", "file not found", true), true},
			{"fileDoesNotExist", http.StatusNotFound, completionBody("Failure", "file does not exist", true), true},
			{"notMounted", http.StatusOK, completionBody("Failure", "no remote drive mounted", true), true},
		} {
			for _, lostResponse := range []bool{false, true} {
				t.Run(operation+"/"+retry.name+"/lost="+map[bool]string{false: "false", true: "true"}[lostResponse], func(t *testing.T) {
					var (
						paths    []string
						attempts int
					)

					srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
						paths = append(paths, r.Method+" "+r.URL.Path)
						if r.URL.Path == rackManagerPath+"/SystemPower" {
							_, _ = w.Write([]byte(`{"System Power States":{"7":"On"}}`))
							return
						}

						if r.Method == http.MethodPatch {
							body, err := io.ReadAll(r.Body)
							assert.NoError(t, err)
							assert.JSONEq(t, `{"Boot":{"BootSourceOverrideTarget":"Hdd"}}`, string(body))
							w.WriteHeader(http.StatusNoContent)

							return
						}

						if r.URL.Path == rackTftpPath+"Delete" {
							body, err := io.ReadAll(r.Body)
							assert.NoError(t, err)

							// The primer file is deleted first and always succeeds here.
							if strings.Contains(string(body), "up-") {
								w.WriteHeader(http.StatusNoContent)
								return
							}
						}

						attempts++
						if attempts == 1 {
							if lostResponse {
								w.Header().Set("Content-Length", "100")
								_, _ = w.Write([]byte("{"))

								return
							}

							w.WriteHeader(http.StatusNoContent)

							return
						}

						w.WriteHeader(retry.status)
						_, _ = w.Write([]byte(retry.body))
					})
					c := dialRack(t, srv, "7")
					invoke := c.CleanupVirtualMedia
					want := []string{"PATCH /7" + rackSystemPath, "POST /7" + remoteDrivePath + "Unmount"}

					if operation == "delete" {
						invoke = c.DeleteVirtualMedia
						want = []string{"POST " + rackTftpPath + "Delete", "POST " + rackTftpPath + "Delete"}
					}

					err := invoke(t.Context(), testMedia)
					if lostResponse {
						require.ErrorIs(t, err, io.ErrUnexpectedEOF)
					} else {
						require.NoError(t, err)
					}

					err = invoke(t.Context(), testMedia)
					if retry.wantError {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}

					want = append(want, want...)

					require.Equal(t, want, paths)
				})
			}
		}
	}
}

func TestVirtualMediaStageNeverRestoresPower(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests int

			srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests++

				assert.Equal(t, rackTftpPath+"Get", r.URL.Path)
				w.WriteHeader(status)
			})
			c := dialRack(t, srv, "7")
			require.Error(t, c.StageVirtualMedia(t.Context(), testMedia))
			require.Equal(t, 1, requests)
		})
	}
}

func TestVirtualMediaRecoveryFailure(t *testing.T) {
	var restores int

	srv := rackServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/7" + remoteDrivePath + "Mount", "/7" + rackSystemPath + "/Actions/ComputerSystem.Reset":
			w.WriteHeader(http.StatusServiceUnavailable)
		case rackManagerPath + "/SystemPower":
			_, _ = w.Write([]byte(`{"System Power States":{"7":"Off"}}`))
		case rackPowerOnPath:
			restores++

			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	c := dialRack(t, srv, "7")
	require.ErrorContains(t, c.Reset(t.Context(), ResetOn), "restoring AC power")

	for range 2 {
		err := c.MountVirtualMedia(t.Context(), testMedia)
		require.ErrorContains(t, err, "restoring AC power")
		require.NotErrorIs(t, err, ErrNotReady)
	}

	require.Equal(t, 1, restores)
}
