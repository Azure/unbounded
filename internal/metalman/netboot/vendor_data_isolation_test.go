// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
)

func renderVendorData(t *testing.T, mode v1alpha3.IsolationMode) string {
	t.Helper()

	tmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "vendor-data.tmpl"))
	if err != nil {
		t.Fatalf("reading vendor-data.tmpl: %v", err)
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "n"},
		Spec:       v1alpha3.MachineSpec{Isolation: mode},
	}
	data := newTemplateData(node, ClusterInfo{ApiserverURL: "https://k8s.example.com"}, "http://10.0.1.1:8080", "{}", "", false)

	out, err := renderTemplate(string(tmpl), data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}

	return string(out)
}

var machinedGuards = []string{
	"/etc/apt/apt.conf.d/99-unbounded-no-restart-systemd",
	`"systemd-container";`,
	"/etc/needrestart/conf.d/99-unbounded.conf",
	"systemd-machined",
}

// An unset mode and an explicit nspawn render exactly what they rendered before
// the setting existed, and a template data value built without the field keeps
// the guards.
func TestVendorDataMachinedGuardsKeptForNSpawn(t *testing.T) {
	t.Parallel()

	unset := renderVendorData(t, "")
	explicit := renderVendorData(t, v1alpha3.IsolationNSpawn)

	if unset != explicit {
		t.Errorf("nspawn and unset render differently:\n%s\n---\n%s", unset, explicit)
	}

	for _, want := range machinedGuards {
		if !strings.Contains(unset, want) {
			t.Errorf("expected %q in nspawn vendor-data", want)
		}
	}

	// The guard block sits between the cloud-init marker and runcmd with the
	// same layout as before: no blank lines were introduced.
	if !strings.Contains(unset, "  content: ''\n# Block unattended-upgrades") || !strings.Contains(unset, "$nrconf{restart} = 'l';\nruncmd:") {
		t.Errorf("guard block layout changed:\n%s", unset)
	}
}

func TestVendorDataMachinedGuardsOmittedForOtherModes(t *testing.T) {
	t.Parallel()

	for _, mode := range []v1alpha3.IsolationMode{v1alpha3.IsolationMode("none"), v1alpha3.IsolationMode("container")} {
		body := renderVendorData(t, mode)

		for _, unwanted := range machinedGuards[:3] {
			if strings.Contains(body, unwanted) {
				t.Errorf("mode %s: unexpected %q in vendor-data", mode, unwanted)
			}
		}

		for _, want := range []string{"/etc/cloud/cloud-init.disabled", "runcmd:", "unbounded-agent-install.sh", "/etc/unbounded/agent/config.json"} {
			if !strings.Contains(body, want) {
				t.Errorf("mode %s: expected %q to remain", mode, want)
			}
		}

		if !strings.Contains(body, "  content: ''\nruncmd:") {
			t.Errorf("mode %s: runcmd should follow the cloud-init marker directly:\n%s", mode, body)
		}
	}
}

func TestOmitsMachinedGuards(t *testing.T) {
	t.Parallel()

	for mode, want := range map[v1alpha3.IsolationMode]bool{"": false, v1alpha3.IsolationNSpawn: false, "none": true, "container": true} {
		if got := omitsMachinedGuards(mode); got != want {
			t.Errorf("omitsMachinedGuards(%q) = %v, want %v", mode, got, want)
		}
	}

	if newTemplateData(nil, ClusterInfo{}, "", "", "", false).OmitMachinedGuards {
		t.Error("a nil machine keeps the guards")
	}
}
