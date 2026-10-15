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

	// MaxSize bounds the encoded ledger for writes that add a grant.
	// Cleanup writes have extra headroom for release records.
	MaxSize = 900 * 1024

	// ReleaseRetention is how long a Release record is kept after it is written.
	// The evictor must act on it within this time.
	ReleaseRetention = 10 * time.Minute

	// HandoffCooldown is how long a node cannot be granted again after the
	// removal of its grant is confirmed (see Release.ConfirmedAt). The
	// evictor issues a delete authorized by an active grant only within
	// HandoffCooldown minus a margin of the ledger read that authorized it,
	// and cancels it at that point. A read that saw the grant happened
	// before the removal committed, and so before it was confirmed. So a
	// delete authorized by the old grant has finished before a new grant can
	// admit pods. See evictor.Reconcile for the margin and the clock
	// assumptions.
	HandoffCooldown = 10 * time.Second

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
// end time; DropAt is when the record can be removed, though Reap keeps it
// while it is unconfirmed and until its handoff cooldown ends.
//
// ConfirmedAt is a time at or after the removal was durably committed. The
// handoff cooldown counts from it, because a removal stamped at ReleasedAt
// can commit much later, and until then the evictor still sees the grant.
// A grant that expired is gone for the evictor from ExpiresAt whether or not
// it is still stored, so its release is confirmed at ReleasedAt when written.
// A live removal is written unconfirmed and confirmed by a later write that
// read it back. A zero ConfirmedAt fences the node until then.
type Release struct {
	UID          types.UID `json:"-"`
	Namespace    string    `json:"namespace"`
	NamespaceUID types.UID `json:"namespaceUID,omitempty"`
	Nodes        []string  `json:"nodes"`
	CreatedAt    time.Time `json:"createdAt"`
	ReleasedAt   time.Time `json:"releasedAt"`
	ConfirmedAt  time.Time `json:"confirmedAt,omitzero"`
	DropAt       time.Time `json:"dropAt"`
}

// ConflictError reports nodes that are already granted.
type ConflictError struct {
	Nodes []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("nodes already granted: %s", strings.Join(e.Nodes, ", "))
}

// CooldownError reports nodes whose previous grant's removal was confirmed
// less than HandoffCooldown ago, or is not confirmed yet. Unconfirmed is set
// in the latter case.
type CooldownError struct {
	Nodes       []string
	RetryAfter  time.Duration
	Unconfirmed bool
}

func (e *CooldownError) Error() string {
	return fmt.Sprintf("nodes were released less than %s ago, retry in %s: %s",
		HandoffCooldown, e.RetryAfter.Round(time.Second), strings.Join(e.Nodes, ", "))
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

// CheckCooldown returns *CooldownError if any of nodes has a release record
// that is unconfirmed or was confirmed less than HandoffCooldown before now,
// and nil otherwise. For an unconfirmed release, RetryAfter assumes it is
// confirmed at now.
//
// The cooldown counts from ConfirmedAt, which is the grant's expiry for a
// grant reaped after it expired. That is enough: the evictor treats a grant
// as gone from its expiry, whether or not it is still in the ledger.
func (l *Ledger) CheckCooldown(nodes []string, now time.Time) error {
	want := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		want[n] = true
	}

	var (
		fenced      []string
		until       time.Time
		unconfirmed bool
	)

	for _, r := range l.releases {
		end := r.ConfirmedAt.Add(HandoffCooldown)
		if r.ConfirmedAt.IsZero() {
			end = now.Add(HandoffCooldown)
		} else if !now.Before(end) {
			continue
		}

		hit := false

		for _, n := range r.Nodes {
			if want[n] {
				fenced = append(fenced, n)
				hit = true
			}
		}

		if hit && end.After(until) {
			until = end
		}

		if hit && r.ConfirmedAt.IsZero() {
			unconfirmed = true
		}
	}

	if len(fenced) == 0 {
		return nil
	}

	slices.Sort(fenced)

	return &CooldownError{Nodes: slices.Compact(fenced), RetryAfter: until.Sub(now), Unconfirmed: unconfirmed}
}

// Confirm sets ConfirmedAt to now on every unconfirmed release. Callers must
// have read the ledger from a committed ConfigMap, which shows the removals
// are durable, and must read now after that read. It reports whether any
// release changed.
func (l *Ledger) Confirm(now time.Time) bool {
	changed := false

	for _, r := range l.releases {
		if r.ConfirmedAt.IsZero() {
			r.ConfirmedAt = now
			changed = true
		}
	}

	return changed
}

// Unconfirmed reports whether any release is unconfirmed.
func (l *Ledger) Unconfirmed() bool {
	for _, r := range l.releases {
		if r.ConfirmedAt.IsZero() {
			return true
		}
	}

	return false
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
// The release of an expired grant is confirmed at ExpiresAt; any other release
// is left unconfirmed until Confirm runs after the removal commits. It
// reports whether the grant existed.
func (l *Ledger) Remove(key types.NamespacedName, now time.Time) bool {
	g, ok := l.grants[key]
	if ok && g.Expired(now) {
		return l.remove(key, g.ExpiresAt, g.ExpiresAt, now)
	}

	return l.remove(key, now, time.Time{}, now)
}

func (l *Ledger) remove(key types.NamespacedName, releasedAt, confirmedAt, now time.Time) bool {
	g, ok := l.grants[key]
	if !ok {
		return false
	}

	delete(l.grants, key)

	if g.UID != "" {
		l.releases[g.UID] = &Release{
			UID: g.UID, Namespace: g.Namespace, NamespaceUID: g.NamespaceUID, Nodes: g.Nodes,
			CreatedAt: g.CreatedAt, ReleasedAt: releasedAt, ConfirmedAt: confirmedAt, DropAt: now.Add(ReleaseRetention),
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
// false, and drops confirmed releases at DropAt, or at the end of their
// handoff cooldown if that is later. Unconfirmed releases are kept. Expired grants end at ExpiresAt and
// their releases are confirmed then; other removals end at now and are left
// unconfirmed. Release retention starts at now. It returns the
// removed grants and the next time Reap has work to do (zero if never).
func (l *Ledger) Reap(now time.Time, keep func(*Grant) bool) (removed []*Grant, next time.Time) {
	for _, g := range l.Grants() {
		if g.Expired(now) || (keep != nil && !keep(g)) {
			if g.Expired(now) {
				l.remove(g.Key(), g.ExpiresAt, g.ExpiresAt, now)
			} else {
				l.remove(g.Key(), now, time.Time{}, now)
			}

			removed = append(removed, g)

			continue
		}

		if next.IsZero() || g.ExpiresAt.Before(next) {
			next = g.ExpiresAt
		}
	}

	for uid, r := range l.releases {
		if r.ConfirmedAt.IsZero() {
			// Keep fencing the nodes until the removal is confirmed.
			continue
		}

		dropAt := r.DropAt
		if end := r.ConfirmedAt.Add(HandoffCooldown); end.After(dropAt) {
			// The record fences its nodes until the cooldown ends.
			dropAt = end
		}

		if !now.Before(dropAt) {
			delete(l.releases, uid)
		} else if next.IsZero() || dropAt.Before(next) {
			next = dropAt
		}
	}

	return removed, next
}
