// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"sort"
	"strconv"

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

// MembershipStep is one move toward a desired membership, with the generation
// stride the move needs.
type MembershipStep struct {
	// Next is the membership to publish.
	Next Membership

	// Stride is how far the node generation must advance for the dataplane to
	// accept the move. A single swap is an incremental handoff and takes one
	// generation. Anything larger is not a transient the dataplane can reason
	// about, so it is delivered as a settled state by skipping a generation,
	// which is the escape hatch the schema names.
	Stride uint64

	// Done reports that Next already is the desired membership.
	Done bool
}

// NextMembership advances current one step toward desired.
//
// Consecutive generations may differ by at most one id, so ordinary churn is a
// sequence of single swaps: the newcomer inherits the departing node's groups,
// replays them, and the departing node drops what it held once the new members
// confirm. Resizing the catalog cannot be expressed that way - it moves three
// nodes at once, one per cohort - so it is delivered as a whole new settled
// state, and the caller is expected to have quiesced the universe first.
func NextMembership(current, desired Membership, catalogSize int) (MembershipStep, error) {
	currentPer, currentErr := current.PerCohort()

	desiredPer, desiredErr := desired.PerCohort()
	if desiredErr != nil {
		return MembershipStep{}, fmt.Errorf("desired membership: %w", desiredErr)
	}

	// No usable current membership: this universe has not been published to this
	// zone before, so there is no handoff to be incremental about.
	if currentErr != nil || currentPer == 0 {
		return MembershipStep{Next: desired.Normalized(), Stride: 1}, nil
	}

	if sameMembership(current, desired) {
		return MembershipStep{Next: current.Normalized(), Stride: 0, Done: true}, nil
	}

	if currentPer != desiredPer {
		return MembershipStep{Next: desired.Normalized(), Stride: 2}, nil
	}

	// Same size, different members: swap exactly one node, keeping cohorts intact
	// so the catalog stays balanced at every step.
	currentByCohort := current.Normalized().ByCohort()
	desiredByCohort := desired.Normalized().ByCohort()

	for cohort := range Cohorts {
		leaving, joining, ok := firstDifference(currentByCohort[cohort], desiredByCohort[cohort])
		if !ok {
			continue
		}

		next := make(Membership, 0, len(current))

		for _, member := range current.Normalized() {
			if member.NodeID == leaving {
				next = append(next, Member{NodeID: joining, Cohort: uint32(cohort)})

				continue
			}

			next = append(next, member)
		}

		return MembershipStep{Next: next.Normalized(), Stride: 1}, nil
	}

	return MembershipStep{Next: current.Normalized(), Stride: 0, Done: true}, nil
}

// firstDifference finds the lowest id in current that is absent from desired,
// paired with the lowest id in desired that is absent from current. Both lists
// are the same length, so one implies the other.
func firstDifference(current, desired []uint32) (leaving, joining uint32, ok bool) {
	inDesired := make(map[uint32]bool, len(desired))
	for _, id := range desired {
		inDesired[id] = true
	}

	inCurrent := make(map[uint32]bool, len(current))
	for _, id := range current {
		inCurrent[id] = true
	}

	for _, id := range current {
		if !inDesired[id] {
			leaving = id
			ok = true

			break
		}
	}

	if !ok {
		return 0, 0, false
	}

	for _, id := range desired {
		if !inCurrent[id] {
			return leaving, id, true
		}
	}

	return 0, 0, false
}

func sameMembership(a, b Membership) bool {
	if len(a) != len(b) {
		return false
	}

	left := a.Normalized()
	right := b.Normalized()

	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}

	return true
}
