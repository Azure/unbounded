// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	pathpkg "path"
	"strconv"
	"strings"
	"sync"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/indexing"
	"github.com/Azure/unbounded/internal/provision"
)

// ErrNotYetDownloaded is returned when an OCI image has not yet been
// pulled and unpacked to the local cache.
var ErrNotYetDownloaded = fmt.Errorf("file not yet downloaded")

// ResolvedFile is the result of resolving a file from an OCI image.
// For static files on disk, DiskPath is set so callers can stream from disk.
// For template files, Data holds the rendered content.
type ResolvedFile struct {
	DiskPath    string // on-disk path for static files
	Data        []byte // rendered content for template files
	ContentType string // MIME type hint for the response
}

// ClusterInfo holds the API server URL and CA certificate discovered from
// the cluster-info ConfigMap in kube-public. These values may change at
// runtime (e.g. API server URL rotation), so they are provided through
// ClusterInfoProvider rather than stored statically.
type ClusterInfo struct {
	ApiserverURL string
	CACertBase64 string
}

// ClusterInfoProvider returns the current cluster-info snapshot.
// Implementations should be safe for concurrent use.
type ClusterInfoProvider interface {
	ClusterInfo() ClusterInfo
}

// StaticClusterInfo is a ClusterInfoProvider that returns a fixed
// configuration. Useful for tests and simple deployments where runtime
// refresh is not needed.
type StaticClusterInfo struct {
	Info ClusterInfo
}

func (s *StaticClusterInfo) ClusterInfo() ClusterInfo { return s.Info }

type FileResolver struct {
	Cache             *OCICache
	Reader            client.Reader
	Cluster           ClusterInfoProvider
	ServeURL          string
	DefaultNetbootRef string
	KubernetesVersion string
	ClusterDNS        string
	ProviderLabels    map[string]string
}

func (f *FileResolver) NetbootImageRef(node *v1alpha3.Machine) string {
	if node == nil || node.Spec.Netboot() == nil {
		return ""
	}

	if node.Spec.Netboot().NetbootImage != "" {
		return node.Spec.Netboot().NetbootImage
	}

	if f.DefaultNetbootRef != "" {
		return f.DefaultNetbootRef
	}

	return node.Spec.Netboot().Image
}

func (f *FileResolver) HTTPBootPath(node *v1alpha3.Machine) (string, error) {
	if node == nil || node.Spec.Netboot() == nil {
		return "", fmt.Errorf("node has no PXE config")
	}

	imageRef := f.NetbootImageRef(node)
	if imageRef == "" {
		return "", fmt.Errorf("node %s has no netboot image", node.Name)
	}

	if f.Cache == nil {
		return "", fmt.Errorf("OCI cache is not configured")
	}

	meta, err := f.Cache.MetadataForRefArchitecture(imageRef, node.Spec.Netboot().TargetArchitecture())
	if err != nil {
		return "", err
	}

	return HTTPBootPathFromMetadata(meta), nil
}

func (f *FileResolver) HTTPBootURL(node *v1alpha3.Machine) (string, error) {
	path, err := f.HTTPBootPath(node)
	if err != nil {
		return "", err
	}

	return JoinServeURLPath(f.ServeURL, path)
}

func HTTPBootPathFromMetadata(meta *ImageMetadata) string {
	if meta == nil {
		return ""
	}

	if meta.HTTPBootPath != "" {
		return strings.TrimPrefix(meta.HTTPBootPath, "/")
	}

	return strings.TrimPrefix(meta.DHCPBootImageName, "/")
}

func JoinServeURLPath(serveURL, path string) (string, error) {
	if strings.TrimSpace(serveURL) == "" {
		return "", fmt.Errorf("serve URL is not configured")
	}

	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", fmt.Errorf("HTTP boot path is not configured")
	}

	cleanPath := pathpkg.Clean(path)
	if cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return "", fmt.Errorf("invalid HTTP boot path %q: resolves outside cache directory", path)
	}

	base, err := url.Parse(serveURL)
	if err != nil {
		return "", fmt.Errorf("parsing serve URL: %w", err)
	}

	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("serve URL %q must be absolute", serveURL)
	}

	base.Path = strings.TrimRight(base.Path, "/") + "/" + cleanPath
	base.RawQuery = ""
	base.Fragment = ""

	return base.String(), nil
}

func (f *FileResolver) LookupNodeByIP(ctx context.Context, ip string) (*v1alpha3.Machine, error) {
	var nodes v1alpha3.MachineList
	if err := f.Reader.List(ctx, &nodes, client.MatchingFields{indexing.IndexNodeByIP: ip}); err != nil {
		return nil, fmt.Errorf("looking up node by IP: %w", err)
	}

	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("no node found for IP %s", ip)
	}

	return &nodes.Items[0], nil
}

const userDataPath = "cloud-init/user-data"

// defaultUserData is the minimal cloud-config returned when no custom
// user-data ConfigMap is configured on the Machine.
const defaultUserData = "#cloud-config\n"

func (f *FileResolver) ResolveFileByPath(ctx context.Context, path string, node *v1alpha3.Machine, imageRef string) (*ResolvedFile, error) {
	return f.ResolveFileByPathForIP(ctx, path, node, imageRef, "")
}

func (f *FileResolver) ResolveFileByPathForIP(ctx context.Context, path string, node *v1alpha3.Machine, imageRef, requestIP string) (*ResolvedFile, error) {
	if path == userDataPath && node != nil {
		if data, ok, err := f.resolveUserDataFromConfigMap(ctx, node); err != nil {
			return nil, fmt.Errorf("resolving user-data from ConfigMap: %w", err)
		} else if ok {
			return &ResolvedFile{Data: data, ContentType: "text/plain"}, nil
		}

		return &ResolvedFile{Data: []byte(defaultUserData), ContentType: "text/plain"}, nil
	}

	architecture := v1alpha3.DefaultPXEArchitecture
	if node != nil && node.Spec.Netboot() != nil {
		architecture = node.Spec.Netboot().TargetArchitecture()
	}

	diskPath, isTemplate, err := f.Cache.ResolvePathForArchitecture(imageRef, architecture, path)
	if err != nil {
		// Check if the image just hasn't been pulled yet
		digest := f.Cache.DigestForArchitecture(imageRef, architecture)
		if digest == "" {
			return nil, ErrNotYetDownloaded
		}

		return nil, fmt.Errorf("file not found: %s", path)
	}

	if isTemplate {
		content, err := os.ReadFile(diskPath)
		if err != nil {
			return nil, fmt.Errorf("reading template %s: %w", path, err)
		}

		if node != nil {
			ci := f.Cluster.ClusterInfo()

			agentConfig := provision.BuildAgentConfig(provision.BuildAgentConfigParams{
				Machine: node,
				Cluster: provision.ClusterEndpoint{
					APIServer:    ci.ApiserverURL,
					CACertBase64: ci.CACertBase64,
					ClusterDNS:   f.ClusterDNS,
					KubeVersion:  f.KubernetesVersion,
				},
				ProviderLabels: f.ProviderLabels,
				AttestURL:      f.ServeURL,
			})

			// The MarshalIndent prefix "    " (4 spaces) must match the
			// indentation level of the {{ .AgentConfigJSON }} placeholder
			// inside vendor-data.tmpl so that all lines of the multi-line
			// JSON are properly indented within the YAML content: | block.
			agentConfigJSON, err := json.MarshalIndent(agentConfig, "    ", "  ")
			if err != nil {
				return nil, fmt.Errorf("marshal agent config: %w", err)
			}

			installRequested, err := f.installRequested(ctx, node)
			if err != nil {
				return nil, err
			}

			data, err := renderTemplate(string(content), newTemplateData(node, ci, f.ServeURL, string(agentConfigJSON), requestIP, installRequested))
			if err != nil {
				return nil, err
			}

			return &ResolvedFile{Data: data, ContentType: "text/plain"}, nil
		}

		// No node context - return template content verbatim
		return &ResolvedFile{Data: content, ContentType: "text/plain"}, nil
	}

	// Static file - serve from disk
	return &ResolvedFile{DiskPath: diskPath}, nil
}

func (f *FileResolver) resolveUserDataFromConfigMap(ctx context.Context, node *v1alpha3.Machine) ([]byte, bool, error) {
	if node.Spec.Netboot() == nil || node.Spec.Netboot().CloudInit == nil || node.Spec.Netboot().CloudInit.UserDataConfigMapRef == nil {
		return nil, false, nil
	}

	ref := node.Spec.Netboot().CloudInit.UserDataConfigMapRef

	var cm corev1.ConfigMap
	if err := f.Reader.Get(ctx, client.ObjectKey{
		Namespace: ref.Namespace,
		Name:      ref.Name,
	}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("getting ConfigMap %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	key := ref.Key
	if key == "" {
		key = "user-data"
	}

	if data, ok := cm.Data[key]; ok {
		return []byte(data), true, nil
	}

	if data, ok := cm.BinaryData[key]; ok {
		return data, true, nil
	}

	return nil, false, fmt.Errorf("key %q not found in ConfigMap %s/%s", key, ref.Namespace, ref.Name)
}

type templateData struct {
	Machine             *v1alpha3.Machine
	BootLease           *v1alpha3.DHCPLease
	ApiserverURL        string
	ServeURL            string
	ArtifactBaseURL     string
	BootImageWrittenURL string
	CloudInitURL        string
	InstallLogURL       string
	AgentConfigJSON     string
	InstallScript       string
	InstallEnv          []string
	InstallRequested    bool
}

func newTemplateData(node *v1alpha3.Machine, ci ClusterInfo, serveURL, agentConfigJSON, requestIP string, installRequested bool) templateData {
	var (
		agent     *v1alpha3.AgentSpec
		bootLease *v1alpha3.DHCPLease
	)

	if node != nil {
		agent = node.Spec.Agent
		bootLease = selectBootLease(node, requestIP)
	}

	serveURL = strings.TrimRight(serveURL, "/")

	return templateData{
		Machine:             node,
		BootLease:           bootLease,
		ApiserverURL:        ci.ApiserverURL,
		ServeURL:            serveURL,
		ArtifactBaseURL:     serveURL,
		BootImageWrittenURL: serveURL + "/pxe/disable",
		CloudInitURL:        serveURL + "/cloudinit/log",
		InstallLogURL:       serveURL + "/unbounded-agent/install-log",
		AgentConfigJSON:     agentConfigJSON,
		InstallScript:       provision.UnboundedAgentInstallScript(),
		InstallEnv:          provision.AgentInstallEnv(agent),
		InstallRequested:    installRequested,
	}
}

func selectBootLease(node *v1alpha3.Machine, requestIP string) *v1alpha3.DHCPLease {
	if node == nil || node.Spec.Netboot() == nil || len(node.Spec.Netboot().DHCPLeases) == 0 {
		return nil
	}

	for i := range node.Spec.Netboot().DHCPLeases {
		if node.Spec.Netboot().DHCPLeases[i].IPv4 == requestIP {
			return &node.Spec.Netboot().DHCPLeases[i]
		}
	}

	return &node.Spec.Netboot().DHCPLeases[0]
}

func (f *FileResolver) installRequested(ctx context.Context, node *v1alpha3.Machine) (bool, error) {
	if f.Reader == nil || node == nil {
		return false, nil
	}

	var list v1alpha3.MachineOperationList
	if err := f.Reader.List(ctx, &list); err != nil {
		return false, fmt.Errorf("list MachineOperations: %w", err)
	}

	for _, op := range list.Items {
		if op.Spec.OperationKind != v1alpha3.OperationHostReplace || op.Status.IsTerminal() {
			continue
		}

		if operationRequestsInstall(&op, node.Name) {
			return true, nil
		}
	}

	return false, nil
}

func operationRequestsInstall(op *v1alpha3.MachineOperation, machineName string) bool {
	if cond := apimeta.FindStatusCondition(op.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten); cond != nil && cond.Status == metav1.ConditionTrue {
		return false
	}

	for _, target := range op.Status.Targets {
		if target.MachineRef != machineName {
			continue
		}

		return target.Phase != v1alpha3.OperationPhaseComplete && target.Phase != v1alpha3.OperationPhaseFailed
	}

	return len(op.Status.Targets) == 0 && op.Spec.MachineRef == machineName
}

var (
	templateFuncMap = template.FuncMap{
		"indent":       indentTemplateBlock,
		"ipAddresses":  ipAddresses,
		"join":         strings.Join,
		"subnetPrefix": subnetPrefix,
		"yamlQuote":    strconv.Quote,
	}
	templatePool = sync.Pool{
		New: func() any {
			return new(bytes.Buffer)
		},
	}
)

func ipAddresses(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if net.ParseIP(value) != nil {
			result = append(result, value)
		}
	}

	return result
}

func subnetPrefix(mask string) (int, error) {
	ip := net.ParseIP(mask)
	if ip == nil || ip.To4() == nil {
		return 0, fmt.Errorf("invalid IPv4 subnet mask %q", mask)
	}

	ones, bits := net.IPMask(ip.To4()).Size()
	if bits != net.IPv4len*8 || ones < 0 {
		return 0, fmt.Errorf("non-contiguous IPv4 subnet mask %q", mask)
	}

	return ones, nil
}

func indentTemplateBlock(spaces int, value string) string {
	if value == "" {
		return ""
	}

	prefix := strings.Repeat(" ", spaces)

	return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix)
}

func renderTemplate(tmplStr string, data templateData) ([]byte, error) {
	serveURL := strings.TrimRight(data.ServeURL, "/")
	if data.ArtifactBaseURL == "" {
		data.ArtifactBaseURL = serveURL
	}
	if data.BootImageWrittenURL == "" {
		data.BootImageWrittenURL = serveURL + "/pxe/disable"
	}
	if data.CloudInitURL == "" {
		data.CloudInitURL = serveURL + "/cloudinit/log"
	}
	if data.InstallLogURL == "" {
		data.InstallLogURL = serveURL + "/unbounded-agent/install-log"
	}

	t, err := template.New("").Funcs(templateFuncMap).Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}

	buf, ok := templatePool.Get().(*bytes.Buffer)
	if !ok {
		buf = new(bytes.Buffer)
	}

	buf.Reset()

	defer templatePool.Put(buf)

	if err := t.Execute(buf, data); err != nil {
		return nil, fmt.Errorf("executing template: %w", err)
	}

	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())

	return result, nil
}
