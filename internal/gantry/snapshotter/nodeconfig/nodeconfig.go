// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package nodeconfig merges gantry-snapshotter's containerd configuration into
// a node's existing one.
//
// It exists because the configuration cannot be shipped as a file. containerd's
// config is a single document a node's own installer already owns: on AKS it
// pins the pause image, the registry config path and the runc handler's cgroup
// driver, and overwriting any of that breaks the node. Nor can it be appended
// to as text, because the tables this needs to reach into are tables that
// installer has already written. So the merge is done on the parsed document,
// and only the keys named here are touched.
//
// The merge is in two phases, and the order is load-bearing rather than
// stylistic. Phase one is inert: it registers the proxy plugin and a runtime
// handler that deliberately still uses overlayfs. Phase two makes the
// snapshotter containerd's default, which is the point at which every new
// container on the node depends on a socket in a tmpfs. A node that had both at
// once and then rebooted would need that socket to start the very pod that
// creates it. Phase one's runtime handler is what breaks that cycle, and it has
// to already be in force before phase two is written.
package nodeconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	toml "github.com/pelletier/go-toml/v2"
)

// Phase names one half of the configuration.
type Phase int

const (
	// PhaseBootstrap registers the proxy plugin and the bootstrap runtime
	// handler. Applying it changes nothing about how images are unpacked.
	PhaseBootstrap Phase = 1

	// PhaseDefault makes the snapshotter containerd's default. It is only safe
	// once the snapshotter is actually serving on this node.
	PhaseDefault Phase = 2
)

// Defaults for everything the caller does not pin.
const (
	// DefaultSnapshotter is the proxy plugin name. It is what appears in
	// `ctr plugin ls` and what a CRI snapshotter setting refers to.
	DefaultSnapshotter = "gantry"

	// DefaultBootstrapRuntime is the runtime handler that keeps using
	// overlayfs. The snapshotter's own pod runs under it.
	DefaultBootstrapRuntime = "gantry-bootstrap"

	// DefaultSocket is where the snapshotter listens.
	DefaultSocket = "/run/gantry-snapshotter/snapshotter.sock"

	// DefaultConfigPath is containerd's configuration file.
	DefaultConfigPath = "/etc/containerd/config.toml"
)

// Options are the knobs the merge takes.
type Options struct {
	// Snapshotter is the proxy plugin name.
	Snapshotter string

	// BootstrapRuntime is the name of the runtime handler that stays on
	// overlayfs.
	BootstrapRuntime string

	// Socket is the snapshotter's listen address.
	Socket string

	// Platform is the platform whose unpack path is pointed at the
	// snapshotter. Empty means the running one.
	Platform string
}

func (o Options) withDefaults() Options {
	if o.Snapshotter == "" {
		o.Snapshotter = DefaultSnapshotter
	}

	if o.BootstrapRuntime == "" {
		o.BootstrapRuntime = DefaultBootstrapRuntime
	}

	if o.Socket == "" {
		o.Socket = DefaultSocket
	}

	return o
}

// ErrNoCRIPlugin is returned when the document has no recognisable CRI plugin
// configuration. Guessing one would mean writing a table containerd's own
// config migration might then rename, so the merge stops instead.
var ErrNoCRIPlugin = errors.New("no CRI plugin configuration found")

// Document is a parsed containerd configuration.
type Document map[string]any

// Load reads a containerd configuration. A missing file is an empty document,
// which is what containerd itself does with one.
func Load(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Document{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	doc := Document{}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	return doc, nil
}

// Save writes a containerd configuration, keeping a copy of what was there
// before the first time it replaces it.
//
// The backup is written once and never overwritten, so it is always the node's
// original configuration rather than the last one this wrote. That is the copy
// worth having: recovering a node means getting back to what its installer
// produced, not to a half-applied state.
func Save(path string, doc Document) error {
	data, err := toml.Marshal(map[string]any(doc))
	if err != nil {
		return fmt.Errorf("render containerd config: %w", err)
	}

	backup := path + ".gantry-orig"
	if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
		if original, err := os.ReadFile(path); err == nil {
			if err := writeFileAtomic(backup, original); err != nil {
				return fmt.Errorf("back up %s: %w", path, err)
			}
		}
	}

	return writeFileAtomic(path, data)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}

	defer os.Remove(tmp.Name()) //nolint:errcheck // best effort cleanup of a file that is gone on success

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck,gosec // the write error is the one worth reporting

		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}

	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close() //nolint:errcheck,gosec // the chmod error is the one worth reporting

		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck,gosec // the sync error is the one worth reporting

		return fmt.Errorf("sync %s: %w", tmp.Name(), err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}

	return os.Rename(tmp.Name(), path)
}

// Apply merges a phase into a document and reports whether anything changed.
//
// Reporting no change is what keeps the node quiet: the caller restarts
// containerd only when this says it has to, so the agent that owns this can run
// forever without ever touching a node that is already configured.
func Apply(doc Document, phase Phase, opts Options) (bool, error) {
	opts = opts.withDefaults()

	switch phase {
	case PhaseBootstrap:
		return applyBootstrap(doc, opts)
	case PhaseDefault:
		return applyDefault(doc, opts)
	}

	return false, fmt.Errorf("unknown phase %d", int(phase))
}

// applyBootstrap registers the proxy plugin and the bootstrap runtime handler.
func applyBootstrap(doc Document, opts Options) (bool, error) {
	layout, err := detect(doc)
	if err != nil {
		return false, err
	}

	changed := setPath(doc, []string{"proxy_plugins", opts.Snapshotter, "type"}, "snapshot")
	changed = setPath(doc, []string{"proxy_plugins", opts.Snapshotter, "address"}, opts.Socket) || changed

	// The handler is a copy of the node's real default rather than a minimal
	// table of its own. containerd fills an omitted options table with the
	// runtime's built-in defaults, not with the default handler's, so a handler
	// written from scratch would silently run containers with the wrong cgroup
	// driver.
	handler := map[string]any{}

	source, ok := lookup(doc, path(layout.runtimes, defaultRuntimeName(doc, layout))).(map[string]any)
	if ok {
		handler = deepCopy(source)
	}

	// Except for this: the whole purpose of the handler is to not be served by
	// the snapshotter it exists to bootstrap.
	handler["snapshotter"] = "overlayfs"

	target := path(layout.runtimes, opts.BootstrapRuntime)

	if existing, ok := lookup(doc, target).(map[string]any); ok && equal(existing, handler) {
		return changed, nil
	}

	return setPath(doc, target, handler) || changed, nil
}

// applyDefault makes the snapshotter containerd's default for images.
func applyDefault(doc Document, opts Options) (bool, error) {
	layout, err := detect(doc)
	if err != nil {
		return false, err
	}

	changed := setPath(doc, layout.snapshotter, opts.Snapshotter)

	// The snapshotter is told which image a layer belongs to, which digest it
	// has and which layers precede it through descriptor annotations. CRI
	// strips those when this is true, and without them every Prepare is a miss
	// with nothing to look up.
	changed = setPath(doc, layout.snapshotAnnotations, false) || changed

	// Ingest reads the layer tar back out of the content store after containerd
	// has unpacked it. Discarding it leaves nothing to read.
	changed = setPath(doc, layout.discardUnpacked, false) || changed

	if opts.Platform == "" {
		return changed, nil
	}

	// The transfer service unpacks into whichever snapshotter its own config
	// names, which is not the CRI default. An image pulled through it would
	// otherwise land somewhere the snapshotter never sees.
	unpack := []string{"plugins", "io.containerd.transfer.v1.local", "unpack_config"}

	entries, ok := lookup(doc, unpack).([]any)
	if !ok {
		entries = nil
	}

	for _, entry := range entries {
		row, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		if row["platform"] == opts.Platform {
			if row["snapshotter"] == opts.Snapshotter {
				return changed, nil
			}

			row["snapshotter"] = opts.Snapshotter

			return true, nil
		}
	}

	entries = append(entries, map[string]any{
		"platform":    opts.Platform,
		"snapshotter": opts.Snapshotter,
	})

	return setPath(doc, unpack, entries) || changed, nil
}

// Revert undoes phase two, leaving phase one in place, and reports whether
// anything changed.
//
// This is not an uninstall. It is what keeps a node able to start pods when the
// snapshotter is not serving. containerd unpacks every image through the CRI
// default snapshotter, and the runtime handler a pod names has no say in it, so
// once phase two is written a node whose socket is gone cannot pull anything -
// including the snapshotter's own image, which is exactly what an upgrade needs
// it to do. Dropping back to overlayfs breaks that cycle; phase one stays so the
// proxy plugin and the bootstrap handler survive, and phase two goes back on as
// soon as the socket answers again.
func Revert(doc Document, opts Options) (bool, error) {
	opts = opts.withDefaults()

	layout, err := detect(doc)
	if err != nil {
		return false, err
	}

	// Only the snapshotter itself is removed. The two annotation settings are
	// inert without it, and taking them out as well would mean a second
	// containerd restart to put them back for no gain.
	changed := deletePath(doc, layout.snapshotter)

	if opts.Platform == "" {
		return changed, nil
	}

	unpack := []string{"plugins", "io.containerd.transfer.v1.local", "unpack_config"}

	entries, ok := lookup(doc, unpack).([]any)
	if !ok {
		return changed, nil
	}

	kept := make([]any, 0, len(entries))

	for _, entry := range entries {
		row, ok := entry.(map[string]any)
		if ok && row["platform"] == opts.Platform && row["snapshotter"] == opts.Snapshotter {
			continue
		}

		kept = append(kept, entry)
	}

	if len(kept) == len(entries) {
		return changed, nil
	}

	if len(kept) == 0 {
		return deletePath(doc, unpack) || changed, nil
	}

	return setPath(doc, unpack, kept) || changed, nil
}

// layout is where the keys this cares about live, which depends on which
// generation of the CRI plugin the node's configuration uses.
type layout struct {
	runtimes            []string
	defaultRuntime      []string
	snapshotter         []string
	snapshotAnnotations []string
	discardUnpacked     []string
}

// detect works out which CRI plugin layout a document uses.
//
// containerd 2.x split the single CRI plugin into a runtime half and an images
// half, and a node that has been through the config migration carries the new
// names even where the document still declares the old version. So the shape is
// read off the document rather than off its version number.
func detect(doc Document) (layout, error) {
	plugins, ok := doc["plugins"].(map[string]any)
	if !ok {
		return layout{}, ErrNoCRIPlugin
	}

	if _, ok := plugins["io.containerd.cri.v1.images"]; ok {
		return splitLayout(), nil
	}

	if _, ok := plugins["io.containerd.cri.v1.runtime"]; ok {
		return splitLayout(), nil
	}

	if _, ok := plugins["io.containerd.grpc.v1.cri"]; ok {
		return unifiedLayout(), nil
	}

	return layout{}, ErrNoCRIPlugin
}

func splitLayout() layout {
	runtime := []string{"plugins", "io.containerd.cri.v1.runtime", "containerd"}
	images := []string{"plugins", "io.containerd.cri.v1.images"}

	return layout{
		runtimes:            path(runtime, "runtimes"),
		defaultRuntime:      path(runtime, "default_runtime_name"),
		snapshotter:         path(images, "snapshotter"),
		snapshotAnnotations: path(images, "disable_snapshot_annotations"),
		discardUnpacked:     path(images, "discard_unpacked_layers"),
	}
}

func unifiedLayout() layout {
	base := []string{"plugins", "io.containerd.grpc.v1.cri", "containerd"}
	at := func(key string) []string { return path(base, key) }

	return layout{
		runtimes:            at("runtimes"),
		defaultRuntime:      at("default_runtime_name"),
		snapshotter:         at("snapshotter"),
		snapshotAnnotations: at("disable_snapshot_annotations"),
		discardUnpacked:     at("discard_unpacked_layers"),
	}
}

// defaultRuntimeName is the handler the bootstrap handler is copied from.
//
// An unset default_runtime_name means containerd's own, which is runc. Falling
// back to the only handler present would be wrong: a node with one non-runc
// handler configured has not thereby made it the default.
func defaultRuntimeName(doc Document, l layout) string {
	if name, ok := lookup(doc, l.defaultRuntime).(string); ok && name != "" {
		return name
	}

	return "runc"
}

// lookup returns the value at a path, or nil.
func lookup(doc Document, path []string) any {
	var current any = map[string]any(doc)

	for _, key := range path {
		table, ok := current.(map[string]any)
		if !ok {
			return nil
		}

		current, ok = table[key]
		if !ok {
			return nil
		}
	}

	return current
}

// setPath writes a value, creating the tables above it, and reports whether the
// document changed.
func setPath(doc Document, path []string, value any) bool {
	table := map[string]any(doc)

	for _, key := range path[:len(path)-1] {
		next, ok := table[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			table[key] = next
		}

		table = next
	}

	last := path[len(path)-1]
	if existing, ok := table[last]; ok && equal(existing, value) {
		return false
	}

	table[last] = value

	return true
}

// deletePath removes a key and reports whether the document changed. Tables
// left empty are kept: they are the node's own, and an empty table is what its
// configuration already looked like before anything was written into it.
func deletePath(doc Document, path []string) bool {
	table := map[string]any(doc)

	for _, key := range path[:len(path)-1] {
		next, ok := table[key].(map[string]any)
		if !ok {
			return false
		}

		table = next
	}

	last := path[len(path)-1]
	if _, ok := table[last]; !ok {
		return false
	}

	delete(table, last)

	return true
}

// equal compares two decoded TOML values. It is not reflect.DeepEqual because
// an integer read back from a file is an int64 and one written by this package
// is an int, and a document that differs only in that has not changed.
func equal(a, b any) bool {
	switch left := a.(type) {
	case map[string]any:
		right, ok := b.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}

		keys := make([]string, 0, len(left))
		for key := range left {
			keys = append(keys, key)
		}

		sort.Strings(keys)

		for _, key := range keys {
			other, ok := right[key]
			if !ok || !equal(left[key], other) {
				return false
			}
		}

		return true

	case []any:
		right, ok := b.([]any)
		if !ok || len(left) != len(right) {
			return false
		}

		for i := range left {
			if !equal(left[i], right[i]) {
				return false
			}
		}

		return true

	case int64, int:
		want, _ := asInt64(a)

		got, ok := asInt64(b)

		return ok && want == got
	}

	return a == b
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	}

	return 0, false
}

// path joins a prefix and a key without ever writing into the prefix's backing
// array, which a bare append would be free to do.
func path(prefix []string, keys ...string) []string {
	out := make([]string, 0, len(prefix)+len(keys))
	out = append(out, prefix...)

	return append(out, keys...)
}

// deepCopy clones a decoded TOML table, so editing the copy cannot reach back
// into the document it came from.
func deepCopy(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))

	for key, value := range in {
		switch typed := value.(type) {
		case map[string]any:
			out[key] = deepCopy(typed)
		case []any:
			list := make([]any, len(typed))

			for i, item := range typed {
				if table, ok := item.(map[string]any); ok {
					list[i] = deepCopy(table)

					continue
				}

				list[i] = item
			}

			out[key] = list
		default:
			out[key] = value
		}
	}

	return out
}
