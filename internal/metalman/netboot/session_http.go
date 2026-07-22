// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	pathpkg "path"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
)

// SessionHTTPServer serves only immutable artifacts authorized by a
// session-scoped capability.
type SessionHTTPServer struct {
	Client            client.Reader
	Cache             *OCICache
	Capabilities      *CapabilitySigner
	StatusRecorder    SessionConditionRecorder
	EdgeAuthenticator EdgeAuthenticator
	Attestation       SessionAttester
}

// EdgeAuthenticator validates access to internal edge-only routes.
type EdgeAuthenticator interface {
	Authenticate(ctx context.Context, request *http.Request) bool
}

// SessionConditionRecorder persists a milestone for an exact session identity.
type SessionConditionRecorder interface {
	RecordCondition(ctx context.Context, sessionName string, sessionUID types.UID, condition metav1.Condition) error
}

// SessionAttester performs TPM attestation for an exact authenticated Machine.
type SessionAttester interface {
	AttestMachine(w http.ResponseWriter, r *http.Request, machine *v1alpha3.Machine)
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
	baseURL, err := SessionBaseURL(signer, session)
	if err != nil {
		return "", err
	}

	return JoinServeURLPath(baseURL, pathpkg.Join("artifacts", cleanArtifact))
}

// SessionBaseURL returns the externally advertised capability root for a
// session's artifacts and callbacks.
func SessionBaseURL(signer *CapabilitySigner, session *v1alpha3.NetbootSession) (string, error) {
	if signer == nil || session == nil {
		return "", errors.New("capability signer and session are required")
	}
	capability, err := signer.Sign(session)
	if err != nil {
		return "", err
	}

	return JoinServeURLPath(session.Spec.Endpoint.ExternalURL, pathpkg.Join("v1/netboot/sessions", session.Name, capability))
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
	mux.HandleFunc("POST /v1/netboot/sessions/{session}/{capability}/logs/agent-install", s.handleInstallLog)
	mux.HandleFunc("POST /v1/netboot/sessions/{session}/{capability}/attest", s.handleAttest)
	mux.HandleFunc("GET /v1/netboot/endpoints/{endpoint}/dhcp/{mac}", s.handleDHCPDecision)
}

func (s *SessionHTTPServer) handleDHCPDecision(w http.ResponseWriter, r *http.Request) {
	if s.Client == nil || s.Capabilities == nil {
		http.Error(w, "session server unavailable", http.StatusServiceUnavailable)
		return
	}
	if s.EdgeAuthenticator != nil && !s.EdgeAuthenticator.Authenticate(r.Context(), r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	endpoint := r.PathValue("endpoint")
	mac := strings.ToLower(r.PathValue("mac"))
	var sessions v1alpha3.NetbootSessionList
	if err := s.Client.List(r.Context(), &sessions); err != nil {
		http.Error(w, "loading sessions", http.StatusServiceUnavailable)
		return
	}

	matches := make([]*v1alpha3.NetbootSession, 0, 1)
	for i := range sessions.Items {
		session := &sessions.Items[i]
		if session.Spec.Endpoint.Name != endpoint || s.Capabilities.IsExpired(session) || (session.Status.Phase != v1alpha3.NetbootSessionPhaseReady && session.Status.Phase != v1alpha3.NetbootSessionPhaseActive) {
			continue
		}
		for _, lease := range session.Spec.Boot.DHCPLeases {
			if strings.EqualFold(lease.MAC, mac) {
				matches = append(matches, session)
				break
			}
		}
	}
	if len(matches) == 0 {
		http.NotFound(w, r)
		return
	}
	if len(matches) != 1 {
		http.Error(w, "multiple ready sessions match DHCP client", http.StatusConflict)
		return
	}

	session := matches[0]
	var lease *v1alpha3.DHCPLease
	for i := range session.Spec.Boot.DHCPLeases {
		if strings.EqualFold(session.Spec.Boot.DHCPLeases[i].MAC, mac) {
			lease = &session.Spec.Boot.DHCPLeases[i]
			break
		}
	}
	if lease == nil {
		http.NotFound(w, r)
		return
	}

	bootFile := ""
	if session.Spec.Boot.ConfigurationSource == v1alpha3.NetbootConfigurationSourceDHCP {
		var err error
		bootFile, err = SessionArtifactURL(s.Capabilities, session, session.Spec.Boot.FirmwareArtifact)
		if err != nil {
			http.Error(w, "building firmware URL", http.StatusServiceUnavailable)
			return
		}
		if session.Spec.Boot.Transport == v1alpha3.NetbootTransportTFTP {
			capability, err := s.Capabilities.Sign(session)
			if err != nil {
				http.Error(w, "building firmware capability", http.StatusServiceUnavailable)
				return
			}
			bootFile = pathpkg.Join("v1/netboot/sessions", session.Name, capability, "artifacts", session.Spec.Boot.FirmwareArtifact)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Lease     v1alpha3.DHCPLease        `json:"lease"`
		Transport v1alpha3.NetbootTransport `json:"transport"`
		BootFile  string                    `json:"bootFile"`
	}{Lease: *lease, Transport: session.Spec.Boot.Transport, BootFile: bootFile}); err != nil {
		slog.Warn("encoding DHCP decision", "err", err)
	}
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

	milestone := r.PathValue("milestone")
	if milestone == "cloud-init" {
		s.handleSessionCloudInit(w, r, session)
		return
	}

	conditionType, ok := sessionMilestoneCondition(milestone)
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

func (s *SessionHTTPServer) handleSessionCloudInit(w http.ResponseWriter, r *http.Request, session *v1alpha3.NetbootSession) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading cloud-init event", http.StatusBadRequest)
		return
	}
	var event cloudInitEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid cloud-init event", http.StatusBadRequest)
		return
	}
	condition := buildCloudInitCondition(&event, session.Spec.Machine.Generation)
	if condition != nil {
		condition.Type = v1alpha3.NetbootSessionConditionCloudInitDone
		if err := s.StatusRecorder.RecordCondition(r.Context(), session.Name, session.UID, *condition); err != nil {
			http.Error(w, "recording session status", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *SessionHTTPServer) handleInstallLog(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authorizeSession(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading install log", http.StatusBadRequest)
		return
	}
	slog.Warn("unbounded-agent install log", "session", session.Name, "body", strings.TrimSpace(string(body)))
	w.WriteHeader(http.StatusNoContent)
}

func (s *SessionHTTPServer) handleAttest(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authorizeSession(w, r)
	if !ok {
		return
	}
	if s.Attestation == nil || s.StatusRecorder == nil {
		http.Error(w, "session attestation unavailable", http.StatusServiceUnavailable)
		return
	}

	var machine v1alpha3.Machine
	if err := s.Client.Get(r.Context(), client.ObjectKey{Name: session.Spec.Machine.Name}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "loading Machine", http.StatusServiceUnavailable)
		return
	}
	if machine.UID != session.Spec.Machine.UID {
		http.Error(w, "Machine identity changed", http.StatusConflict)
		return
	}

	response := newBufferedResponseWriter()
	s.Attestation.AttestMachine(response, r, &machine)
	if response.status < http.StatusOK || response.status >= http.StatusMultipleChoices {
		response.writeTo(w)
		return
	}
	if err := s.StatusRecorder.RecordCondition(r.Context(), session.Name, session.UID, metav1.Condition{
		Type:    v1alpha3.NetbootSessionConditionAttested,
		Status:  metav1.ConditionTrue,
		Reason:  "Succeeded",
		Message: "TPM attestation succeeded",
	}); err != nil {
		http.Error(w, "recording session status", http.StatusServiceUnavailable)
		return
	}
	response.writeTo(w)
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
	if artifact.Source == "Session" {
		if artifact.Name != "cloud-init/user-data" {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, artifact.Name, session.CreationTimestamp.Time, strings.NewReader(session.Spec.Provisioning.UserData))
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
		data, err := s.renderSessionTemplate(diskPath, session)
		if err != nil {
			http.Error(w, "rendering artifact", http.StatusServiceUnavailable)
			return
		}
		http.ServeContent(w, r, artifact.Name, session.CreationTimestamp.Time, bytes.NewReader(data))
		s.recordFirmwareDownloaded(r.Context(), session, artifact.Name)
		return
	}

	http.ServeFile(w, r, diskPath)
	s.recordFirmwareDownloaded(r.Context(), session, artifact.Name)
}

func (s *SessionHTTPServer) recordFirmwareDownloaded(ctx context.Context, session *v1alpha3.NetbootSession, artifactName string) {
	if s.StatusRecorder == nil || artifactName != session.Spec.Boot.FirmwareArtifact {
		return
	}
	if err := s.StatusRecorder.RecordCondition(ctx, session.Name, session.UID, metav1.Condition{
		Type:    v1alpha3.NetbootSessionConditionBootLoaderDownloaded,
		Status:  metav1.ConditionTrue,
		Reason:  "Succeeded",
		Message: "firmware artifact downloaded",
	}); err != nil {
		slog.Warn("recording firmware artifact download", "session", session.Name, "err", err)
	}
}

func (s *SessionHTTPServer) renderSessionTemplate(templatePath string, session *v1alpha3.NetbootSession) ([]byte, error) {
	content, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("reading template: %w", err)
	}
	baseURL, err := SessionBaseURL(s.Capabilities, session)
	if err != nil {
		return nil, err
	}
	machine := sessionMachine(session)
	cluster := session.Spec.Provisioning.Cluster
	agentConfig := provision.BuildAgentConfig(provision.BuildAgentConfigParams{
		Machine: machine,
		Cluster: provision.ClusterEndpoint{
			APIServer:    cluster.APIServerURL,
			CACertBase64: cluster.CACertBase64,
			ClusterDNS:   cluster.DNS,
			KubeVersion:  cluster.KubernetesVersion,
		},
		ProviderLabels: session.Spec.Provisioning.ProviderLabels,
		AttestURL:      baseURL,
	})
	agentConfigJSON, err := json.MarshalIndent(agentConfig, "    ", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling agent config: %w", err)
	}
	data := newTemplateData(machine, ClusterInfo{ApiserverURL: cluster.APIServerURL, CACertBase64: cluster.CACertBase64}, baseURL, string(agentConfigJSON), "", true)
	data.ArtifactBaseURL, err = JoinServeURLPath(baseURL, "artifacts")
	if err != nil {
		return nil, err
	}
	data.BootImageWrittenURL, err = JoinServeURLPath(baseURL, "callbacks/boot-image-written")
	if err != nil {
		return nil, err
	}
	data.CloudInitURL, err = JoinServeURLPath(baseURL, "callbacks/cloud-init")
	if err != nil {
		return nil, err
	}
	data.InstallLogURL, err = JoinServeURLPath(baseURL, "logs/agent-install")
	if err != nil {
		return nil, err
	}

	return renderTemplate(string(content), data)
}

func sessionMachine(session *v1alpha3.NetbootSession) *v1alpha3.Machine {
	return &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: session.Spec.Machine.Name, UID: session.Spec.Machine.UID, Generation: session.Spec.Machine.Generation},
		Spec: v1alpha3.MachineSpec{
			Host: &v1alpha3.HostSpec{Netboot: &v1alpha3.PXESpec{
				Transport:           session.Spec.Boot.Transport,
				ConfigurationSource: session.Spec.Boot.ConfigurationSource,
				NetworkMode:         session.Spec.Boot.NetworkMode,
				Architecture:        session.Spec.Boot.Architecture,
				DHCPLeases:          append([]v1alpha3.DHCPLease(nil), session.Spec.Boot.DHCPLeases...),
				TargetDisk:          session.Spec.Boot.TargetDisk,
			}},
			Kubernetes: session.Spec.Provisioning.Kubernetes.DeepCopy(),
			Agent:      session.Spec.Provisioning.Agent.DeepCopy(),
		},
	}
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
	default:
		return "", false
	}
}

type bufferedResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{header: make(http.Header), status: http.StatusOK}
}

func (w *bufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *bufferedResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *bufferedResponseWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

func (w *bufferedResponseWriter) writeTo(destination http.ResponseWriter) {
	for key, values := range w.header {
		destination.Header()[key] = append([]string(nil), values...)
	}
	destination.WriteHeader(w.status)
	if _, err := destination.Write(w.body.Bytes()); err != nil {
		slog.Warn("writing buffered attestation response", "err", err)
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
