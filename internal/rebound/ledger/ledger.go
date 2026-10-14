// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package ledger stores NodeGrants in a single ConfigMap.
//
// The ConfigMap holds three kinds of keys:
//
//   - "<node>" maps a granted node to "<namespace>/<grant>". The pod placement
//     ValidatingAdmissionPolicy reads these keys through its paramRef.
//   - "_grant.<namespace>.<grant>" holds the JSON Grant record.
//   - "_release.<uid>" holds the JSON Release record of a removed grant.
//
// Grant records are authoritative. Node keys are always derived from them
// when the ledger is encoded, so a hand-edited or partially written node key
// is repaired on the next write.
// Releases separate the grant's end time from the record's retention deadline,
// so a late reap does not extend the eviction window or skip pod cleanup.
package ledger

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// ConfigMapName is the name of the ledger ConfigMap.
	ConfigMapName = "rebound-ledger"

	// MaxSize bounds the encoded ledger for writes that increase the grant count.
	// Cleanup writes have extra headroom for release records.
	MaxSize = 900 * 1024

	// ReleaseRetention is how long a Release record is kept after it is written.
	// The evictor must act on it within this time.
	ReleaseRetention = 10 * time.Minute

	grantKeyPrefix   = "_grant."
	releaseKeyPrefix = "_release."
)

// Grant is a stored NodeGrant.
type Grant struct {
	Namespace    string                `json:"-"`
	NamespaceUID types.UID             `json:"namespaceUID,omitempty"`
	Name         string                `json:"-"`
	UID          types.UID             `json:"uid"`
	Nodes        []string              `json:"nodes"`
	Selector     *metav1.LabelSelector `json:"selector,omitempty"`
	Holder       string                `json:"holder"`
	CreatedAt    time.Time             `json:"createdAt"`
	ExpiresAt    time.Time             `json:"expiresAt"`
	Labels       map[string]string     `json:"labels,omitempty"`
	Annotations  map[string]string     `json:"annotations,omitempty"`
}

// Key returns the namespace/name of the grant.
func (g *Grant) Key() types.NamespacedName {
	return types.NamespacedName{Namespace: g.Namespace, Name: g.Name}
}

// Expired reports whether the grant has expired at now.
func (g *Grant) Expired(now time.Time) bool {
	return !now.Before(g.ExpiresAt)
}

// Release records a removed grant. Pods in Namespace on Nodes that were
// created while the grant was active are evicted. NamespaceUID is the grant's
// namespace UID, so pods in a namespace recreated with the same name are not
// evicted; it is empty for older records and grants. ReleasedAt is the grant's
// end time; DropAt is when the record can be removed.
type Release struct {
	UID          types.UID `json:"-"`
	Namespace    string    `json:"namespace"`
	NamespaceUID types.UID `json:"namespaceUID,omitempty"`
	Nodes        []string  `json:"nodes"`
	CreatedAt    time.Time `json:"createdAt"`
	ReleasedAt   time.Time `json:"releasedAt"`
	DropAt       time.Time `json:"dropAt"`
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
	grants   map[types.NamespacedName]*Grant
	nodes    map[string]types.NamespacedName
	releases map[types.UID]*Release
}

// New returns an empty ledger.
func New() *Ledger {
	return &Ledger{
		grants:   map[types.NamespacedName]*Grant{},
		nodes:    map[string]types.NamespacedName{},
		releases: map[types.UID]*Release{},
	}
}

// Parse decodes ConfigMap data. It never fails: malformed records, records
// whose UID does not form a valid release key, and grants that claim a node
// already claimed by an earlier grant (in key order) are dropped, and node
// keys are ignored. Comparing Encode() with the input tells the caller whether
// a repair write is needed.
func Parse(data map[string]string) *Ledger {
	l := New()

	keys := make([]string, 0, len(data))
	for k, v := range data {
		if strings.HasPrefix(k, grantKeyPrefix) {
			keys = append(keys, k)
		} else if uid, ok := strings.CutPrefix(k, releaseKeyPrefix); ok && validUID(types.UID(uid)) {
			r := &Release{}
			if err := json.Unmarshal([]byte(v), r); err == nil && validNames(r.Namespace, r.Nodes) {
				r.UID = types.UID(uid)
				if r.DropAt.IsZero() {
					// Older records counted retention from the release time.
					r.DropAt = r.ReleasedAt.Add(ReleaseRetention)
				}

				l.releases[r.UID] = r
			}
		}
	}

	sort.Strings(keys)

	for _, k := range keys {
		ns, name, ok := strings.Cut(strings.TrimPrefix(k, grantKeyPrefix), ".")
		if !ok || len(validation.IsDNS1123Label(name)) != 0 {
			continue
		}

		g := &Grant{}
		// The selector is informational. Bad selector data must not release nodes.
		record := struct {
			*Grant
			Selector json.RawMessage `json:"selector"`
		}{Grant: g}
		if err := json.Unmarshal([]byte(data[k]), &record); err != nil || !validNames(ns, g.Nodes) {
			continue
		}

		// An empty UID is a legacy grant that records no release on removal.
		if g.UID != "" && !validUID(g.UID) {
			continue
		}

		if err := json.Unmarshal(record.Selector, &g.Selector); err != nil {
			g.Selector = nil
		}

		g.Namespace, g.Name = ns, name
		if err := l.Add(g); err != nil {
			continue
		}
	}

	return l
}

// validNames reports whether a decoded record has a valid namespace and at
// least one valid node name. Node names become ConfigMap keys on Encode, so an
// invalid one would make every later ledger write fail.
func validNames(namespace string, nodes []string) bool {
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(nodes) == 0 {
		return false
	}

	for _, n := range nodes {
		if len(validation.IsDNS1123Subdomain(n)) != 0 {
			return false
		}
	}

	return true
}

// validUID reports whether uid is non-empty and its release key is a valid
// ConfigMap key. Removing a grant writes that key, so an invalid UID would
// make the cleanup write fail and leave the grant in place.
func validUID(uid types.UID) bool {
	return uid != "" && len(validation.IsConfigMapKey(releaseKey(uid))) == 0
}

// releaseKey returns the ConfigMap key for a release record.
func releaseKey(uid types.UID) string {
	return releaseKeyPrefix + string(uid)
}

// Encode returns the ConfigMap data for the ledger.
func (l *Ledger) Encode() map[string]string {
	data := make(map[string]string, len(l.grants)+len(l.nodes)+len(l.releases))

	for uid, r := range l.releases {
		b, err := json.Marshal(r)
		if err != nil {
			// Release only holds marshalable fields.
			panic(err)
		}

		data[releaseKey(uid)] = string(b)
	}

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

// Releases returns all release records sorted by UID.
func (l *Ledger) Releases() []*Release {
	out := make([]*Release, 0, len(l.releases))
	for _, r := range l.releases {
		out = append(out, r)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })

	return out
}

// Remove deletes a grant and records a Release for it. The grant ends at now,
// or at ExpiresAt if it has already expired, so pods created after expiry are
// not treated as created during the grant. Release retention starts at now.
// It reports whether the grant existed.
func (l *Ledger) Remove(key types.NamespacedName, now time.Time) bool {
	releasedAt := now
	if g, ok := l.grants[key]; ok && g.Expired(now) {
		releasedAt = g.ExpiresAt
	}

	return l.remove(key, releasedAt, now)
}

func (l *Ledger) remove(key types.NamespacedName, releasedAt, now time.Time) bool {
	g, ok := l.grants[key]
	if !ok {
		return false
	}

	delete(l.grants, key)

	if g.UID != "" {
		l.releases[g.UID] = &Release{
			UID: g.UID, Namespace: g.Namespace, NamespaceUID: g.NamespaceUID, Nodes: g.Nodes,
			CreatedAt: g.CreatedAt, ReleasedAt: releasedAt, DropAt: now.Add(ReleaseRetention),
		}
	}

	for _, n := range g.Nodes {
		if l.nodes[n] == key {
			delete(l.nodes, n)
		}
	}

	return true
}

// Reap removes grants that are expired at now or for which keep returns
// false, and drops releases at DropAt. Expired grants end at ExpiresAt;
// other removals end at now. Release retention starts at now. It returns the
// removed grants and the next time Reap has work to do (zero if never).
func (l *Ledger) Reap(now time.Time, keep func(*Grant) bool) (removed []*Grant, next time.Time) {
	for _, g := range l.Grants() {
		if g.Expired(now) || (keep != nil && !keep(g)) {
			releasedAt := now
			if g.Expired(now) {
				releasedAt = g.ExpiresAt
			}

			l.remove(g.Key(), releasedAt, now)
			removed = append(removed, g)

			continue
		}

		if next.IsZero() || g.ExpiresAt.Before(next) {
			next = g.ExpiresAt
		}
	}

	for uid, r := range l.releases {
		if !now.Before(r.DropAt) {
			delete(l.releases, uid)
		} else if next.IsZero() || r.DropAt.Before(next) {
			next = r.DropAt
		}
	}

	return removed, next
}
