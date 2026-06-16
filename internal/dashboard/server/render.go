// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package server

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"

	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/dashboard/contract"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// navItem is a single left-nav entry rendered by the base layout.
type navItem struct {
	Title  string
	Href   string
	Active bool
}

// pageData is the top-level value passed to every page template.
type pageData struct {
	Nav  []navItem
	Data any
}

// templateFuncs are the shared helpers available to all templates. They keep
// status colour decisions in one place so modules stay consistent.
var templateFuncs = template.FuncMap{
	"healthClass": healthClass,
	"healthLabel": healthLabel,
}

func healthClass(h contract.Health) string {
	switch h {
	case contract.HealthOK:
		return "success"
	case contract.HealthWarning:
		return "warning"
	case contract.HealthError:
		return "danger"
	default:
		return "secondary"
	}
}

func healthLabel(h contract.Health) string {
	switch h {
	case contract.HealthOK:
		return "OK"
	case contract.HealthWarning:
		return "Warning"
	case contract.HealthError:
		return "Error"
	default:
		return "Unknown"
	}
}

// renderer parses each page template together with the base layout and shared
// partials, then renders them with the shared func map.
type renderer struct {
	pages map[string]*template.Template
}

// pageNames are the content templates rendered through the base layout.
var pageNames = []string{
	"overview",
	"modules",
	"module",
	"resources",
	"detail",
	"error",
}

func newRenderer() (*renderer, error) {
	base, err := templatesFS.ReadFile("templates/base.html")
	if err != nil {
		return nil, fmt.Errorf("reading base template: %w", err)
	}

	partials, err := templatesFS.ReadFile("templates/partials.html")
	if err != nil {
		return nil, fmt.Errorf("reading partials template: %w", err)
	}

	r := &renderer{pages: make(map[string]*template.Template, len(pageNames))}

	for _, name := range pageNames {
		page, err := templatesFS.ReadFile("templates/" + name + ".html")
		if err != nil {
			return nil, fmt.Errorf("reading %s template: %w", name, err)
		}

		tmpl := template.New("base").Funcs(templateFuncs)

		for _, src := range [][]byte{base, partials, page} {
			if _, err := tmpl.Parse(string(src)); err != nil {
				return nil, fmt.Errorf("parsing %s template: %w", name, err)
			}
		}

		r.pages[name] = tmpl
	}

	return r, nil
}

// render writes the named page to w. On template execution failure it logs and
// emits a 500 (only safe before any bytes are written, so it buffers first).
func (r *renderer) render(w http.ResponseWriter, status int, page string, data pageData) {
	tmpl, ok := r.pages[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "base", data); err != nil {
		klog.Errorf("dashboard: rendering page %q: %v", page, err)
		http.Error(w, "internal error rendering page", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	if _, err := buf.WriteTo(w); err != nil {
		klog.V(4).Infof("dashboard: writing page %q: %v", page, err)
	}
}

// staticHandler serves the embedded static assets under /static/.
func staticHandler() (http.Handler, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("creating static sub-filesystem: %w", err)
	}

	return http.StripPrefix("/static/", http.FileServer(http.FS(sub))), nil
}
