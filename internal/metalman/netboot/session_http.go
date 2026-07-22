// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	pathpkg "path"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// SessionHTTPServer serves only immutable artifacts authorized by a
// session-scoped capability.
type SessionHTTPServer struct {
	Client         client.Reader
	Cache          *OCICache
	Capabilities   *CapabilitySigner
	StatusRecorder SessionConditionRecorder
}

// SessionConditionRecorder persists a milestone for an exact session identity.
type SessionConditionRecorder interface {
	RecordCondition(ctx context.Context, sessionName string, sessionUID types.UID, condition metav1.Condition) error
}

// SessionArtifactURL returns the externally advertised capability URL for one
// artifact listed by the immutable session.
func SessionArtifactURL(signer *CapabilitySigner, session *v1alpha3.NetbootSession, artifactName string) (string, error) {
	if signer == nil || session == nil {
		return "", errors.New("capability signer and session are required")
	}
	if _, ok := sessionArtifact(session, artifactName); !ok {
		return "", fmt.Errorf("artifact %q is not listed by session %s", artifactName, session.Name)
	}
	cleanArtifact := strings.TrimPrefix(artifactName, "/")
	if cleanArtifact == "" || pathpkg.Clean(cleanArtifact) != cleanArtifact || strings.HasPrefix(artifactName, "/") {
		return "", fmt.Errorf("invalid artifact name %q", artifactName)
	}
	capability, err := signer.Sign(session)
	if err != nil {
		return "", err
	}

	return JoinServeURLPath(session.Spec.Endpoint.ExternalURL, pathpkg.Join("v1/netboot/sessions", session.Name, capability, "artifacts", cleanArtifact))
}

func (s *SessionHTTPServer) Handler() http.Handler {
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	return mux
}

// RegisterHandlers adds authenticated session artifact and callback routes.
func (s *SessionHTTPServer) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/netboot/sessions/{session}/{capability}/artifacts/{artifact...}", s.handleArtifact)
	mux.HandleFunc("POST /v1/netboot/sessions/{session}/{capability}/callbacks/{milestone}", s.handleCallback)
}

func (s *SessionHTTPServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authorizeSession(w, r)
	if !ok {
		return
	}
	if s.StatusRecorder == nil {
		http.Error(w, "session status unavailable", http.StatusServiceUnavailable)
		return
	}

	conditionType, ok := sessionMilestoneCondition(r.PathValue("milestone"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.StatusRecorder.RecordCondition(r.Context(), session.Name, session.UID, metav1.Condition{
		Type:    conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  "Succeeded",
		Message: "provisioning client reported milestone",
	}); err != nil {
		http.Error(w, "recording session status", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *SessionHTTPServer) handleArtifact(w http.ResponseWriter, r *http.Request) {
	if s.Cache == nil {
		http.Error(w, "session server unavailable", http.StatusServiceUnavailable)
		return
	}

	session, ok := s.authorizeSession(w, r)
	if !ok {
		return
	}

	artifact, ok := sessionArtifact(session, r.PathValue("artifact"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	image, ok := sessionArtifactImage(session, artifact.Source)
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

func (s *SessionHTTPServer) authorizeSession(w http.ResponseWriter, r *http.Request) (*v1alpha3.NetbootSession, bool) {
	if s.Client == nil || s.Capabilities == nil {
		http.Error(w, "session server unavailable", http.StatusServiceUnavailable)
		return nil, false
	}

	var session v1alpha3.NetbootSession
	if err := s.Client.Get(r.Context(), client.ObjectKey{Name: r.PathValue("session")}, &session); err != nil {
		if apierrors.IsNotFound(err) {
			http.NotFound(w, r)
			return nil, false
		}
		http.Error(w, "loading session", http.StatusServiceUnavailable)
		return nil, false
	}
	if err := s.Capabilities.Verify(&session, r.PathValue("capability")); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	if session.Status.Phase != v1alpha3.NetbootSessionPhaseReady && session.Status.Phase != v1alpha3.NetbootSessionPhaseActive {
		http.Error(w, "session unavailable", http.StatusServiceUnavailable)
		return nil, false
	}

	return &session, true
}

func sessionMilestoneCondition(milestone string) (string, bool) {
	switch milestone {
	case "boot-loader-downloaded":
		return v1alpha3.NetbootSessionConditionBootLoaderDownloaded, true
	case "boot-image-written":
		return v1alpha3.NetbootSessionConditionBootImageWritten, true
	case "cloud-init-done":
		return v1alpha3.NetbootSessionConditionCloudInitDone, true
	case "attested":
		return v1alpha3.NetbootSessionConditionAttested, true
	default:
		return "", false
	}
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
