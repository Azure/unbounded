// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"errors"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// SessionHTTPServer serves only immutable artifacts authorized by a
// session-scoped capability.
type SessionHTTPServer struct {
	Client       client.Reader
	Cache        *OCICache
	Capabilities *CapabilitySigner
}

func (s *SessionHTTPServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/netboot/sessions/{session}/{capability}/artifacts/{artifact...}", s.handleArtifact)

	return mux
}

func (s *SessionHTTPServer) handleArtifact(w http.ResponseWriter, r *http.Request) {
	if s.Client == nil || s.Cache == nil || s.Capabilities == nil {
		http.Error(w, "session server unavailable", http.StatusServiceUnavailable)
		return
	}

	var session v1alpha3.NetbootSession
	if err := s.Client.Get(r.Context(), client.ObjectKey{Name: r.PathValue("session")}, &session); err != nil {
		if apierrors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "loading session", http.StatusServiceUnavailable)
		return
	}
	if err := s.Capabilities.Verify(&session, r.PathValue("capability")); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if session.Status.Phase != v1alpha3.NetbootSessionPhaseReady && session.Status.Phase != v1alpha3.NetbootSessionPhaseActive {
		http.Error(w, "session unavailable", http.StatusServiceUnavailable)
		return
	}

	artifact, ok := sessionArtifact(&session, r.PathValue("artifact"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	image, ok := sessionArtifactImage(&session, artifact.Source)
	if !ok {
		http.NotFound(w, r)
		return
	}

	reqPath := strings.TrimPrefix(artifact.Path, "/disk/")
	if reqPath == artifact.Path {
		http.NotFound(w, r)
		return
	}
	diskPath, isTemplate, err := s.Cache.ResolveDigestPathForArchitecture(image.Digest, session.Spec.Boot.Architecture, reqPath)
	if err != nil {
		if errors.Is(err, ErrNotYetDownloaded) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "artifact unavailable", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
		return
	}
	if isTemplate {
		http.Error(w, "rendered artifact unavailable", http.StatusServiceUnavailable)
		return
	}

	http.ServeFile(w, r, diskPath)
}

func sessionArtifact(session *v1alpha3.NetbootSession, name string) (v1alpha3.NetbootSessionArtifact, bool) {
	for _, artifact := range session.Spec.Artifacts.Files {
		if artifact.Name == name {
			return artifact, true
		}
	}

	return v1alpha3.NetbootSessionArtifact{}, false
}

func sessionArtifactImage(session *v1alpha3.NetbootSession, source string) (v1alpha3.NetbootSessionImage, bool) {
	switch source {
	case "MachineImage":
		return session.Spec.Artifacts.MachineImage, true
	case "NetbootImage":
		return session.Spec.Artifacts.NetbootImage, true
	default:
		return v1alpha3.NetbootSessionImage{}, false
	}
}
