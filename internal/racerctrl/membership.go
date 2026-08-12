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
