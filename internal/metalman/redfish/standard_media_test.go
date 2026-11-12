// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/metalman/bootmedia"
)

const testISOURL = "http://192.0.2.10:8880/bootmedia/0123456789abcdef0123456789abcdef/boot.iso"

var standardMedia = bootmedia.Source{Name: "ub-0123456789abcdef0123456789abcdef.iso", URL: testISOURL}

// fakeILO advertises manager-owned media and trailing-slash action targets.
type fakeILO struct {
	mu                                   sync.Mutex
	image                                string
	inserted                             bool
	powerState                           PowerState
	boot                                 map[string]string
	allowed                              []string
	noManager, systemMedia, requireMatch bool
	insertStatus, patchStatus            int
	errorBody, actionInfo, async         string
	etag, polls                          int
	mutations                            []string
	patches                              []map[string]string
	inserts                              []map[string]any
	ejects                               []string
	insertResult                         map[string]any
	ejectResult                          map[string]any
	missingHeader                        []string
	ignoreInsert, ignoreBoot             bool
	noInsert                             bool
	firstMedia                           map[string]any
	firstActionInfo                      string
	actionInfoReads                      int
	resourceStatuses                     map[string]int
	relativeLinks                        bool
	resourceReads                        []string
}

func newFakeILO(t *testing.T) (*fakeILO, *Client) {
	t.Helper()

	f := &fakeILO{powerState: PowerOff, boot: map[string]string{"BootSourceOverrideTarget": "None", "BootSourceOverrideEnabled": "Disabled", "BootSourceOverrideMode": "UEFI"}, allowed: []string{"None", "Cd", "Pxe", "Hdd"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		if r.Header.Get("OData-Version") != "4.0" {
			f.missingHeader = append(f.missingHeader, r.URL.Path)
		}

		if r.Method != http.MethodGet {
			f.mutations = append(f.mutations, r.Method+" "+r.URL.Path)
		} else {
			f.resourceReads = append(f.resourceReads, r.URL.Path)
		}

		path := strings.TrimRight(r.URL.Path, "/")
		write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }

		if status := f.resourceStatuses[path]; r.Method == http.MethodGet && status != 0 {
			w.WriteHeader(status)
			return
		}

		switch {
		case path == "/redfish/v1/Systems":
			write(map[string]any{"Members": []redfishLink{{ODataID: "/redfish/v1/Systems/1/"}}})
		case path == "/redfish/v1/Systems/1" && r.Method == http.MethodGet:
			f.etag++
			w.Header().Set("ETag", fmt.Sprintf(`W/"%d"`, f.etag))

			boot := map[string]any{}
			for k, v := range f.boot {
				boot[k] = v
			}

			boot["BootSourceOverrideTarget@Redfish.AllowableValues"] = f.allowed

			system := map[string]any{"PowerState": f.powerState, "Boot": boot, "Actions": map[string]any{"#ComputerSystem.Reset": map[string]any{"target": "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset/", "ResetType@Redfish.AllowableValues": []string{"On", "ForceOff"}}}}
			if f.relativeLinks {
				system["Actions"] = map[string]any{"#ComputerSystem.Reset": redfishAction{Target: "Actions/ComputerSystem.Reset/", ActionInfo: "ResetActionInfo/"}}
			}

			if !f.noManager {
				manager := "/redfish/v1/Managers/1/"
				if f.relativeLinks {
					manager = "../../Managers/1/"
				}

				system["Links"] = map[string]any{"ManagedBy": []redfishLink{{ODataID: manager}}}
			}

			if f.systemMedia {
				media := "/redfish/v1/Systems/1/VirtualMedia/"
				if f.relativeLinks {
					media = "VirtualMedia/"
				}

				system["VirtualMedia"] = redfishLink{ODataID: media}
			}

			write(system)
		case path == "/redfish/v1/Systems/1" && r.Method == http.MethodPatch:
			if f.requireMatch && r.Header.Get("If-Match") != fmt.Sprintf(`W/"%d"`, f.etag) {
				w.WriteHeader(http.StatusPreconditionRequired)
				return
			}

			if f.patchStatus != 0 {
				http.Error(w, f.errorBody, f.patchStatus)
				return
			}

			var body struct{ Boot map[string]string }
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

			f.patches = append(f.patches, body.Boot)
			if f.ignoreBoot {
				return
			}

			for k, v := range body.Boot {
				f.boot[k] = v
			}
		case strings.HasSuffix(path, "/Actions/ComputerSystem.Reset"):
			w.WriteHeader(http.StatusOK)
		case path == "/redfish/v1/Systems/1/ResetActionInfo":
			write(map[string]any{"Parameters": []actionParameter{{Name: "ResetType", AllowableValues: []string{"On", "ForceOff"}}}})
		case path == "/redfish/v1/Managers/1":
			media := path + "/VirtualMedia/"
			if f.relativeLinks {
				media = "VirtualMedia/"
			}

			write(map[string]any{"VirtualMedia": redfishLink{ODataID: media}})
		case strings.HasSuffix(path, "/VirtualMedia"):
			first, second := path+"/1/", path+"/2/"
			if f.relativeLinks {
				first, second = "1/", "2/"
			}

			write(map[string]any{"Members": []redfishLink{{ODataID: first}, {ODataID: second}}})
		case strings.HasSuffix(path, "/VirtualMedia/1"):
			if f.firstMedia != nil {
				write(f.firstMedia)
				return
			}

			write(map[string]any{"MediaTypes": []string{"Floppy", "USBStick"}})
		case strings.HasSuffix(path, "/VirtualMedia/2"):
			if len(f.ejects) > 0 && f.ejectResult != nil {
				write(f.ejectResult)
				return
			}

			if len(f.inserts) > 0 && f.insertResult != nil {
				write(f.insertResult)
				return
			}

			base := path + "/"
			if f.relativeLinks {
				base = ""
			}

			insert := &redfishAction{Target: base + "Actions/VirtualMedia.InsertMedia/"}
			if f.actionInfo != "" {
				insert.ActionInfo = base + "InsertMediaActionInfo/"
			}

			if f.noInsert {
				insert = nil
			}

			write(map[string]any{"MediaTypes": []string{"CD", "DVD"}, "Image": f.image, "Inserted": f.inserted, "Actions": map[string]any{"#VirtualMedia.InsertMedia": insert, "#VirtualMedia.EjectMedia": redfishAction{Target: base + "Actions/VirtualMedia.EjectMedia/"}}})
		case strings.HasSuffix(path, "/InsertMediaActionInfo"):
			f.actionInfoReads++
			if strings.HasSuffix(path, "/VirtualMedia/1/InsertMediaActionInfo") {
				_, _ = w.Write([]byte(f.firstActionInfo))
				return
			}

			_, _ = w.Write([]byte(f.actionInfo))
		case strings.HasSuffix(path, "/Actions/VirtualMedia.InsertMedia"):
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

			f.inserts = append(f.inserts, body)
			if f.insertStatus != 0 {
				http.Error(w, f.errorBody, f.insertStatus)
				return
			}

			if f.async != "" {
				w.Header().Set("Location", "/redfish/v1/TaskService/TaskMonitors/1")
				w.WriteHeader(http.StatusAccepted)

				return
			}

			if !f.ignoreInsert {
				f.image, f.inserted = fmt.Sprint(body["Image"]), true
			}
		case strings.HasSuffix(path, "/Actions/VirtualMedia.EjectMedia"):
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Empty(t, body)

			f.ejects = append(f.ejects, f.image)
			f.image, f.inserted = "", false
		case path == "/redfish/v1/TaskService/TaskMonitors/1":
			f.polls++
			if f.polls == 1 {
				w.WriteHeader(http.StatusAccepted)
				return
			}

			state := "Completed"
			if f.async == "fail" {
				state = "Exception"
			} else {
				f.image, f.inserted = fmt.Sprint(f.inserts[len(f.inserts)-1]["Image"]), true
			}

			write(map[string]string{"TaskState": state, "TaskStatus": "OK"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := Dial(t.Context(), srv.URL, tlsServerFingerprint(srv), "admin", "secret", "")
	require.NoError(t, err)
	t.Cleanup(c.Close)

	return f, c
}

func TestStandardVirtualMediaInstallFlow(t *testing.T) {
	f, c := newFakeILO(t)
	f.image, f.inserted = "http://192.0.2.99/other.iso", true

	require.Equal(t, "1", c.deviceID)
	require.Equal(t, "/redfish/v1/Systems/1", c.system())
	require.True(t, c.SupportsVirtualMedia())
	require.NoError(t, c.StageVirtualMedia(t.Context(), standardMedia))
	require.Empty(t, f.mutations)
	require.NoError(t, c.Reset(t.Context(), ResetForceOff))
	require.Equal(t, "POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset/", f.mutations[0])
	require.NoError(t, c.MountVirtualMedia(t.Context(), standardMedia))
	require.Equal(t, []string{"http://192.0.2.99/other.iso"}, f.ejects)
	require.Equal(t, []map[string]any{{"Image": testISOURL, "Inserted": true, "WriteProtected": true}}, f.inserts)
	require.NoError(t, c.MountVirtualMedia(t.Context(), standardMedia))
	require.Len(t, f.inserts, 1)
	require.NoError(t, c.SetVirtualMediaBoot(t.Context()))
	require.NoError(t, c.SetVirtualMediaBoot(t.Context()))
	require.Equal(t, []map[string]string{{"BootSourceOverrideTarget": "Cd", "BootSourceOverrideEnabled": "Once"}}, f.patches)
	require.NoError(t, c.CleanupVirtualMedia(t.Context(), standardMedia))
	require.False(t, f.inserted)
	require.Equal(t, "Disabled", f.boot["BootSourceOverrideEnabled"])
	require.NoError(t, c.CleanupVirtualMedia(t.Context(), standardMedia))
	require.NoError(t, c.DeleteVirtualMedia(t.Context(), standardMedia))
	require.Len(t, f.ejects, 2)
	require.Empty(t, f.missingHeader)
}

func TestStandardVirtualMediaRelativeLinksWithTrailingSlashes(t *testing.T) {
	for _, owner := range []string{"manager", "system"} {
		t.Run(owner, func(t *testing.T) {
			f, c := newFakeILO(t)
			f.relativeLinks = true
			f.systemMedia = owner == "system"
			f.noManager = f.systemMedia
			f.actionInfo = `{"Parameters":[{"Name":"Image","Required":true}]}`

			require.Equal(t, "1", c.deviceID)
			require.Equal(t, "/redfish/v1/Systems/1", c.system())
			require.NoError(t, c.StageVirtualMedia(t.Context(), standardMedia))
			require.Empty(t, f.mutations)
			require.NoError(t, c.MountVirtualMedia(t.Context(), standardMedia))
			require.NoError(t, c.Reset(t.Context(), ResetForceOff))
			require.NoError(t, c.CleanupVirtualMedia(t.Context(), standardMedia))

			base := "/redfish/v1/Managers/1"
			if f.systemMedia {
				base = "/redfish/v1/Systems/1"
			}

			require.Contains(t, f.resourceReads, base+"/VirtualMedia")
			require.Contains(t, f.resourceReads, base+"/VirtualMedia/2")
			require.Contains(t, f.resourceReads, base+"/VirtualMedia/2/InsertMediaActionInfo")
			require.Contains(t, f.resourceReads, "/redfish/v1/Systems/1/ResetActionInfo")
			require.Equal(t, []string{
				"POST " + base + "/VirtualMedia/2/Actions/VirtualMedia.InsertMedia/",
				"POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset/",
				"POST " + base + "/VirtualMedia/2/Actions/VirtualMedia.EjectMedia/",
			}, f.mutations)
			require.Equal(t, []map[string]any{{"Image": testISOURL}}, f.inserts)
			require.False(t, f.inserted)
		})
	}
}

func TestStandardVirtualMediaCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name        string
		setup       func(*fakeILO)
		action      func(*Client) error
		unsupported bool
	}{
		{"missing service", func(f *fakeILO) { f.noManager = true }, func(c *Client) error { return c.StageVirtualMedia(t.Context(), standardMedia) }, true},
		{"cd not allowed", func(f *fakeILO) { f.allowed = []string{"Pxe"} }, func(c *Client) error { return c.SetVirtualMediaBoot(t.Context()) }, true},
		{"license", func(f *fakeILO) {
			f.insertStatus = 400
			f.errorBody = `{"error":{"@Message.ExtendedInfo":[{"MessageId":"iLO.2.14.LicenseKeyRequired"}]}}`
		}, func(c *Client) error { return c.MountVirtualMedia(t.Context(), standardMedia) }, true},
		{"boot license", func(f *fakeILO) {
			f.patchStatus = 403
			f.errorBody = `{"error":{"@Message.ExtendedInfo":[{"MessageId":"Base.1.19.LicenseRequired"}]}}`
		}, func(c *Client) error { return c.SetVirtualMediaBoot(t.Context()) }, true},
		{"unknown parameter", func(f *fakeILO) {
			f.actionInfo = `{"Parameters":[{"Name":"UserName","Required":true}]}`
			f.image = "other"
			f.inserted = true
		}, func(c *Client) error { return c.MountVirtualMedia(t.Context(), standardMedia) }, true},
		{"protocol", func(f *fakeILO) {
			f.actionInfo = `{"Parameters":[{"Name":"TransferProtocolType","AllowableValues":["HTTPS"]}]}`
		}, func(c *Client) error { return c.MountVirtualMedia(t.Context(), standardMedia) }, true},
		{"transient", func(f *fakeILO) { f.insertStatus = 500 }, func(c *Client) error { return c.MountVirtualMedia(t.Context(), standardMedia) }, false},
		{"reset type", func(*fakeILO) {}, func(c *Client) error { return c.Reset(t.Context(), ResetForceRestart) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeILO(t)
			tc.setup(f)
			err := tc.action(c)
			require.Error(t, err)

			if tc.unsupported {
				require.ErrorIs(t, err, ErrUnsupported)
			} else {
				require.NotErrorIs(t, err, ErrUnsupported)
			}

			require.Empty(t, f.patches)
			require.Empty(t, f.ejects)
		})
	}
}

func TestStandardVirtualMediaActionInfoAndBoot(t *testing.T) {
	f, c := newFakeILO(t)
	f.actionInfo = `{"Parameters":[{"Name":"Image","Required":true},{"Name":"Inserted"},{"Name":"TransferProtocolType","AllowableValues":["HTTP","HTTPS"]},{"Name":"TransferMethod","Required":true,"AllowableValues":["Stream"]}]}`
	f.systemMedia = true
	f.requireMatch = true
	f.boot["BootSourceOverrideMode"] = "Legacy"

	require.NoError(t, c.StageVirtualMedia(t.Context(), standardMedia))
	require.Equal(t, 1, f.actionInfoReads)
	require.Empty(t, f.mutations)
	require.NoError(t, c.MountVirtualMedia(t.Context(), standardMedia))
	require.Equal(t, []map[string]any{{"Image": testISOURL, "Inserted": true, "TransferProtocolType": "HTTP", "TransferMethod": "Stream"}}, f.inserts)
	require.Contains(t, f.mutations, "POST /redfish/v1/Systems/1/VirtualMedia/2/Actions/VirtualMedia.InsertMedia/")
	require.NoError(t, c.SetVirtualMediaBoot(t.Context()))
	require.Len(t, f.patches, 1)
	require.Equal(t, "UEFI", f.patches[0]["BootSourceOverrideMode"])
}

func TestStandardVirtualMediaStagingCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name        string
		setup       func(*fakeILO)
		message     string
		unsupported bool
	}{
		{"cd not allowed", func(f *fakeILO) { f.allowed = []string{"Pxe"} }, "boot override target Cd is not allowed", true},
		{"missing insert", func(f *fakeILO) { f.noInsert = true }, "advertises #VirtualMedia.InsertMedia", true},
		{"required parameter", func(f *fakeILO) {
			f.actionInfo = `{"Parameters":[{"Name":"UserName","Required":true}]}`
		}, "requires unsupported parameter UserName", true},
		{"protocol", func(f *fakeILO) {
			f.actionInfo = `{"Parameters":[{"Name":"TransferProtocolType","AllowableValues":["HTTPS"]}]}`
		}, "does not allow transfer protocol HTTP", true},
		{"transfer method", func(f *fakeILO) {
			f.actionInfo = `{"Parameters":[{"Name":"TransferMethod","Required":true,"AllowableValues":["Upload"]}]}`
		}, "does not allow transfer method Stream", true},
		{"malformed action info", func(f *fakeILO) { f.actionInfo = "invalid" }, "parsing ActionInfo", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeILO(t)
			f.image, f.inserted = "http://192.0.2.99/other.iso", true
			tc.setup(f)

			err := c.StageVirtualMedia(t.Context(), standardMedia)
			require.ErrorContains(t, err, tc.message)

			if tc.unsupported {
				require.ErrorIs(t, err, ErrUnsupported)
			} else {
				require.NotErrorIs(t, err, ErrUnsupported)
			}

			require.Empty(t, f.mutations)
			require.True(t, f.inserted)
			require.Equal(t, "http://192.0.2.99/other.iso", f.image)
		})
	}
}

func TestStandardVirtualMediaStagingSelectsMountDevice(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		t.Run(fmt.Sprintf("first device occupied=%t", occupied), func(t *testing.T) {
			f, c := newFakeILO(t)
			f.firstMedia = map[string]any{
				"MediaTypes": []string{"CD"}, "Inserted": occupied, "Image": "other",
				"Actions": map[string]any{"#VirtualMedia.InsertMedia": redfishAction{
					Target:     "/redfish/v1/Managers/1/VirtualMedia/1/Actions/VirtualMedia.InsertMedia/",
					ActionInfo: "/redfish/v1/Managers/1/VirtualMedia/1/InsertMediaActionInfo/",
				}},
			}
			f.firstActionInfo = `{"Parameters":[{"Name":"UserName","Required":true}]}`

			f.actionInfo = `{"Parameters":[{"Name":"Image","Required":true}]}`
			if occupied {
				f.firstActionInfo, f.actionInfo = f.actionInfo, f.firstActionInfo
			}

			require.ErrorIs(t, c.StageVirtualMedia(t.Context(), standardMedia), ErrUnsupported)
			require.ErrorIs(t, c.MountVirtualMedia(t.Context(), standardMedia), ErrUnsupported)
			require.Equal(t, 2, f.actionInfoReads)
			require.Empty(t, f.mutations)
		})
	}
}

func TestStandardVirtualMediaStagingAlreadyMounted(t *testing.T) {
	for _, noInsert := range []bool{false, true} {
		t.Run(fmt.Sprintf("no insert=%t", noInsert), func(t *testing.T) {
			f, c := newFakeILO(t)
			f.image, f.inserted = testISOURL, true
			f.noInsert = noInsert
			f.actionInfo = `{"Parameters":[{"Name":"UserName","Required":true}]}`

			require.NoError(t, c.StageVirtualMedia(t.Context(), standardMedia))
			require.NoError(t, c.MountVirtualMedia(t.Context(), standardMedia))
			require.Zero(t, f.actionInfoReads)
			require.Empty(t, f.mutations)
		})
	}
}

func TestStandardVirtualMediaStagingRequiresSelectedEject(t *testing.T) {
	for _, state := range []struct {
		name     string
		image    string
		inserted bool
	}{
		{"empty", "", false},
		{"occupied", "http://192.0.2.99/other.iso", true},
		{"already owned", testISOURL, true},
	} {
		for _, action := range []struct {
			name        string
			eject       *redfishAction
			message     string
			unsupported bool
		}{
			{"missing", nil, "does not advertise #VirtualMedia.EjectMedia", true},
			{"empty target", &redfishAction{}, "does not advertise #VirtualMedia.EjectMedia", true},
			{"invalid target", &redfishAction{Target: "http://[invalid"}, "parsing EjectMedia target", false},
		} {
			t.Run(state.name+"/"+action.name, func(t *testing.T) {
				f, c := newFakeILO(t)
				f.powerState = PowerOn
				f.image, f.inserted = "http://192.0.2.99/second.iso", true
				f.firstMedia = map[string]any{
					"MediaTypes": []string{"CD"}, "Image": state.image, "Inserted": state.inserted,
					"Actions": map[string]any{
						"#VirtualMedia.InsertMedia": redfishAction{Target: "/redfish/v1/Managers/1/VirtualMedia/1/Actions/VirtualMedia.InsertMedia/"},
						"#VirtualMedia.EjectMedia":  action.eject,
					},
				}

				err := c.StageVirtualMedia(t.Context(), standardMedia)
				require.ErrorContains(t, err, action.message)

				if action.unsupported {
					require.ErrorIs(t, err, ErrUnsupported)
				} else {
					require.NotErrorIs(t, err, ErrUnsupported)
				}

				require.Empty(t, f.mutations)
				require.Equal(t, state.image, f.firstMedia["Image"])
				require.Equal(t, state.inserted, f.firstMedia["Inserted"])
				require.Equal(t, "http://192.0.2.99/second.iso", f.image)
				require.True(t, f.inserted)
				power, err := c.PowerState(t.Context())
				require.NoError(t, err)
				require.Equal(t, PowerOn, power)
			})
		}
	}
}

func TestStandardVirtualMediaStagingPrefersEmptyDevice(t *testing.T) {
	f, c := newFakeILO(t)
	f.firstMedia = map[string]any{
		"MediaTypes": []string{"CD"}, "Inserted": true, "Image": "other",
		"Actions": map[string]any{"#VirtualMedia.InsertMedia": redfishAction{
			Target:     "/redfish/v1/Managers/1/VirtualMedia/1/Actions/VirtualMedia.InsertMedia/",
			ActionInfo: "/redfish/v1/Managers/1/VirtualMedia/1/InsertMediaActionInfo/",
		}},
	}
	f.firstActionInfo = `{"Parameters":[{"Name":"UserName","Required":true}]}`
	f.actionInfo = `{"Parameters":[{"Name":"Image","Required":true}]}`

	require.NoError(t, c.StageVirtualMedia(t.Context(), standardMedia))
	require.Empty(t, f.mutations)
	require.NoError(t, c.MountVirtualMedia(t.Context(), standardMedia))
	require.Equal(t, []string{"POST /redfish/v1/Managers/1/VirtualMedia/2/Actions/VirtualMedia.InsertMedia/"}, f.mutations)
	require.Empty(t, f.ejects)
	require.Equal(t, []map[string]any{{"Image": testISOURL}}, f.inserts)
}

func TestStandardVirtualMediaTaskMonitor(t *testing.T) {
	old := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = old })

	for _, outcome := range []string{"ok", "fail"} {
		t.Run(outcome, func(t *testing.T) {
			f, c := newFakeILO(t)
			f.async = outcome

			err := c.MountVirtualMedia(t.Context(), standardMedia)
			if outcome == "fail" {
				require.ErrorContains(t, err, "task ended in state Exception")
			} else {
				require.NoError(t, err)
				require.True(t, f.inserted)
			}

			require.Equal(t, 2, f.polls)
		})
	}
}

func TestStandardVirtualMediaCleanupCapability(t *testing.T) {
	for _, tc := range []struct {
		image string
		eject bool
	}{
		{testISOURL, true},
		{strings.Replace(testISOURL, "192.0.2.10", "192.0.2.99", 1), true},
		{"http://192.0.2.10/other.iso", false},
		{strings.Replace(testISOURL, "/bootmedia/", "/prefix/bootmedia/", 1), false},
		{testISOURL + "?other", false},
		{strings.Replace(testISOURL, "0123456789abcdef0123456789abcdef", "1123456789abcdef0123456789abcdef", 1), false},
	} {
		t.Run(tc.image, func(t *testing.T) {
			f, c := newFakeILO(t)
			f.image, f.inserted = tc.image, true

			require.NoError(t, c.CleanupVirtualMedia(t.Context(), standardMedia))
			require.Equal(t, !tc.eject, f.inserted)

			if !tc.eject {
				require.Equal(t, tc.image, f.image)
				require.Empty(t, f.mutations)
			}
		})
	}
}

func TestStandardVirtualMediaMountRequiresImageReadback(t *testing.T) {
	for _, image := range []string{"missing", "empty", "unrelated"} {
		t.Run(image, func(t *testing.T) {
			f, c := newFakeILO(t)

			f.insertResult = map[string]any{"MediaTypes": []string{"CD"}, "Inserted": true}

			switch image {
			case "empty":
				f.insertResult["Image"] = ""
			case "unrelated":
				f.insertResult["Image"] = "http://192.0.2.99/other.iso"
			}

			err := c.MountVirtualMedia(t.Context(), standardMedia)
			require.ErrorContains(t, err, "does not report image "+testISOURL+" as inserted")
			require.NotErrorIs(t, err, ErrUnsupported)
			require.Len(t, f.inserts, 1)
			require.True(t, f.inserted)

			err = c.CleanupVirtualMedia(t.Context(), standardMedia)
			if image == "unrelated" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "inserted media with an unknown image")
				require.NotErrorIs(t, err, ErrUnsupported)
			}

			require.Empty(t, f.ejects)
			require.True(t, f.inserted)
			require.Len(t, f.mutations, 1)
		})
	}
}

func TestStandardVirtualMediaCleanupUnknownImage(t *testing.T) {
	for _, image := range []string{"missing", "empty"} {
		for _, inserted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/inserted=%t", image, inserted), func(t *testing.T) {
				f, c := newFakeILO(t)

				f.firstMedia = map[string]any{"MediaTypes": []string{"DVD"}, "Inserted": inserted}
				if image == "empty" {
					f.firstMedia["Image"] = ""
				}

				f.image, f.inserted = "http://192.0.2.99/other.iso", true

				err := c.CleanupVirtualMedia(t.Context(), standardMedia)
				if inserted {
					require.ErrorContains(t, err, "inserted media with an unknown image")
					require.NotErrorIs(t, err, ErrUnsupported)
				} else {
					require.NoError(t, err)
				}

				require.True(t, f.inserted)
				require.Equal(t, "http://192.0.2.99/other.iso", f.image)
				require.Empty(t, f.mutations)
			})
		}
	}
}

func TestStandardVirtualMediaCleanupVerifiesEjection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		image      string
		inserted   bool
		mediaTypes []string
		message    string
	}{
		{"image cleared but inserted", "", true, []string{"CD"}, "still reports media as inserted"},
		{"image unchanged and inserted", testISOURL, true, []string{"CD"}, "still reports media as inserted"},
		{"image changed but inserted", "http://192.0.2.99/other.iso", true, []string{"CD"}, "still reports media as inserted"},
		{"invalid device", "", false, []string{"USBStick"}, "does not report a CD or DVD device after ejection"},
		{"ejected", "", false, []string{"CD"}, ""},
		{"ejected with stale image", testISOURL, false, []string{"DVD"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeILO(t)
			f.image, f.inserted = testISOURL, true
			f.ejectResult = map[string]any{"MediaTypes": tc.mediaTypes, "Image": tc.image, "Inserted": tc.inserted}

			err := c.CleanupVirtualMedia(t.Context(), standardMedia)
			if tc.message != "" {
				require.ErrorContains(t, err, tc.message)
				require.NotErrorIs(t, err, ErrUnsupported)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, []string{testISOURL}, f.ejects)
		})
	}
}

func TestStandardVirtualMediaCleanupAlreadyEmpty(t *testing.T) {
	f, c := newFakeILO(t)

	require.NoError(t, c.CleanupVirtualMedia(t.Context(), standardMedia))
	require.Empty(t, f.mutations)
}

func TestStandardVirtualMediaCleanupIncompleteDiscovery(t *testing.T) {
	for _, path := range []string{
		"/redfish/v1/Managers/1",
		"/redfish/v1/Managers/1/VirtualMedia",
		"/redfish/v1/Managers/1/VirtualMedia/1",
		"/redfish/v1/Systems/1/VirtualMedia",
	} {
		for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone, http.StatusNotImplemented} {
			t.Run(fmt.Sprintf("%s/%d", path, status), func(t *testing.T) {
				f, c := newFakeILO(t)
				f.image, f.inserted = testISOURL, true
				f.systemMedia = strings.HasPrefix(path, "/redfish/v1/Systems/")
				f.noManager = f.systemMedia
				f.resourceStatuses = map[string]int{path: status}

				err := c.CleanupVirtualMedia(t.Context(), standardMedia)
				require.ErrorContains(t, err, path)
				require.ErrorIs(t, err, ErrUnsupported)
				require.NotErrorIs(t, err, errNoVirtualMediaDevices)
				require.True(t, f.inserted)
				require.Equal(t, testISOURL, f.image)
				require.Empty(t, f.mutations)

				f.resourceStatuses = nil

				require.NoError(t, c.CleanupVirtualMedia(t.Context(), standardMedia))
				require.False(t, f.inserted)
				require.Equal(t, []string{testISOURL}, f.ejects)
			})
		}
	}
}

func TestStandardVirtualMediaRejectsInvalidURL(t *testing.T) {
	f, c := newFakeILO(t)

	for _, raw := range []string{"", "tftp://192.0.2.10/ub.iso", "http://user:pass@192.0.2.10/ub.iso", "/bootmedia/x/boot.iso"} {
		source := standardMedia
		source.URL = raw
		require.Error(t, c.StageVirtualMedia(t.Context(), source))
		require.Error(t, c.MountVirtualMedia(t.Context(), source))
	}

	require.Empty(t, f.mutations)
}

func TestStandardVirtualMediaVerifiesChanges(t *testing.T) {
	f, c := newFakeILO(t)
	f.ignoreInsert = true

	require.ErrorContains(t, c.MountVirtualMedia(t.Context(), standardMedia), "does not report image")

	f.ignoreBoot = true

	require.ErrorContains(t, c.SetVirtualMediaBoot(t.Context()), "does not report one-time")

	f.boot["BootSourceOverrideTarget"] = "Cd"
	f.boot["BootSourceOverrideEnabled"] = "Once"

	require.ErrorContains(t, c.CleanupVirtualMedia(t.Context(), standardMedia), "still enabled")
}

func TestStandardTaskResponses(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		body          string
		done, failure bool
	}{
		{"completed", 200, `{"TaskState":"Completed","TaskStatus":"OK"}`, true, false},
		{"critical", 200, `{"TaskState":"Completed","TaskStatus":"Critical"}`, true, true},
		{"running", 200, `{"TaskState":"Running"}`, false, false},
		{"exception", 200, `{"TaskState":"Exception"}`, true, true},
		{"canceled", 200, `{"TaskState":"Cancelled"}`, true, true}, //nolint:misspell // Redfish wire value.
		{"error", 500, `{"error":{"code":"InternalError"}}`, true, true},
		{"no content", 204, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done, err := taskFinished(sessionResponse{status: tc.status, data: []byte(tc.body)})
			require.Equal(t, tc.done, done)

			if tc.failure {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}

	for _, raw := range []string{"", "invalid", "-1", "0", "1", "999999999"} {
		delay := retryAfter(http.Header{"Retry-After": []string{raw}})
		require.Positive(t, delay)
		require.LessOrEqual(t, delay, maxTaskPollInterval)
	}
}
