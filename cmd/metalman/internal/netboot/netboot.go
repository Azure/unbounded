package netboot

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
	"github.com/project-unbounded/unbounded-kube/cmd/metalman/internal/indexing"
)

// ErrNotYetDownloaded is returned when an HTTP-sourced file has not yet been
// downloaded to the local cache by the ImageReconciler.
var ErrNotYetDownloaded = errors.New("file not yet downloaded")

// ResolvedFile is the result of resolving a file from an Image spec. For
// HTTP-sourced files, DiskPath is set so callers can stream from disk. For
// template and static files, Data holds the rendered content.
type ResolvedFile struct {
	DiskPath    string // on-disk path for HTTP-sourced cached files
	Data        []byte // rendered content for template/static files
	ContentType string // MIME type hint for the response
}

type FileResolver struct {
	CacheDir     string
	Reader       client.Reader
	ApiserverURL string
	ServeURL     string
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
	var img v1alpha3.Image
	if err := f.Reader.Get(ctx, client.ObjectKey{Name: imageRef}, &img); err != nil {
		return nil, fmt.Errorf("image %q not found: %w", imageRef, err)
	}

	for _, file := range img.Spec.Files {
		if file.Path == path {
			return f.resolveFile(file, node, &img)
		}
	}

	return nil, fmt.Errorf("file not found: %s", path)
}

func (f *FileResolver) resolveFile(file v1alpha3.File, node *v1alpha3.Machine, img *v1alpha3.Image) (*ResolvedFile, error) {
	if file.HTTP != nil {
		diskPath := cachePath(f.CacheDir, file.HTTP.SHA256, file.HTTP.Convert)
		if _, err := os.Stat(diskPath); err != nil {
			if os.IsNotExist(err) {
				return nil, ErrNotYetDownloaded
			}

			return nil, fmt.Errorf("checking cached file %s: %w", file.Path, err)
		}

		return &ResolvedFile{DiskPath: diskPath}, nil
	}

	if file.Template != nil {
		if node != nil {
			data, err := renderTemplate(file.Template.Content, templateData{
				Machine:      node,
				Image:        img,
				ApiserverURL: f.ApiserverURL,
				ServeURL:     f.ServeURL,
			})
			if err != nil {
				return nil, err
			}

			return &ResolvedFile{Data: data, ContentType: "text/plain"}, nil
		}

		return &ResolvedFile{Data: []byte(file.Template.Content), ContentType: "text/plain"}, nil
	}

	if file.Static != nil {
		data, err := staticContent(file.Static)
		if err != nil {
			return nil, err
		}

		ct := "text/plain"
		if file.Static.Encoding == "base64" {
			ct = "application/octet-stream"
		}

		return &ResolvedFile{Data: data, ContentType: ct}, nil
	}

	return nil, fmt.Errorf("file %s has no source", file.Path)
}

func staticContent(s *v1alpha3.StaticSource) ([]byte, error) {
	if s.Encoding == "base64" {
		return base64.StdEncoding.DecodeString(s.Content)
	}

	return []byte(s.Content), nil
}

type templateData struct {
	Machine      *v1alpha3.Machine
	Image        *v1alpha3.Image
	ApiserverURL string
	ServeURL     string
}

func renderTemplate(tmplStr string, data templateData) ([]byte, error) {
	t, err := template.New("").Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("executing template: %w", err)
	}

	return buf.Bytes(), nil
}

// cachePath returns the on-disk path for a content-addressed cached file
// identified by its SHA256 checksum and optional conversion method.
func cachePath(cacheDir, sha256sum, convert string) string {
	name := sha256sum
	if convert != "" {
		name += "." + convert
	}

	return filepath.Join(cacheDir, "sha256", name)
}
