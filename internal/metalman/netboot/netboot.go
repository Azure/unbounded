// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	pathpkg "path"
	"strings"
	"sync"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	if node == nil || node.Spec.PXE == nil {
		return ""
	}

	if node.Spec.PXE.NetbootImage != "" {
		return node.Spec.PXE.NetbootImage
	}

	if f.DefaultNetbootRef != "" {
		return f.DefaultNetbootRef
	}

	return node.Spec.PXE.Image
}

func (f *FileResolver) HTTPBootPath(node *v1alpha3.Machine) (string, error) {
	if node == nil || node.Spec.PXE == nil {
		return "", fmt.Errorf("node has no PXE config")
	}

	imageRef := f.NetbootImageRef(node)
	if imageRef == "" {
		return "", fmt.Errorf("node %s has no netboot image", node.Name)
	}

	if f.Cache == nil {
		return "", fmt.Errorf("OCI cache is not configured")
	}

	meta, err := f.Cache.MetadataForRefArchitecture(imageRef, node.Spec.PXE.TargetArchitecture())
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
	if node != nil && node.Spec.PXE != nil {
		architecture = node.Spec.PXE.TargetArchitecture()
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

			data, err := renderTemplate(string(content), newTemplateData(node, ci, f.ServeURL, string(agentConfigJSON), requestIP))
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
	if node.Spec.PXE == nil || node.Spec.PXE.CloudInit == nil || node.Spec.PXE.CloudInit.UserDataConfigMapRef == nil {
		return nil, false, nil
	}

	ref := node.Spec.PXE.CloudInit.UserDataConfigMapRef

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
	AgentConfigJSON     string
	InstallScript       string
	InstallEnv          []string
	SpecRepaveCounter   int64
	StatusRepaveCounter int64
}

func newTemplateData(node *v1alpha3.Machine, ci ClusterInfo, serveURL, agentConfigJSON, requestIP string) templateData {
	var specRepave, statusRepave int64

	var (
		agent     *v1alpha3.AgentSpec
		bootLease *v1alpha3.DHCPLease
	)

	if node != nil {
		agent = node.Spec.Agent
		bootLease = selectBootLease(node, requestIP)

		if node.Spec.Operations != nil {
			specRepave = node.Spec.Operations.RepaveCounter
		}

		if node.Status.Operations != nil {
			statusRepave = node.Status.Operations.RepaveCounter
		}
	}

	return templateData{
		Machine:             node,
		BootLease:           bootLease,
		ApiserverURL:        ci.ApiserverURL,
		ServeURL:            serveURL,
		AgentConfigJSON:     agentConfigJSON,
		InstallScript:       provision.UnboundedAgentInstallScript(),
		InstallEnv:          provision.AgentInstallEnv(agent),
		SpecRepaveCounter:   specRepave,
		StatusRepaveCounter: statusRepave,
	}
}

func selectBootLease(node *v1alpha3.Machine, requestIP string) *v1alpha3.DHCPLease {
	if node == nil || node.Spec.PXE == nil || len(node.Spec.PXE.DHCPLeases) == 0 {
		return nil
	}

	for i := range node.Spec.PXE.DHCPLeases {
		if node.Spec.PXE.DHCPLeases[i].IPv4 == requestIP {
			return &node.Spec.PXE.DHCPLeases[i]
		}
	}

	return &node.Spec.PXE.DHCPLeases[0]
}

var (
	templateFuncMap = template.FuncMap{
		"indent": indentTemplateBlock,
	}
	templatePool = sync.Pool{
		New: func() any {
			return new(bytes.Buffer)
		},
	}
)

func indentTemplateBlock(spaces int, value string) string {
	if value == "" {
		return ""
	}

	prefix := strings.Repeat(" ", spaces)

	return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix)
}

func renderTemplate(tmplStr string, data templateData) ([]byte, error) {
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
