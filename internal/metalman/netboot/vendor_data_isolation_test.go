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

// The vendor-data template ships in the netboot image and is rendered by
// whatever metalman serves it, so it must not depend on the isolation mode or
// on a field only a newer metalman has. The machined guards are written for
// every mode; an agent that runs a mode without an nspawn machine removes them
// at bootstrap.
func TestVendorDataIsTheSameForEveryIsolationMode(t *testing.T) {
	t.Parallel()

	nspawn := renderVendorData(t, "")

	for _, want := range []string{
		"/etc/apt/apt.conf.d/99-unbounded-no-restart-systemd",
		"/etc/needrestart/conf.d/99-unbounded.conf",
		"/etc/unbounded/agent/config.json",
		"runcmd:",
	} {
		if !strings.Contains(nspawn, want) {
			t.Errorf("expected %q in vendor-data", want)
		}
	}

	for _, mode := range []v1alpha3.IsolationMode{v1alpha3.IsolationNSpawn, v1alpha3.IsolationNone} {
		if got := renderVendorData(t, mode); got != nspawn {
			t.Errorf("mode %q renders differently from an unset mode", mode)
		}
	}
}

// A template written for a newer metalman can name a field this one does not
// have. It renders as no value instead of failing the whole render, so a
// newer netboot image keeps booting machines served by an older metalman.
func TestRenderTemplateToleratesUnknownFields(t *testing.T) {
	t.Parallel()

	tmpl := `{{- if not .FieldFromTheFuture }}kept{{ end }} {{ .ApiserverURL }}`

	out, err := renderTemplate(tmpl, newTemplateData(nil, ClusterInfo{ApiserverURL: "https://k8s"}, "", "", "", false))
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}

	if string(out) != "kept https://k8s" {
		t.Errorf("got %q", out)
	}
}

// Every templateData field is available to templates under its own name.
func TestTemplateDataValuesCoverEveryField(t *testing.T) {
	t.Parallel()

	data := templateData{ApiserverURL: "a", ServeURL: "s", InstallRequested: true, VirtualMedia: true, ArtifactRoot: "r", MediaCallbackURL: "m"}

	out, err := renderTemplate("{{.ApiserverURL}}{{.ServeURL}}{{.InstallRequested}}{{.VirtualMedia}}{{.ArtifactRoot}}{{.MediaCallbackURL}}{{.BootLease}}", data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}

	if string(out) != "astruetruerm<nil>" {
		t.Errorf("got %q", out)
	}
}
