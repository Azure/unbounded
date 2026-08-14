// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

// membershipCohortKey is the query key carrying a member's cohort.
const membershipCohortKey = "cohort"

// Member is one node of a zone's agreed catalog membership, with the cohort it
// occupies in every trio it appears in. A node's cohort is fixed for its life,
// because its position in a trio is normative: the three members of a group are
// not interchangeable.
type Member struct {
	NodeID uint32
	Cohort uint32
}

// Membership is a zone's agreed catalog membership within one universe. It is
// the only thing the operator has to agree; the trios follow from it by a pure
// function, so every node derives the same catalog without coordinating.
type Membership []Member

// ByCohort splits a membership into its three cohorts, each sorted by node id.
func (m Membership) ByCohort() [Cohorts][]uint32 {
	var cohorts [Cohorts][]uint32

	for _, member := range m {
		if member.Cohort >= Cohorts {
			continue
		}

		cohorts[member.Cohort] = append(cohorts[member.Cohort], member.NodeID)
	}

	for i := range cohorts {
		sort.Slice(cohorts[i], func(a, b int) bool { return cohorts[i][a] < cohorts[i][b] })
	}

	return cohorts
}

// Contains reports whether a node is a member.
func (m Membership) Contains(nodeID uint32) bool {
	for _, member := range m {
		if member.NodeID == nodeID {
			return true
		}
	}

	return false
}

// NodeIDs returns the member ids, sorted.
func (m Membership) NodeIDs() []uint32 {
	ids := make([]uint32, 0, len(m))
	for _, member := range m {
		ids = append(ids, member.NodeID)
	}

	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })

	return ids
}

// Normalized returns the membership sorted by cohort then node id, which is the
// form it is written to an annotation in. Sorting means an unchanged membership
// always renders to the same string and never provokes a write.
func (m Membership) Normalized() Membership {
	out := make(Membership, len(m))
	copy(out, m)

	sort.Slice(out, func(a, b int) bool {
		if out[a].Cohort != out[b].Cohort {
			return out[a].Cohort < out[b].Cohort
		}

		return out[a].NodeID < out[b].NodeID
	})

	return out
}

// PerCohort is how many nodes each cohort holds, or an error if the cohorts are
// not equal. They have to be: a node appears in `len(catalog) / perCohort`
// groups, so unequal cohorts give unequal load and the dataplane refuses the
// catalog outright.
func (m Membership) PerCohort() (int, error) {
	cohorts := m.ByCohort()

	size := len(cohorts[0])
	for i := 1; i < Cohorts; i++ {
		if len(cohorts[i]) != size {
			return 0, fmt.Errorf(
				"cohorts must be equal sized, got %d/%d/%d",
				len(cohorts[0]), len(cohorts[1]), len(cohorts[2]),
			)
		}
	}

	return size, nil
}

// FormatMembership renders a membership to its annotation value.
func FormatMembership(m Membership) string {
	normalized := m.Normalized()
	entries := make([]ListEntry, 0, len(normalized))

	for _, member := range normalized {
		entries = append(entries, ListEntry{
			Item:   fmt.Sprintf("%d", member.NodeID),
			Values: map[string][]string{membershipCohortKey: {strconv.FormatUint(uint64(member.Cohort), 10)}},
		})
	}

	return FormatList(entries)
}

// ParseMembership reads a membership from its annotation value.
func ParseMembership(raw string) (Membership, error) {
	entries, err := ParseList(raw)
	if err != nil {
		return nil, err
	}

	out := make(Membership, 0, len(entries))
	seen := make(map[uint32]bool, len(entries))

	for _, entry := range entries {
		nodeID, err := ParseUint32(entry.Item)
		if err != nil {
			return nil, fmt.Errorf("membership node id: %w", err)
		}

		if nodeID == 0 {
			return nil, fmt.Errorf("membership names node id zero")
		}

		if seen[nodeID] {
			return nil, fmt.Errorf("membership names node %d twice", nodeID)
		}

		seen[nodeID] = true

		cohort, err := uint32Value(entry.Values, membershipCohortKey)
		if err != nil {
			return nil, fmt.Errorf("membership node %d: %w", nodeID, err)
		}

		if cohort >= Cohorts {
			return nil, fmt.Errorf("membership node %d has cohort %d, want 0, 1 or 2", nodeID, cohort)
		}

		out = append(out, Member{NodeID: nodeID, Cohort: cohort})
	}

	return out, nil
}

// BuildCatalog derives a zone's consensus groups from its membership.
//
// Group i takes cohort 0's node at index `i % perCohort`, and cohorts 1 and 2
// at indices rotated by how many times the cohort has already been walked. The
// rotation is what stops the catalog collapsing into `perCohort` distinct
// replica sets repeated over and over: rotating spreads the pairings out, so
// losing any three nodes costs a bounded share of the groups rather than all of
// the ones a single trio owned.
//
// Every node lands in exactly `len(catalog) / perCohort` groups, which is the
// balance the dataplane insists on.
func BuildCatalog(members Membership, catalogSize int) ([]*racerconfig.Trio, error) {
	if catalogSize <= 0 {
		return nil, fmt.Errorf("catalog size must be positive, got %d", catalogSize)
	}

	perCohort, err := members.PerCohort()
	if err != nil {
		return nil, err
	}

	if perCohort == 0 {
		return nil, fmt.Errorf("membership is empty")
	}

	if catalogSize%perCohort != 0 {
		return nil, fmt.Errorf(
			"cohort size %d does not divide catalog size %d, so the catalog cannot be balanced",
			perCohort, catalogSize,
		)
	}

	cohorts := members.ByCohort()
	catalog := make([]*racerconfig.Trio, 0, catalogSize)

	for i := range catalogSize {
		block := i / perCohort
		index := i % perCohort

		catalog = append(catalog, &racerconfig.Trio{
			Cohort_0: cohorts[0][index],
			Cohort_1: cohorts[1][(index+block)%perCohort],
			Cohort_2: cohorts[2][(index+2*block)%perCohort],
		})
	}

	return catalog, nil
}

// DesiredMembership picks the largest balanced membership the given nodes can
// form. Nodes past that are still real members of the zone and still route; they
// simply hold no groups until enough of their peers arrive to grow the catalog
// by a whole step.
//
// Growth is coarse on purpose. Cohorts must stay equal and their size must
// divide the catalog length, so the admissible sizes are the divisors of the
// catalog length, and the catalog length is pinned for the life of the zone.
func DesiredMembership(candidates Membership, catalogSize int) Membership {
	cohorts := candidates.Normalized().ByCohort()

	smallest := len(cohorts[0])
	for i := 1; i < Cohorts; i++ {
		if len(cohorts[i]) < smallest {
			smallest = len(cohorts[i])
		}
	}

	perCohort := largestDivisorAtMost(catalogSize, smallest)
	if perCohort == 0 {
		return nil
	}

	out := make(Membership, 0, perCohort*Cohorts)

	for cohort := range Cohorts {
		for _, nodeID := range cohorts[cohort][:perCohort] {
			out = append(out, Member{NodeID: nodeID, Cohort: uint32(cohort)})
		}
	}

	return out.Normalized()
}

// largestDivisorAtMost is the largest divisor of n that does not exceed limit.
func largestDivisorAtMost(n, limit int) int {
	if limit <= 0 || n <= 0 {
		return 0
	}

	if limit > n {
		limit = n
	}

	for candidate := limit; candidate >= 1; candidate-- {
		if n%candidate == 0 {
			return candidate
		}
	}

	return 0
}

// MembershipStep is one move toward the membership a zone's nodes describe.
//
// A step is always exactly one generation. The rule a step has to keep is per
// group, not per catalog: every group keeps at least two of the three nodes it
// named, so the two that stayed can serve reads and replay the one that
// arrived. Moving a whole catalog at once, however few ids it changed, is what
// leaves a group with nothing to replay from.
type MembershipStep struct {
	// Next is the membership to publish. It is exactly the set of nodes Catalog
	// names, with each node's cohort being the column it occupies.
	Next Membership

	// Catalog is the group assignment to publish. It is published rather than
	// derived because deriving it from the member list means every membership
	// change reshuffles every group, and a reshuffle is exactly what the
	// per-group rule forbids.
	Catalog Catalog

	// Done reports that nothing is left to move.
	Done bool

	// Draining are the nodes the catalog no longer names but which have not yet
	// handed over what they held. They keep deriving the universe, with
	// themselves absent from its catalog, because that configuration is what
	// makes racer shed.
	Draining Membership

	// Reason says what the step is doing, for the operator's wait message.
	Reason string

	// Seeded reports that Catalog is the catalog the zone's nodes were already
	// deriving for themselves, written down for the first time. Nothing about
	// the universe changes, so the step keeps the epoch it is published under
	// rather than spending a new one.
	Seeded bool
}

// Group is one catalog entry: the three nodes that hold a group, one per
// cohort. Position is normative, so a group is an array and not a set.
type Group [Cohorts]uint32

// Contains reports whether a node holds this group.
func (g Group) Contains(nodeID uint32) bool {
	for _, id := range g {
		if id == nodeID {
			return true
		}
	}

	return false
}

// Survivors counts how many of a group's nodes are still in another version of
// the same group. Two is the number that matters: a group that keeps two of
// three can still reach a quorum and replay the third.
func (g Group) Survivors(other Group) int {
	var kept int

	for cohort := range Cohorts {
		if g[cohort] != 0 && g[cohort] == other[cohort] {
			kept++
		}
	}

	return kept
}

// Catalog is a zone's assignment of consensus groups to nodes, published as
// state rather than derived from the member list.
//
// It has to be published. The catalog is what folds a slot onto a group and
// what every anti-entropy key is drawn from, so recomputing it from a changed
// member list moves data that had no reason to move, and moves it in every
// group at once. Published, a membership change is a list of individual slots
// changing hands, and every group that is not one of them stays exactly where
// it was.
type Catalog []Group

// Members is the membership a catalog describes: every node it names, in the
// column it occupies. The column is the cohort, which is why a node may appear
// in only one of them.
func (c Catalog) Members() Membership {
	seen := make(map[uint32]uint32, Cohorts*4)

	for _, group := range c {
		for cohort := range Cohorts {
			if group[cohort] != 0 {
				seen[group[cohort]] = uint32(cohort)
			}
		}
	}

	members := make(Membership, 0, len(seen))
	for id, cohort := range seen {
		members = append(members, Member{NodeID: id, Cohort: cohort})
	}

	return members.Normalized()
}

// Equal reports whether two catalogs name the same nodes in the same positions.
func (c Catalog) Equal(other Catalog) bool {
	if len(c) != len(other) {
		return false
	}

	for i := range c {
		if c[i] != other[i] {
			return false
		}
	}

	return true
}

// Load counts the groups each node holds. This is a node's whole share of the
// zone: there is no per-node weight anywhere else.
func (c Catalog) Load() map[uint32]int {
	load := make(map[uint32]int)

	for _, group := range c {
		for cohort := range Cohorts {
			if group[cohort] != 0 {
				load[group[cohort]]++
			}
		}
	}

	return load
}

// Column returns the ids holding one cohort's slot in every group, in group
// order. Rebalancing happens within a column, because a node's cohort is frozen
// and a column only ever holds nodes of that cohort.
func (c Catalog) Column(cohort int) []uint32 {
	ids := make([]uint32, len(c))
	for i, group := range c {
		ids[i] = group[cohort]
	}

	return ids
}

// Clone returns a copy that can be moved around without disturbing the
// published one.
func (c Catalog) Clone() Catalog {
	out := make(Catalog, len(c))
	copy(out, c)

	return out
}

// Trios renders a catalog into the schema's form.
func (c Catalog) Trios() []*racerconfig.Trio {
	trios := make([]*racerconfig.Trio, 0, len(c))
	for _, group := range c {
		trios = append(trios, &racerconfig.Trio{
			Cohort_0: group[0],
			Cohort_1: group[1],
			Cohort_2: group[2],
		})
	}

	return trios
}

// CatalogOf reads a catalog back out of the schema's form.
func CatalogOf(trios []*racerconfig.Trio) Catalog {
	catalog := make(Catalog, 0, len(trios))
	for _, trio := range trios {
		catalog = append(catalog, Group{
			trio.GetCohort_0(), trio.GetCohort_1(), trio.GetCohort_2(),
		})
	}

	return catalog
}

// Validate rejects a catalog racer would refuse: an empty one, a group with a
// zero id, or a group naming the same node twice.
func (c Catalog) Validate() error {
	if len(c) == 0 {
		return fmt.Errorf("catalog is empty")
	}

	for i, group := range c {
		for cohort := range Cohorts {
			if group[cohort] == 0 {
				return fmt.Errorf("catalog group %d has no node in cohort %d", i, cohort)
			}
		}

		if group[0] == group[1] || group[0] == group[2] || group[1] == group[2] {
			return fmt.Errorf("catalog group %d names a node twice: %v", i, group)
		}
	}

	return nil
}

// catalogGroupSeparator and catalogNodeSeparator render a catalog compactly:
// a couple of thousand groups have to fit in one ConfigMap value.
const (
	catalogGroupSeparator = ","
	catalogNodeSeparator  = ":"
)

// FormatCatalog renders a catalog to its ConfigMap value.
func FormatCatalog(c Catalog) string {
	if len(c) == 0 {
		return ""
	}

	var builder strings.Builder

	for i, group := range c {
		if i > 0 {
			builder.WriteString(catalogGroupSeparator)
		}

		for cohort := range Cohorts {
			if cohort > 0 {
				builder.WriteString(catalogNodeSeparator)
			}

			builder.WriteString(strconv.FormatUint(uint64(group[cohort]), 10))
		}
	}

	return builder.String()
}

// ParseCatalog reads a catalog back. An empty value is no catalog, not an
// error: a zone that has never been published has none.
func ParseCatalog(raw string) (Catalog, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	groups := strings.Split(raw, catalogGroupSeparator)
	catalog := make(Catalog, 0, len(groups))

	for i, entry := range groups {
		ids := strings.Split(strings.TrimSpace(entry), catalogNodeSeparator)
		if len(ids) != Cohorts {
			return nil, fmt.Errorf("catalog group %d has %d nodes, want %d", i, len(ids), Cohorts)
		}

		var group Group

		for cohort := range Cohorts {
			id, err := strconv.ParseUint(strings.TrimSpace(ids[cohort]), 10, 32)
			if err != nil {
				return nil, fmt.Errorf("catalog group %d cohort %d: %w", i, cohort, err)
			}

			group[cohort] = uint32(id)
		}

		catalog = append(catalog, group)
	}

	if err := catalog.Validate(); err != nil {
		return nil, err
	}

	return catalog, nil
}

// Equal reports whether two memberships name the same nodes in the same
// cohorts, whatever order they are written in.
func (m Membership) Equal(other Membership) bool {
	if len(m) != len(other) {
		return false
	}

	left := m.Normalized()
	right := other.Normalized()

	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}

	return true
}
