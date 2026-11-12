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
	insertTarget                         string
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
	taskLink                             string
	mediaReadyAfter, mediaPolls          int
	cancelInsert                         context.CancelFunc
	ejectAsync                           string
	ejectPolls                           int
	keepEjectedImage                     bool
	connect                              func() *Client
}

func (f *fakeILO) finishEject() {
	// A completed detach also stops any pending insertion of this image.
	f.mediaReadyAfter = 0

	f.inserted = false
	if !f.keepEjectedImage {
		f.image = ""
	}
}

func newFakeILO(t *testing.T) (*fakeILO, *Client) {
	t.Helper()

	return newFakeILOWithDeviceID(t, "")
}

func newFakeILOWithDeviceID(t *testing.T, deviceID string) (*fakeILO, *Client) {
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

			system := map[string]any{"@odata.id": "/redfish/v1/Systems/1/", "PowerState": f.powerState, "Boot": boot, "Actions": map[string]any{"#ComputerSystem.Reset": map[string]any{"target": "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset/", "ResetType@Redfish.AllowableValues": []string{"On", "ForceOff"}}}}
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
			if len(f.inserts) > 0 && f.cancelInsert != nil {
				f.cancelInsert()
			}

			if f.mediaReadyAfter > 0 && (len(f.inserts) > 0 || f.image == testISOURL) {
				f.mediaPolls++
				if f.mediaPolls >= f.mediaReadyAfter {
					f.image, f.inserted = testISOURL, true
				}
			}

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
			if f.insertTarget != "" {
				insert.Target = f.insertTarget
			}

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
				if f.async == "unmonitored" {
					f.image = fmt.Sprint(body["Image"])
				} else if f.taskLink == "" {
					w.Header().Set("Location", "/redfish/v1/TaskService/TaskMonitors/1")
				}

				w.WriteHeader(http.StatusAccepted)

				if f.taskLink != "" {
					write(redfishLink{ODataID: f.taskLink})
				}

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
			if f.ejectAsync != "" {
				if f.ejectAsync != "unmonitored" {
					w.Header().Set("Location", "/redfish/v1/TaskService/TaskMonitors/2")
				}

				w.WriteHeader(http.StatusAccepted)

				return
			}

			f.finishEject()
		case path == "/redfish/v1/TaskService/TaskMonitors/2":
			f.ejectPolls++

			state := "Running"
			if f.ejectAsync == "fail" {
				state = "Exception"
			} else if f.ejectAsync != "running" && f.ejectPolls > 1 {
				f.finishEject()

				state = "Completed"
			}

			write(map[string]string{"TaskState": state, "TaskStatus": "OK"})
		case path == "/redfish/v1/TaskService/TaskMonitors/1" || path == "/redfish/v1/Managers/1/VirtualMedia/2/Actions/VirtualMedia.InsertMedia/task":
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

	f.connect = func() *Client {
		c, err := Dial(t.Context(), srv.URL, tlsServerFingerprint(srv), "admin", "secret", deviceID)
		require.NoError(t, err)
		t.Cleanup(c.Close)

		return c
	}

	return f, f.connect()
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
	require.Equal(t, []map[string]any{{"Image": testISOURL}}, f.inserts)
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

func TestStandardVirtualMediaExplicitSystemID(t *testing.T) {
	for _, owner := range []string{"manager", "system"} {
		for _, links := range []string{"relative", "absolute"} {
			t.Run(owner+"/"+links, func(t *testing.T) {
				f, c := newFakeILOWithDeviceID(t, "1")
				f.relativeLinks = links == "relative"
				f.systemMedia = owner == "system"
				f.noManager = f.systemMedia
				f.actionInfo = `{"Parameters":[{"Name":"Image","Required":true}]}`

				require.Equal(t, "1", c.deviceID)
				require.Equal(t, "/redfish/v1/Systems/1", c.systemBase())
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

				require.NotContains(t, f.resourceReads, "/redfish/v1/Systems")
				require.Contains(t, f.resourceReads, base+"/VirtualMedia/2/InsertMediaActionInfo")

				if f.relativeLinks {
					require.Contains(t, f.resourceReads, "/redfish/v1/Systems/1/ResetActionInfo")
				}

				require.Equal(t, []string{
					"POST " + base + "/VirtualMedia/2/Actions/VirtualMedia.InsertMedia/",
					"POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset/",
					"POST " + base + "/VirtualMedia/2/Actions/VirtualMedia.EjectMedia/",
				}, f.mutations)
				require.Equal(t, []map[string]any{{"Image": testISOURL}}, f.inserts)
				require.False(t, f.inserted)
				require.Empty(t, f.missingHeader)
			})
		}
	}
}

func TestStandardSystemCanonicalURI(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantError string
		status                int
	}{
		{name: "canonical", body: `{"@odata.id":"/redfish/v1/Systems/1/"}`},
		{name: "absolute URI", body: `{"@odata.id":"https://bmc.example/redfish/v1/Systems/1/"}`},
		{name: "missing canonical URI", body: `{"Id":"1"}`},
		{name: "different response ID", body: `{"Id":"other","@odata.id":"/redfish/v1/Systems/1/"}`},
		{name: "invalid JSON", body: `{`, wantError: "parsing system"},
		{name: "invalid canonical URI", body: `{"@odata.id":"%zz"}`, wantError: "parsing system link"},
		{name: "unavailable", status: http.StatusServiceUnavailable, body: `{"error":"unavailable"}`, wantError: "returned HTTP 503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/redfish/v1/Systems/1", r.URL.Path)
				require.Equal(t, "4.0", r.Header.Get("OData-Version"))

				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}

				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			client := &Client{session: &bmcSession{httpClient: srv.Client(), baseURL: srv.URL}, deviceID: "1"}
			target, resetErr := client.resetTarget(t.Context(), ResetForceOff)

			collections, mediaErr := client.virtualMediaCollections(t.Context())
			for _, err := range []error{resetErr, mediaErr} {
				if tc.wantError != "" {
					require.ErrorContains(t, err, tc.wantError)
				} else {
					require.NoError(t, err)
				}
			}

			if tc.wantError == "" {
				require.Equal(t, "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", target)
			}

			require.Empty(t, collections)
			require.Equal(t, "1", client.deviceID)
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

func TestStandardVirtualMediaForbidden(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		unsupported bool
	}{
		{"SMC code", `{"error":{"code":"SMC.1.0.OemLicenseNotPassed"}}`, true},
		{"SMC nested extended info", `{"error":{"code":"Base.1.0.GeneralError","@Message.ExtendedInfo":[{"MessageId":"SMC.1.0.OemLicenseNotPassed"}]}}`, true},
		{"SMC top-level extended info", `{"@Message.ExtendedInfo":[{"MessageId":"SMC.1.0.OemLicenseNotPassed"}]}`, true},
		{"ordinary forbidden", `{"error":{"code":"Base.1.19.InsufficientPrivilege"}}`, false},
	} {
		for _, operation := range []string{"mount", "boot"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				f, c := newFakeILO(t)
				f.errorBody = tc.body
				f.insertStatus, f.patchStatus = http.StatusForbidden, http.StatusForbidden

				require.NoError(t, c.StageVirtualMedia(t.Context(), standardMedia))
				require.Empty(t, f.mutations)

				var err error
				if operation == "mount" {
					err = c.MountVirtualMedia(t.Context(), standardMedia)
					require.Len(t, f.inserts, 1)
				} else {
					err = c.SetVirtualMediaBoot(t.Context())
					require.Empty(t, f.inserts)
				}

				require.ErrorContains(t, err, "HTTP 403")
				require.ErrorContains(t, err, tc.body)

				if tc.unsupported {
					require.ErrorIs(t, err, ErrUnsupported)
				} else {
					require.NotErrorIs(t, err, ErrUnsupported)
				}

				require.Len(t, f.mutations, 1)
				require.Empty(t, f.ejects)
				require.False(t, f.inserted)
				require.Equal(t, "Disabled", f.boot["BootSourceOverrideEnabled"])
			})
		}
	}
}

func TestStandardVirtualMediaBootTargets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed []string
		target  BootTarget
	}{
		{"USB CD only", []string{"UsbCd"}, BootTargetUsbCd},
		{"both prefer USB CD", []string{"Cd", "UsbCd"}, BootTargetUsbCd},
		{"CD only", []string{"Cd"}, BootTargetCd},
		{"missing allowed targets", nil, BootTargetCd},
		{"empty allowed targets", []string{}, BootTargetCd},
		{"no supported target", []string{"None", "Pxe", "Hdd"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeILO(t)
			f.allowed = tc.allowed

			stageErr := c.StageVirtualMedia(t.Context(), standardMedia)
			require.Empty(t, f.mutations)

			setErr := c.SetVirtualMediaBoot(t.Context())
			if tc.target == "" {
				require.ErrorIs(t, stageErr, ErrUnsupported)
				require.ErrorIs(t, setErr, ErrUnsupported)
				require.Empty(t, f.mutations)

				return
			}

			require.NoError(t, stageErr)
			require.NoError(t, setErr)
			require.Equal(t, []map[string]string{{"BootSourceOverrideTarget": string(tc.target), "BootSourceOverrideEnabled": "Once"}}, f.patches)
			require.NoError(t, c.SetVirtualMediaBoot(t.Context()))
			require.Len(t, f.patches, 1)
		})
	}
}

func TestStandardVirtualMediaBootRequiresSelectedTargetReadback(t *testing.T) {
	for _, target := range []BootTarget{BootTargetCd, BootTargetUsbCd} {
		t.Run(string(target), func(t *testing.T) {
			f, c := newFakeILO(t)
			f.allowed = []string{string(target)}
			f.ignoreBoot = true
			f.boot["BootSourceOverrideEnabled"] = "Once"

			f.boot["BootSourceOverrideTarget"] = "UsbCd"
			if target == BootTargetUsbCd {
				f.boot["BootSourceOverrideTarget"] = "Cd"
			}

			require.ErrorContains(t, c.SetVirtualMediaBoot(t.Context()), "does not report one-time UEFI "+string(target)+" boot")
			require.Equal(t, []map[string]string{{"BootSourceOverrideTarget": string(target), "BootSourceOverrideEnabled": "Once"}}, f.patches)
		})
	}
}

func TestStandardVirtualMediaCleanupBootTargets(t *testing.T) {
	for _, target := range []BootTarget{BootTargetCd, BootTargetUsbCd} {
		for _, tc := range []struct {
			name    string
			allowed []string
			setNone bool
		}{
			{"both allowed", []string{"None", "Cd", "UsbCd"}, true},
			{"allowed targets changed", []string{"Pxe"}, false},
			{"only None now allowed", []string{"None"}, true},
			{"missing allowed targets", nil, true},
		} {
			for _, ignore := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/ignore=%t", target, tc.name, ignore), func(t *testing.T) {
					f, c := newFakeILO(t)
					f.boot["BootSourceOverrideTarget"] = string(target)
					f.boot["BootSourceOverrideEnabled"] = "Once"
					f.allowed = tc.allowed
					f.ignoreBoot = ignore
					f.image, f.inserted = testISOURL, true

					err := c.CleanupVirtualMedia(t.Context(), standardMedia)

					settings := map[string]string{"BootSourceOverrideEnabled": "Disabled"}
					if tc.setNone {
						settings["BootSourceOverrideTarget"] = "None"
					}

					require.Equal(t, []map[string]string{settings}, f.patches)

					if ignore {
						require.ErrorContains(t, err, "still enabled")
						require.Empty(t, f.ejects)

						return
					}

					require.NoError(t, err)
					require.Equal(t, "Disabled", f.boot["BootSourceOverrideEnabled"])
					require.False(t, f.inserted)
					require.Equal(t, []string{testISOURL}, f.ejects)
					require.NoError(t, c.CleanupVirtualMedia(t.Context(), standardMedia))
					require.Len(t, f.patches, 1)
					require.Len(t, f.ejects, 1)
				})
			}
		}
	}
}

func TestStandardVirtualMediaInsertOptionalBooleans(t *testing.T) {
	for _, tc := range []struct {
		name       string
		actionInfo string
		want       map[string]any
	}{
		{
			name: "no action info",
			want: map[string]any{"Image": testISOURL},
		},
		{
			name:       "booleans not listed",
			actionInfo: `{"Parameters":[{"Name":"Image","Required":true}]}`,
			want:       map[string]any{"Image": testISOURL},
		},
		{
			name:       "optional booleans",
			actionInfo: `{"Parameters":[{"Name":"Image","Required":true},{"Name":"Inserted"},{"Name":"WriteProtected","Required":false}]}`,
			want:       map[string]any{"Image": testISOURL},
		},
		{
			name:       "required booleans",
			actionInfo: `{"Parameters":[{"Name":"Image","Required":true},{"Name":"Inserted","Required":true},{"Name":"WriteProtected","Required":true}]}`,
			want:       map[string]any{"Image": testISOURL, "Inserted": true, "WriteProtected": true},
		},
		{
			name:       "only inserted required",
			actionInfo: `{"Parameters":[{"Name":"Inserted","Required":true},{"Name":"WriteProtected"}]}`,
			want:       map[string]any{"Image": testISOURL, "Inserted": true},
		},
		{
			name:       "only write protected required",
			actionInfo: `{"Parameters":[{"Name":"Inserted"},{"Name":"WriteProtected","Required":true}]}`,
			want:       map[string]any{"Image": testISOURL, "WriteProtected": true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeILO(t)
			f.actionInfo = tc.actionInfo

			require.NoError(t, c.StageVirtualMedia(t.Context(), standardMedia))
			require.Empty(t, f.mutations)
			require.NoError(t, c.MountVirtualMedia(t.Context(), standardMedia))
			require.Equal(t, []map[string]any{tc.want}, f.inserts)
			require.True(t, f.inserted)
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
	require.Equal(t, []map[string]any{{"Image": testISOURL, "TransferProtocolType": "HTTP", "TransferMethod": "Stream"}}, f.inserts)
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
		{"cd not allowed", func(f *fakeILO) { f.allowed = []string{"Pxe"} }, "neither boot override target UsbCd nor Cd is allowed", true},
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

func TestStandardVirtualMediaStagingInsertTarget(t *testing.T) {
	for _, state := range []struct {
		name     string
		image    string
		inserted bool
	}{
		{"empty", "", false},
		{"occupied", "http://192.0.2.99/other.iso", true},
		{"already mounted", testISOURL, true},
	} {
		for _, target := range []struct {
			name, path string
			invalid    bool
		}{
			{"relative", "Actions/VirtualMedia.InsertMedia/", false},
			{"root relative", "/redfish/v1/Managers/1/VirtualMedia/2/Actions/VirtualMedia.InsertMedia/", false},
			{"absolute", "https://bmc.example/redfish/v1/Managers/1/VirtualMedia/2/Actions/VirtualMedia.InsertMedia/", false},
			{"malformed", "http://[invalid", true},
		} {
			t.Run(state.name+"/"+target.name, func(t *testing.T) {
				f, c := newFakeILO(t)
				f.powerState = PowerOn
				f.image, f.inserted = state.image, state.inserted
				f.insertTarget = target.path

				stageErr := c.StageVirtualMedia(t.Context(), standardMedia)
				require.Empty(t, f.mutations)
				require.Equal(t, state.image, f.image)
				require.Equal(t, state.inserted, f.inserted)

				mountErr := c.MountVirtualMedia(t.Context(), standardMedia)
				if target.invalid && state.image != testISOURL {
					for _, err := range []error{stageErr, mountErr} {
						require.ErrorContains(t, err, "parsing InsertMedia target")
						require.NotErrorIs(t, err, ErrUnsupported)
					}

					require.Empty(t, f.mutations)
					require.Equal(t, state.image, f.image)
					require.Equal(t, state.inserted, f.inserted)
				} else {
					require.NoError(t, stageErr)
					require.NoError(t, mountErr)
					require.True(t, f.inserted)
					require.Equal(t, testISOURL, f.image)

					if state.image == testISOURL {
						require.Empty(t, f.mutations)
					} else {
						require.Equal(t, []map[string]any{{"Image": testISOURL}}, f.inserts)
					}
				}

				power, err := c.PowerState(t.Context())
				require.NoError(t, err)
				require.Equal(t, PowerOn, power)
				require.NotContains(t, f.mutations, "POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset/")
			})
		}
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

func TestStandardVirtualMediaTaskBodyLink(t *testing.T) {
	old := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = old })

	for _, link := range []string{"/redfish/v1/TaskService/TaskMonitors/1", "task"} {
		for _, outcome := range []string{"ok", "fail"} {
			t.Run(link+"/"+outcome, func(t *testing.T) {
				f, c := newFakeILO(t)
				f.async, f.taskLink = outcome, link

				err := c.MountVirtualMedia(t.Context(), standardMedia)
				if outcome == "fail" {
					require.ErrorContains(t, err, "task ended in state Exception")
				} else {
					require.NoError(t, err)
					require.True(t, f.inserted)
				}

				require.Equal(t, 2, f.polls)
				require.Len(t, f.inserts, 1)
				require.Empty(t, f.ejects)
			})
		}
	}
}

func TestStandardVirtualMediaUnmonitoredInsert(t *testing.T) {
	old := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = old })

	t.Run("delayed insertion", func(t *testing.T) {
		f, c := newFakeILO(t)
		f.async, f.mediaReadyAfter = "unmonitored", 3

		require.NoError(t, c.MountVirtualMedia(t.Context(), standardMedia))
		require.True(t, f.inserted)
		require.Equal(t, 3, f.mediaPolls)
		require.Len(t, f.inserts, 1)
		require.Empty(t, f.ejects)
		require.NoError(t, c.MountVirtualMedia(t.Context(), standardMedia))
		require.Len(t, f.inserts, 1)
	})

	for _, outcome := range []string{"timeout", "cancel", "wrong image"} {
		t.Run(outcome, func(t *testing.T) {
			f, c := newFakeILO(t)

			f.async = "unmonitored"
			if outcome == "wrong image" {
				f.insertResult = map[string]any{"MediaTypes": []string{"CD"}, "Image": testISOURL + "?other", "Inserted": true}
			}

			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()

			if outcome == "cancel" {
				f.cancelInsert = cancel
			}

			err := c.MountVirtualMedia(ctx, standardMedia)
			if outcome == "cancel" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}

			f.mu.Lock()
			defer f.mu.Unlock()

			require.Len(t, f.inserts, 1)
			require.Empty(t, f.ejects)
			require.Len(t, f.mutations, 1)
		})
	}
}

func TestStandardVirtualMediaPendingImageRetry(t *testing.T) {
	old := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = old })

	f, c := newFakeILO(t)
	f.image, f.mediaReadyAfter = testISOURL, 3

	require.NoError(t, c.MountVirtualMedia(t.Context(), standardMedia))
	require.True(t, f.inserted)
	require.Equal(t, 3, f.mediaPolls)
	require.Empty(t, f.mutations)
}

func TestStandardVirtualMediaCleanupAfterInterruptedInsert(t *testing.T) {
	old := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = old })

	for _, outcome := range []string{"cancel", "timeout"} {
		for _, changedHost := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/changedHost=%t", outcome, changedHost), func(t *testing.T) {
				f, c := newFakeILO(t)
				f.async = "unmonitored"

				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()

				if outcome == "cancel" {
					f.cancelInsert = cancel
				}

				err := c.MountVirtualMedia(ctx, standardMedia)
				if outcome == "cancel" {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}

				f.mu.Lock()
				f.cancelInsert = nil
				require.Equal(t, testISOURL, f.image)
				require.False(t, f.inserted)
				require.Len(t, f.inserts, 1)
				f.mu.Unlock()

				source := standardMedia
				if changedHost {
					source.URL = strings.Replace(testISOURL, "192.0.2.10", "192.0.2.99", 1)
				}

				c.Close()

				c = f.connect()
				for _, ejectOutcome := range []string{"unmonitored", "fail", "running"} {
					f.mu.Lock()
					f.ejectAsync = ejectOutcome
					f.mu.Unlock()

					cleanupCtx, cleanupCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
					err = c.CleanupVirtualMedia(cleanupCtx, source)

					cleanupCancel()

					switch ejectOutcome {
					case "unmonitored":
						require.ErrorContains(t, err, "no completion monitor")
					case "fail":
						require.ErrorContains(t, err, "task ended in state Exception")
					case "running":
						require.ErrorIs(t, err, context.DeadlineExceeded)
					}

					require.NotErrorIs(t, err, ErrUnsupported)
					f.mu.Lock()
					require.Equal(t, testISOURL, f.image)
					require.False(t, f.inserted)
					f.mu.Unlock()
				}

				f.mu.Lock()
				f.ejectAsync, f.ejectPolls = "ok", 0
				// Without a successful eject, this insertion would finish later.
				f.mediaReadyAfter = f.mediaPolls + 3
				f.mu.Unlock()

				require.NoError(t, c.CleanupVirtualMedia(t.Context(), source))
				require.Equal(t, []string{testISOURL, testISOURL, testISOURL, testISOURL}, f.ejects)
				require.Equal(t, 2, f.ejectPolls)
				require.Zero(t, f.mediaReadyAfter)
				require.False(t, f.inserted)
				require.Empty(t, f.image)
				require.NoError(t, c.CleanupVirtualMedia(t.Context(), source))
				require.Len(t, f.ejects, 4)
			})
		}
	}
}

func TestStandardVirtualMediaRetryAfterCanceledInsert(t *testing.T) {
	old := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = old })

	f, c := newFakeILO(t)
	f.async = "unmonitored"

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	f.cancelInsert = cancel

	require.ErrorIs(t, c.MountVirtualMedia(ctx, standardMedia), context.Canceled)

	f.mu.Lock()
	f.cancelInsert, f.mediaReadyAfter = nil, 3
	f.mu.Unlock()

	require.NoError(t, c.MountVirtualMedia(t.Context(), standardMedia))
	require.Len(t, f.inserts, 1)
	require.Empty(t, f.ejects)
	require.Len(t, f.mutations, 1)
}

func TestStandardVirtualMediaSynchronousInsertReadbackFailure(t *testing.T) {
	old := taskPollInterval
	taskPollInterval = time.Hour

	t.Cleanup(func() { taskPollInterval = old })

	f, c := newFakeILO(t)
	f.ignoreInsert = true

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := c.MountVirtualMedia(ctx, standardMedia)
	require.ErrorContains(t, err, "does not report image "+testISOURL+" as inserted")
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, f.inserts, 1)
	require.Empty(t, f.ejects)
}

func TestStandardVirtualMediaInsertWaitHonorsRetryAfter(t *testing.T) {
	old := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = old })

	f, c := newFakeILO(t)
	f.image, f.inserted = testISOURL, true
	reads := len(f.resourceReads)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	err := c.waitVirtualMediaInsert(ctx, "/redfish/v1/Managers/1/VirtualMedia/2/", testISOURL, http.Header{"Retry-After": {"999999"}})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, f.resourceReads, reads)
	require.Empty(t, f.mutations)
}

func TestStandardTaskLinkResolution(t *testing.T) {
	old := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = old })

	for _, tc := range []struct {
		name, path, link, location, monitor string
	}{
		{
			name: "trailing slash action", path: "/redfish/v1/Managers/1/VirtualMedia/2/Actions/VirtualMedia.InsertMedia/",
			link: "task", monitor: "/redfish/v1/Managers/1/VirtualMedia/2/Actions/VirtualMedia.InsertMedia/task",
		},
		{
			name: "no trailing slash action", path: "/redfish/v1/Managers/1/VirtualMedia/2/Actions/VirtualMedia.InsertMedia",
			link: "VirtualMedia.InsertMedia/task", monitor: "/redfish/v1/Managers/1/VirtualMedia/2/Actions/VirtualMedia.InsertMedia/task",
		},
		{
			name: "location takes precedence", path: "/redfish/v1/Managers/1/VirtualMedia/2/Actions/VirtualMedia.InsertMedia/",
			link: "unused", location: "/redfish/v1/TaskService/TaskMonitors/1", monitor: "/redfish/v1/TaskService/TaskMonitors/1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeILO(t)
			f.async = "fail"
			header := http.Header{}
			header.Set("Location", tc.location)
			data, err := json.Marshal(redfishLink{ODataID: tc.link})
			require.NoError(t, err)

			_, err = c.waitTask(t.Context(), http.MethodPost, tc.path, sessionResponse{status: http.StatusAccepted, header: header, data: data})
			require.ErrorContains(t, err, "task ended in state Exception")
			require.Contains(t, f.resourceReads, tc.monitor)
			require.Equal(t, 2, f.polls)
			require.Empty(t, f.mutations)
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

func TestStandardVirtualMediaCleanupPendingImageCleared(t *testing.T) {
	f, c := newFakeILO(t)
	f.image = testISOURL
	f.ejectAsync = "unmonitored"

	require.ErrorContains(t, c.CleanupVirtualMedia(t.Context(), standardMedia), "insertion may be pending")

	f.image = ""

	require.NoError(t, c.CleanupVirtualMedia(t.Context(), standardMedia))
	require.Equal(t, []string{testISOURL}, f.ejects)
}

func TestStandardVirtualMediaCleanupStaleImageAfterRestart(t *testing.T) {
	old := taskPollInterval
	taskPollInterval = time.Millisecond

	t.Cleanup(func() { taskPollInterval = old })

	for _, initialInserted := range []bool{false, true} {
		for _, async := range []string{"", "ok"} {
			for _, changedHost := range []bool{false, true} {
				t.Run(fmt.Sprintf("inserted=%t/async=%s/changedHost=%t", initialInserted, async, changedHost), func(t *testing.T) {
					f, c := newFakeILO(t)
					f.image, f.inserted = testISOURL, initialInserted
					f.keepEjectedImage, f.ejectAsync = true, async

					require.NoError(t, c.CleanupVirtualMedia(t.Context(), standardMedia))
					require.Equal(t, testISOURL, f.image)
					require.False(t, f.inserted)
					require.Len(t, f.ejects, 1)

					// Simulate failure to save Cleaned, followed by a restart.
					// Only the original source survives, not client-side evidence.
					c.Close()
					c = f.connect()

					source := standardMedia
					if changedHost {
						source.URL = strings.Replace(testISOURL, "192.0.2.10", "192.0.2.99", 1)
					}

					for range 2 {
						require.NoError(t, c.CleanupVirtualMedia(t.Context(), source))
						require.Equal(t, testISOURL, f.image)
						require.False(t, f.inserted)
					}

					require.Equal(t, []string{testISOURL, testISOURL, testISOURL}, f.ejects)
				})
			}
		}
	}
}

func TestStandardVirtualMediaCleanupUnrelatedPendingImage(t *testing.T) {
	for _, image := range []string{
		"http://192.0.2.10/other.iso",
		strings.Replace(testISOURL, "/bootmedia/", "/prefix/bootmedia/", 1),
		testISOURL + "?other",
		strings.Replace(testISOURL, "0123456789abcdef0123456789abcdef", "1123456789abcdef0123456789abcdef", 1),
	} {
		t.Run(image, func(t *testing.T) {
			f, c := newFakeILO(t)
			f.image = image

			require.NoError(t, c.CleanupVirtualMedia(t.Context(), standardMedia))
			require.Equal(t, image, f.image)
			require.False(t, f.inserted)
			require.Empty(t, f.mutations)
		})
	}
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
