// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package ledger stores NodeGrants in a single ConfigMap.
//
// The ConfigMap holds two kinds of keys:
//
//   - "<node>" maps a granted node to "<namespace>/<grant>". The pod placement
//     ValidatingAdmissionPolicy reads these keys through its paramRef.
//   - "_grant.<namespace>.<grant>" holds the JSON Grant record.
//
// Grant records are authoritative. Node keys are always derived from them
// when the ledger is encoded, so a hand-edited or partially written node key
// is repaired on the next write.
package ledger

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

const (
	// ConfigMapName is the name of the ledger ConfigMap.
	ConfigMapName = "rebound-ledger"

	// MaxSize bounds the encoded ledger. ConfigMaps are limited to 1 MiB;
	// leave headroom for object metadata.
	MaxSize = 900 * 1024

	grantKeyPrefix = "_grant."
)

// Grant is a stored NodeGrant.
type Grant struct {
	Namespace   string            `json:"-"`
	Name        string            `json:"-"`
	UID         types.UID         `json:"uid"`
	Nodes       []string          `json:"nodes"`
	Holder      string            `json:"holder"`
	CreatedAt   time.Time         `json:"createdAt"`
	ExpiresAt   time.Time         `json:"expiresAt"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Key returns the namespace/name of the grant.
func (g *Grant) Key() types.NamespacedName {
	return types.NamespacedName{Namespace: g.Namespace, Name: g.Name}
}

// Expired reports whether the grant has expired at now.
func (g *Grant) Expired(now time.Time) bool {
	return !now.Before(g.ExpiresAt)
}

// ConflictError reports nodes that are already granted.
type ConflictError struct {
	Nodes []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("nodes already granted: %s", strings.Join(e.Nodes, ", "))
}

// ExistsError reports that a grant with the same name already exists.
type ExistsError struct {
	Key types.NamespacedName
}

func (e *ExistsError) Error() string {
	return fmt.Sprintf("grant %s already exists", e.Key)
}

// Ledger is the decoded content of the ledger ConfigMap.
type Ledger struct {
	grants map[types.NamespacedName]*Grant
	nodes  map[string]types.NamespacedName
}

// New returns an empty ledger.
func New() *Ledger {
	return &Ledger{
		grants: map[types.NamespacedName]*Grant{},
		nodes:  map[string]types.NamespacedName{},
	}
}

// Parse decodes ConfigMap data. It never fails: malformed grant records and
// grants that claim a node already claimed by an earlier grant (in key order)
// are dropped, and node keys are ignored. Comparing Encode() with the input
// tells the caller whether a repair write is needed.
func Parse(data map[string]string) *Ledger {
	l := New()

	keys := make([]string, 0, len(data))
	for k := range data {
		if strings.HasPrefix(k, grantKeyPrefix) {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	for _, k := range keys {
		ns, name, ok := strings.Cut(strings.TrimPrefix(k, grantKeyPrefix), ".")
		if !ok || ns == "" || name == "" || strings.Contains(name, ".") {
			continue
		}

		g := &Grant{}
		if err := json.Unmarshal([]byte(data[k]), g); err != nil || len(g.Nodes) == 0 {
			continue
		}

		g.Namespace, g.Name = ns, name
		if err := l.Add(g); err != nil {
			continue
		}
	}

	return l
}

// Encode returns the ConfigMap data for the ledger.
func (l *Ledger) Encode() map[string]string {
	data := make(map[string]string, len(l.grants)+len(l.nodes))

	for key, g := range l.grants {
		b, err := json.Marshal(g)
		if err != nil {
			// Grant only holds marshalable fields.
			panic(err)
		}

		data[GrantKey(key)] = string(b)
	}

	for node, key := range l.nodes {
		data[node] = key.String()
	}

	return data
}

// GrantKey returns the ConfigMap key for a grant record.
func GrantKey(key types.NamespacedName) string {
	return grantKeyPrefix + key.Namespace + "." + key.Name
}

// Size returns the number of bytes the data contributes to a ConfigMap.
func Size(data map[string]string) int {
	n := 0
	for k, v := range data {
		n += len(k) + len(v)
	}

	return n
}

// Get returns the grant with the given key.
func (l *Ledger) Get(key types.NamespacedName) (*Grant, bool) {
	g, ok := l.grants[key]

	return g, ok
}

// NodeGrant returns the grant holding the node.
func (l *Ledger) NodeGrant(node string) (types.NamespacedName, bool) {
	key, ok := l.nodes[node]

	return key, ok
}

// Grants returns all grants sorted by namespace and name.
func (l *Ledger) Grants() []*Grant {
	out := make([]*Grant, 0, len(l.grants))
	for _, g := range l.grants {
		out = append(out, g)
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}

		return a.Name < b.Name
	})

	return out
}

// Add adds a grant atomically. It returns *ExistsError if the name is taken
// and *ConflictError if any node is already granted; in both cases the ledger
// is unchanged.
func (l *Ledger) Add(g *Grant) error {
	key := g.Key()
	if _, ok := l.grants[key]; ok {
		return &ExistsError{Key: key}
	}

	var taken []string

	for _, n := range g.Nodes {
		if _, ok := l.nodes[n]; ok {
			taken = append(taken, n)
		}
	}

	if len(taken) > 0 {
		slices.Sort(taken)

		return &ConflictError{Nodes: slices.Compact(taken)}
	}

	l.grants[key] = g
	for _, n := range g.Nodes {
		l.nodes[n] = key
	}

	return nil
}

// Remove deletes a grant. It reports whether the grant existed.
func (l *Ledger) Remove(key types.NamespacedName) bool {
	g, ok := l.grants[key]
	if !ok {
		return false
	}

	delete(l.grants, key)

	for _, n := range g.Nodes {
		if l.nodes[n] == key {
			delete(l.nodes, n)
		}
	}

	return true
}

// Reap removes grants that are expired at now or for which keep returns
// false. It returns the removed grants and the earliest expiry among the
// grants that remain (zero if none remain).
func (l *Ledger) Reap(now time.Time, keep func(*Grant) bool) (removed []*Grant, next time.Time) {
	for _, g := range l.Grants() {
		if g.Expired(now) || (keep != nil && !keep(g)) {
			l.Remove(g.Key())
			removed = append(removed, g)

			continue
		}

		if next.IsZero() || g.ExpiresAt.Before(next) {
			next = g.ExpiresAt
		}
	}

	return removed, next
}
