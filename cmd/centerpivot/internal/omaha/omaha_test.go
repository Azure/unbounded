// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package omaha

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const appID = "11111111-2222-3333-4444-555555555555"

func check(server string) Check {
	return Check{
		Server:    server,
		AppID:     appID,
		Track:     "stable",
		MachineID: "ebb9a2bb-9365-d34e-133f-8da0664e7366",
		Version:   "202606.15.0",
		Arch:      "amd64",
	}
}

// serve answers every request with status and body, and records the last
// request's body.
func serve(t *testing.T, status int, body string, received *[]byte) *httptest.Server {
	t.Helper()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if received != nil {
			data, err := io.ReadAll(r.Body)
			assert.NoError(t, err)

			*received = data
		}

		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	return server
}

func TestUpdateCheckSendsWhatTheAgentSends(t *testing.T) {
	t.Parallel()

	var received []byte

	server := serve(t, http.StatusOK, nebraskaResponse(`<app appid="`+appID+`" status="ok"><updatecheck status="noupdate"></updatecheck></app>`), &received)
	client := &Client{HTTP: server.Client()}

	_, err := client.UpdateCheck(t.Context(), check(server.URL))
	require.NoError(t, err)

	// What Nebraska reads, element by element: the agent's wire.rs writes the
	// same attributes, with the os version copied from the app's.
	var got struct {
		Protocol  string `xml:"protocol,attr"`
		IsMachine string `xml:"ismachine,attr"`
		SessionID string `xml:"sessionid,attr"`
		OS        struct {
			Platform string `xml:"platform,attr"`
			Version  string `xml:"version,attr"`
			Arch     string `xml:"arch,attr"`
		} `xml:"os"`
		Apps []struct {
			AppID       string     `xml:"appid,attr"`
			Version     string     `xml:"version,attr"`
			Track       string     `xml:"track,attr"`
			MachineID   string     `xml:"machineid,attr"`
			UpdateCheck *struct{}  `xml:"updatecheck"`
			Ping        *struct{}  `xml:"ping"`
			Events      []struct{} `xml:"event"`
		} `xml:"app"`
	}
	require.NoError(t, xml.Unmarshal(received, &got))

	assert.Equal(t, "3.0", got.Protocol)
	assert.Equal(t, "1", got.IsMachine)
	assert.NoError(t, uuid.Validate(got.SessionID))
	assert.Equal(t, "linux", got.OS.Platform)
	assert.Equal(t, "202606.15.0", got.OS.Version)
	assert.Equal(t, "amd64", got.OS.Arch)
	require.Len(t, got.Apps, 1)

	app := got.Apps[0]
	assert.Equal(t, appID, app.AppID)
	assert.Equal(t, "202606.15.0", app.Version)
	assert.Equal(t, "stable", app.Track)
	assert.Equal(t, "ebb9a2bb-9365-d34e-133f-8da0664e7366", app.MachineID)
	assert.NotNil(t, app.UpdateCheck)
	assert.Nil(t, app.Ping, "pings belong to the agent")
	assert.Empty(t, app.Events, "an update check carries no events")
}

func TestReportFailureSendsTheAgentsFailedEvent(t *testing.T) {
	t.Parallel()

	var received []byte

	server := serve(t, http.StatusOK, nebraskaResponse(`<app appid="`+appID+`" status="ok"><event status="ok"></event></app>`), &received)
	client := &Client{HTTP: server.Client()}

	require.NoError(t, client.ReportFailure(t.Context(), check(server.URL)))

	// The agent's report_failure: the same app, with one event, update
	// complete with an error result, and no update check, so it never takes
	// a grant.
	var got struct {
		Apps []struct {
			AppID       string    `xml:"appid,attr"`
			Version     string    `xml:"version,attr"`
			Track       string    `xml:"track,attr"`
			MachineID   string    `xml:"machineid,attr"`
			UpdateCheck *struct{} `xml:"updatecheck"`
			Events      []struct {
				Type            string `xml:"eventtype,attr"`
				Result          string `xml:"eventresult,attr"`
				PreviousVersion string `xml:"previousversion,attr"`
			} `xml:"event"`
		} `xml:"app"`
	}
	require.NoError(t, xml.Unmarshal(received, &got))
	require.Len(t, got.Apps, 1)

	app := got.Apps[0]
	assert.Equal(t, appID, app.AppID)
	assert.Equal(t, "202606.15.0", app.Version)
	assert.Equal(t, "stable", app.Track)
	assert.Equal(t, "ebb9a2bb-9365-d34e-133f-8da0664e7366", app.MachineID)
	assert.Nil(t, app.UpdateCheck)
	require.Len(t, app.Events, 1)
	assert.Equal(t, "3", app.Events[0].Type)
	assert.Equal(t, "0", app.Events[0].Result)
	assert.Equal(t, "202606.15.0", app.Events[0].PreviousVersion)
}

func TestReportFailureAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		app     string
		wantErr bool
	}{
		{
			name: "acknowledged",
			app:  `<app appid="` + appID + `" status="ok"><event status="ok"></event></app>`,
		},
		{
			name: "acknowledged with an update in progress",
			app:  `<app appid="` + appID + `" status="error-updateInProgressOnInstance"><event status="ok"></event></app>`,
		},
		{
			name:    "skipped, for a track Nebraska doesn't know",
			app:     `<app appid="` + appID + `" status="error-unknownApplicationOrGroup"></app>`,
			wantErr: true,
		},
		{
			name:    "rejected",
			app:     `<app appid="` + appID + `" status="ok"><event status="error-internal"></event></app>`,
			wantErr: true,
		},
		{
			name:    "not acknowledged",
			app:     `<app appid="` + appID + `" status="ok"></app>`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := serve(t, http.StatusOK, nebraskaResponse(tt.app), nil)
			err := (&Client{HTTP: server.Client()}).ReportFailure(t.Context(), check(server.URL))

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestUpdateCheckAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		app     string
		want    Result
		wantErr bool
	}{
		{
			name: "no update",
			app:  `<app appid="` + appID + `" status="ok"><updatecheck status="noupdate"></updatecheck></app>`,
			want: Result{Kind: NoUpdate},
		},
		{
			name: "granted",
			app: `<app appid="` + appID + `" status="ok"><updatecheck status="ok">` +
				`<urls><url codebase="https://images.example.com/"></url></urls>` +
				`<manifest version="202606.29.0"><packages><package name="acl.cosi" size="816000000" required="true">` +
				`<labels><label key="cosi.metadata.sha384" value="aa"></label></labels></package></packages></manifest>` +
				`</updatecheck></app>`,
			want: Result{Kind: Granted, Version: "202606.29.0"},
		},
		{
			name: "update in progress",
			app:  `<app appid="` + appID + `" status="error-updateInProgressOnInstance"><updatecheck status="error-internal"></updatecheck></app>`,
			want: Result{Kind: InProgress},
		},
		{
			name: "refused by a rollout limit",
			app:  `<app appid="` + appID + `" status="error-maxUpdatesPerPeriodLimitReached"><updatecheck status="error-internal"></updatecheck></app>`,
			want: Result{Kind: Refused, Status: "error-maxUpdatesPerPeriodLimitReached"},
		},
		{
			name:    "an update without a version",
			app:     `<app appid="` + appID + `" status="ok"><updatecheck status="ok"></updatecheck></app>`,
			wantErr: true,
		},
		{
			name:    "another app",
			app:     `<app appid="99999999-2222-3333-4444-555555555555" status="ok"><updatecheck status="noupdate"></updatecheck></app>`,
			wantErr: true,
		},
		{
			name:    "an HTTP error",
			status:  http.StatusInternalServerError,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			status := tt.status
			if status == 0 {
				status = http.StatusOK
			}

			server := serve(t, status, nebraskaResponse(tt.app), nil)
			client := &Client{HTTP: server.Client()}

			got, err := client.UpdateCheck(t.Context(), check(server.URL))
			if tt.wantErr {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestUpdateCheckNeedsAKnownArchitecture(t *testing.T) {
	t.Parallel()

	c := check("https://nebraska.example.com/v1/update")
	c.Arch = "arm64"

	_, err := (&Client{HTTP: http.DefaultClient}).UpdateCheck(t.Context(), c)
	assert.Error(t, err, "Nebraska spells it aarch64")
}

func TestArch(t *testing.T) {
	t.Parallel()

	for node, want := range map[string]string{"amd64": "amd64", "arm64": "aarch64"} {
		got, err := Arch(node)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}

	_, err := Arch("s390x")
	assert.Error(t, err)
}

// nebraskaResponse wraps apps in a response shaped like Nebraska's.
func nebraskaResponse(apps string) string {
	return xml.Header + `<response protocol="3.0" server="nebraska"><daystart elapsed_seconds="0"></daystart>` + apps + `</response>`
}
