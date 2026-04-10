// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"text/template"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/metalman/indexing"
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

type FileResolver struct {
	Cache             *OCICache
	Reader            client.Reader
	ApiserverURL      string
	ServeURL          string
	KubernetesVersion string
	ClusterDNS        string
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

func (f *FileResolver) ResolveFileByPath(ctx context.Context, path string, node *v1alpha3.Machine, imageRef string) (*ResolvedFile, error) {
	diskPath, isTemplate, err := f.Cache.ResolvePath(imageRef, path)
	if err != nil {
		// Check if the image just hasn't been pulled yet
		digest := f.Cache.DigestFor(imageRef)
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
			agentImage := ""
			if node.Spec.Agent != nil {
				agentImage = node.Spec.Agent.Image
			}

			data, err := renderTemplate(string(content), templateData{
				Machine:           node,
				ApiserverURL:      f.ApiserverURL,
				ServeURL:          f.ServeURL,
				KubernetesVersion: f.KubernetesVersion,
				ClusterDNS:        f.ClusterDNS,
				AgentImage:        agentImage,
			})
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

type templateData struct {
	Machine           *v1alpha3.Machine
	ApiserverURL      string
	ServeURL          string
	KubernetesVersion string
	ClusterDNS        string
	AgentImage        string
}

var (
	templateFuncMap = template.FuncMap{}
	templatePool    = sync.Pool{
		New: func() any {
			return new(bytes.Buffer)
		},
	}
)

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
