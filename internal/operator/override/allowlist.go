// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"sort"
	"strings"
)

// ReservedPrefix marks labels and annotations the operator owns. They carry
// config hashes, Site scoping and override visibility, so a patch that wrote
// them could hide the fact that an override is in effect.
const ReservedPrefix = "unbounded-cloud.io/"

// wildcard matches any single path element: a list index, or an arbitrary
// user-chosen map key such as a label name.
const wildcard = "*"

// permittedPaths enumerates what a patch may change.
//
// A path ending in a leaf permits exactly that field. A path marked as a
// subtree permits every descendant, including fields added by future Kubernetes
// releases. Subtrees are deliberate rather than an oversight: per the package
// documentation the allowlist is an integrity control, and a new field under
// securityContext is not a new capability for a principal who can already
// replace the container image. Enumerating leaves would mean revisiting this
// list on every Kubernetes minor release for no security benefit.
//
// The allowlist is therefore fail-closed at the path level and fail-open within
// a permitted subtree.
var permittedPaths = []struct {
	path    string
	subtree bool
}{
	// Workload metadata. The reserved prefix is rejected separately.
	{path: "metadata.labels", subtree: true},
	{path: "metadata.annotations", subtree: true},

	// Workload spec.
	{path: "spec.replicas"},
	{path: "spec.strategy", subtree: true},
	{path: "spec.updateStrategy", subtree: true},
	{path: "spec.minReadySeconds"},
	{path: "spec.revisionHistoryLimit"},

	// Pod template metadata.
	{path: "spec.template.metadata.labels", subtree: true},
	{path: "spec.template.metadata.annotations", subtree: true},

	// Containers. name is permitted because it is the strategic merge key:
	// without it the patch cannot identify which container it targets.
	{path: "spec.template.spec.containers.*.name"},
	{path: "spec.template.spec.containers.*.image"},
	{path: "spec.template.spec.containers.*.imagePullPolicy"},
	{path: "spec.template.spec.containers.*.args", subtree: true},
	{path: "spec.template.spec.containers.*.command", subtree: true},
	{path: "spec.template.spec.containers.*.env", subtree: true},
	{path: "spec.template.spec.containers.*.envFrom", subtree: true},
	{path: "spec.template.spec.containers.*.resources", subtree: true},
	{path: "spec.template.spec.containers.*.volumeMounts", subtree: true},
	{path: "spec.template.spec.containers.*.securityContext", subtree: true},
	{path: "spec.template.spec.containers.*.livenessProbe", subtree: true},
	{path: "spec.template.spec.containers.*.readinessProbe", subtree: true},
	{path: "spec.template.spec.containers.*.startupProbe", subtree: true},
	{path: "spec.template.spec.containers.*.lifecycle", subtree: true},
	{path: "spec.template.spec.containers.*.ports", subtree: true},
	{path: "spec.template.spec.containers.*.terminationMessagePath"},
	{path: "spec.template.spec.containers.*.terminationMessagePolicy"},
	{path: "spec.template.spec.containers.*.workingDir"},

	// Init containers accept the same surface.
	{path: "spec.template.spec.initContainers.*.name"},
	{path: "spec.template.spec.initContainers.*.image"},
	{path: "spec.template.spec.initContainers.*.imagePullPolicy"},
	{path: "spec.template.spec.initContainers.*.args", subtree: true},
	{path: "spec.template.spec.initContainers.*.command", subtree: true},
	{path: "spec.template.spec.initContainers.*.env", subtree: true},
	{path: "spec.template.spec.initContainers.*.envFrom", subtree: true},
	{path: "spec.template.spec.initContainers.*.resources", subtree: true},
	{path: "spec.template.spec.initContainers.*.volumeMounts", subtree: true},
	{path: "spec.template.spec.initContainers.*.securityContext", subtree: true},
	{path: "spec.template.spec.initContainers.*.restartPolicy"},

	// Pod spec.
	{path: "spec.template.spec.volumes", subtree: true},
	{path: "spec.template.spec.imagePullSecrets", subtree: true},
	{path: "spec.template.spec.nodeSelector", subtree: true},
	{path: "spec.template.spec.tolerations", subtree: true},
	{path: "spec.template.spec.affinity", subtree: true},
	{path: "spec.template.spec.topologySpreadConstraints", subtree: true},
	{path: "spec.template.spec.priorityClassName"},
	{path: "spec.template.spec.dnsPolicy"},
	{path: "spec.template.spec.dnsConfig", subtree: true},
	{path: "spec.template.spec.terminationGracePeriodSeconds"},
	{path: "spec.template.spec.runtimeClassName"},
	{path: "spec.template.spec.schedulerName"},
	{path: "spec.template.spec.securityContext", subtree: true},
}

// protectedPaths may never appear in a patch.
//
// Every one of these is also re-stamped after the merge, so correctness does
// not depend on this list being exhaustive. Rejecting them here turns a silent
// no-op into a clear error.
var protectedPaths = []struct {
	path   string
	reason string
}{
	{
		path:   "apiVersion",
		reason: "the object's group and version decide what resource is written, and the operator holds escalate and bind on ClusterRoleBindings",
	},
	{
		path:   "kind",
		reason: "the object's kind decides what resource is written, and the operator holds escalate and bind on ClusterRoleBindings",
	},
	{path: "metadata.name", reason: "renaming a workload orphans the original, and the operator does not prune"},
	{path: "metadata.namespace", reason: "renaming a workload orphans the original, and the operator does not prune"},
	{path: "metadata.ownerReferences", reason: "owner references drive per-Site garbage collection"},
	{path: "metadata.finalizers", reason: "finalizers drive deletion ordering the operator relies on"},
	{path: "metadata.resourceVersion", reason: "the operator applies rather than updates, so a resourceVersion here is always wrong"},
	{path: "spec.selector", reason: "a workload whose template labels stop satisfying its selector is rejected by the API server"},
	{
		path:   "spec.template.spec.serviceAccountName",
		reason: "retargeting the ServiceAccount borrows another identity's API permissions and detaches the component from its RBAC",
	},
	{path: "spec.template.spec.hostNetwork", reason: "host namespace membership is a deliberate per-component decision"},
	{path: "spec.template.spec.hostPID", reason: "host namespace membership is a deliberate per-component decision"},
	{path: "spec.template.spec.hostIPC", reason: "host namespace membership is a deliberate per-component decision"},
	{path: "status", reason: "status is owned by the workload controller, not by the operator"},
}

// mergeKeyTypes names the field each merge-keyed list is identified by, and the
// type strategic merge requires it to hold.
//
// This is not cosmetic. strategicpatch compares merge keys with Go's ==
// operator (patch.go:955, and findMapInSliceBasedOnKeyValue), which panics at
// runtime on an uncomparable type. A patch containing
// `env: [{name: [oops], value: x}]` would therefore crash-loop the operator
// rather than be rejected, so the type is checked before the merge ever runs.
var mergeKeyTypes = map[string]struct {
	field string
	kind  string
}{
	"spec.template.spec.containers":                    {field: "name", kind: "string"},
	"spec.template.spec.initContainers":                {field: "name", kind: "string"},
	"spec.template.spec.volumes":                       {field: "name", kind: "string"},
	"spec.template.spec.imagePullSecrets":              {field: "name", kind: "string"},
	"spec.template.spec.containers.*.env":              {field: "name", kind: "string"},
	"spec.template.spec.initContainers.*.env":          {field: "name", kind: "string"},
	"spec.template.spec.containers.*.volumeMounts":     {field: "mountPath", kind: "string"},
	"spec.template.spec.initContainers.*.volumeMounts": {field: "mountPath", kind: "string"},
	"spec.template.spec.containers.*.ports":            {field: "containerPort", kind: "number"},
	"spec.template.spec.initContainers.*.ports":        {field: "containerPort", kind: "number"},
	"spec.template.spec.tolerations":                   {field: "", kind: ""},
}

// stringValuedPaths are maps whose values Kubernetes requires to be strings.
//
// A non-string here is not merely invalid: unstructured's GetAnnotations and
// GetLabels return nil for a map containing one, so the operator would silently
// replace every annotation on the object with its own bookkeeping rather than
// merging into them.
var stringValuedPaths = map[string]bool{
	"metadata.labels":                    true,
	"metadata.annotations":               true,
	"spec.template.metadata.labels":      true,
	"spec.template.metadata.annotations": true,
	"spec.template.spec.nodeSelector":    true,
}

// pathNode is one node in the allowlist trie.
type pathNode struct {
	children map[string]*pathNode

	// permitted marks a node a patch may set directly.
	permitted bool

	// subtree marks a node whose descendants are all permitted.
	subtree bool
}

func (n *pathNode) child(name string) *pathNode {
	if n.children == nil {
		n.children = map[string]*pathNode{}
	}

	if existing, ok := n.children[name]; ok {
		return existing
	}

	created := &pathNode{}
	n.children[name] = created

	return created
}

// lookup returns the child matching name, preferring an exact match over the
// wildcard so a named field wins over a list-element rule.
func (n *pathNode) lookup(name string) *pathNode {
	if exact, ok := n.children[name]; ok {
		return exact
	}

	return n.children[wildcard]
}

// allowlist is the compiled permitted-path trie.
type allowlist struct {
	root *pathNode
}

// newAllowlist compiles permittedPaths into a trie once.
func newAllowlist() *allowlist {
	root := &pathNode{}

	for _, entry := range permittedPaths {
		node := root
		for _, element := range strings.Split(entry.path, ".") {
			node = node.child(element)
		}

		node.permitted = true
		node.subtree = entry.subtree
	}

	return &allowlist{root: root}
}

// compiledAllowlist is built once; the trie is read-only after construction.
var compiledAllowlist = newAllowlist()

// PermittedPaths returns the permitted paths in sorted order, marking subtrees.
// It exists so the CLI and documentation can render the surface rather than
// restating it and drifting.
func PermittedPaths() []string {
	out := make([]string, 0, len(permittedPaths))

	for _, entry := range permittedPaths {
		if entry.subtree {
			out = append(out, entry.path+".*")

			continue
		}

		out = append(out, entry.path)
	}

	sort.Strings(out)

	return out
}

// ProtectedPaths returns the protected paths in sorted order.
func ProtectedPaths() []string {
	out := make([]string, 0, len(protectedPaths))

	for _, entry := range protectedPaths {
		out = append(out, entry.path)
	}

	sort.Strings(out)

	return out
}

// protectedReason reports whether path is protected, and why.
func protectedReason(path string) (string, bool) {
	for _, entry := range protectedPaths {
		if path == entry.path || strings.HasPrefix(path, entry.path+".") {
			return entry.reason, true
		}
	}

	return "", false
}

// joinPath appends an element to a dotted path.
func joinPath(base, element string) string {
	if base == "" {
		return element
	}

	return base + "." + element
}

// describePath renders a path for an error message, using the object root when
// the path is empty.
func describePath(path string) string {
	if path == "" {
		return "(root)"
	}

	return path
}
