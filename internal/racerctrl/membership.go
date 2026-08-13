// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"strconv"
)

// A zone's catalog membership lives in its own ConfigMap rather than in an
// annotation on the universe's StorageClass.
//
// The reason is size. A membership entry costs about fourteen bytes, so a zone
// filled to the thousand-node target is fourteen kilobytes, and a universe may
// span sixty-four zones. Kubernetes caps the annotations of one object at 256
// KiB in total, counted over every key and value, so the annotation form runs
// out somewhere around seventeen full zones and takes the universe's id, epoch
// and LBA cursor down with it. It also means every one-node swap in any zone
// rewrites and refans-out the state of every other zone.
//
// One ConfigMap per universe per zone costs a name and a watch, and gives each
// zone a megabyte it does not share with anything. A node watches only the map
// for its own zone, so a membership step in zone A is not traffic in zone B.
const (
	// MembershipDataKey is the ConfigMap key the membership is stored under, in
	// the same `<nodeID>?cohort=<c>` form the annotation used.
	MembershipDataKey = "members"

	// MembershipEpochKey is the ConfigMap key holding the universe topology
	// epoch this membership was published at.
	//
	// The epoch orders configurations, so a catalog and the epoch that names it
	// have to change together. They cannot both live on the StorageClass (a
	// zone's membership does not fit in an annotation) and Kubernetes has no
	// transaction across two objects, so the epoch travels with the membership
	// instead: one ConfigMap write publishes both, and a reader can never see a
	// new catalog carrying the epoch of the old one.
	//
	// The class's own epoch annotation is then a cursor rather than a value the
	// nodes consume: it is the highest epoch handed out, which is what the next
	// step increments. A membership without this key is from before the epoch
	// moved here; the operator stamps it with the class's epoch, which is the
	// value those nodes were already using.
	MembershipEpochKey = "epoch"

	// MembershipDrainingKey is the ConfigMap key listing the nodes the catalog
	// no longer names but which have not yet handed over what they held, in the
	// same form as MembershipDataKey.
	//
	// A node that is simply dropped from a catalog never learns it was dropped.
	// It would derive no universe at all, publish nothing, and sit on the last
	// configuration that still named it, so racer would never see the catalog
	// that orphans its groups and would never shed them. Shedding is what walks
	// a group the node is no longer in, confirms the new members hold every
	// version, and only then drops the registers, so skipping it either strands
	// the data or drops it before anyone else has it.
	//
	// Naming the node here keeps it deriving that universe, with itself absent
	// from the catalog, which is exactly the configuration that makes racer
	// drain. It also keeps the node in every survivor's peer set and allowed
	// hosts, which it needs because confirming a version is a query it makes
	// against the new members. The operator drops the id once the node reports
	// nothing left to shed.
	MembershipDrainingKey = "draining"

	// MembershipCatalogKey is the ConfigMap key holding the zone's catalog: the
	// groups themselves, as `c0:c1:c2` trios separated by commas.
	//
	// The catalog is published rather than derived from the member list because
	// membership now moves one group slot at a time. A catalog rebuilt from a
	// member list is a function of that list alone, so adding one node would
	// reshuffle every group that node's position touches; publishing it means a
	// step moves exactly the slots it says it moves and nothing else. At the
	// default 2520 groups this is around 30 KiB, well inside the ConfigMap
	// limit.
	//
	// A zone published before this key existed has no catalog. The first pass
	// seeds one from the membership already in force, which reproduces exactly
	// what every node was deriving for itself, so nothing moves.
	MembershipCatalogKey = "catalog"

	// MembershipUniverseLabel carries the universe id, so the operator can list
	// a universe's membership maps without knowing which zones exist.
	MembershipUniverseLabel = AnnotationDomain + "universe-id"

	// MembershipZoneLabel carries the zone id. The node agent selects on it so
	// that it watches exactly one membership map.
	MembershipZoneLabel = AnnotationDomain + "zone"

	// MembershipConfigMapPrefix is what every membership ConfigMap's name
	// begins with, so a watch can match on the name alone.
	MembershipConfigMapPrefix = "racer-u"
)

// MembershipConfigMapName is the name of the ConfigMap holding one zone's
// membership in one universe.
//
// It is derived from the two ids rather than from the StorageClass name because
// the ids are what the dataplane knows: a node that has just read its own zone
// annotation can name the map it needs without first resolving a class.
func MembershipConfigMapName(universe, zone uint32) string {
	return fmt.Sprintf("%s%d-z%d", MembershipConfigMapPrefix, universe, zone)
}

// MembershipLabels are the labels a membership ConfigMap carries.
func MembershipLabels(universe, zone uint32) map[string]string {
	return map[string]string{
		"app.kubernetes.io/part-of": "racer",
		MembershipUniverseLabel:     strconv.FormatUint(uint64(universe), 10),
		MembershipZoneLabel:         strconv.FormatUint(uint64(zone), 10),
	}
}

// ParseMembershipLabels reads the universe and zone a membership ConfigMap is
// for. It reports false for a map that is not one, which is how a shared
// informer over a whole namespace is filtered.
func ParseMembershipLabels(labels map[string]string) (universe, zone uint32, ok bool) {
	universe, err := ParseUint32(labels[MembershipUniverseLabel])
	if err != nil || universe == 0 {
		return 0, 0, false
	}

	zone, err = ParseUint32(labels[MembershipZoneLabel])
	if err != nil || zone == 0 {
		return 0, 0, false
	}

	return universe, zone, true
}

// ParseMembershipEpoch reads the topology epoch a membership was published at.
// Zero means the ConfigMap predates the epoch moving here, and the caller falls
// back to the universe's class annotation.
func ParseMembershipEpoch(data map[string]string) (uint32, error) {
	raw := data[MembershipEpochKey]
	if raw == "" {
		return 0, nil
	}

	epoch, err := ParseUint32(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", MembershipEpochKey, err)
	}

	return epoch, nil
}
