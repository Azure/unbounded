//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// maxSeats is the number of precreated racer-seat-N Leases.
	maxSeats = 16
	// seatFloor is the default minimum seat count when enough eligible
	// pods are ready.
	seatFloor        = 3
	primaryLease     = "racer-primary"
	racerStateSecret = "racer-state"
	webhookName      = "racer-webhook"
	webhookPort      = 9443
	dataplaneLabel   = "app.kubernetes.io/name=racer"
	// leaseSkew tolerates clock skew between the host and kind nodes when
	// judging whether a Lease has expired.
	leaseSkew = 5 * time.Second
)

// lease is the subset of a coordination.k8s.io Lease the suite inspects.
type lease struct {
	Name        string
	Holder      string
	HolderUID   string
	Transitions int64
	Duration    time.Duration
	RenewTime   time.Time
}

// unexpired reports whether l was renewed within its duration.
func (l lease) unexpired(now time.Time) bool {
	return l.Duration > 0 && !l.RenewTime.IsZero() && now.Before(l.RenewTime.Add(l.Duration+leaseSkew))
}

// dataplanePod is a racer pod that may hold a seat.
type dataplanePod struct {
	Name  string
	UID   string
	Node  string
	IP    string
	Ready bool
}

// seat is a seat Lease and the live pod that holds it.
type seat struct {
	Lease string
	Pod   dataplanePod
}

// controlView is a point-in-time snapshot of the embedded control plane.
type controlView struct {
	Now     time.Time
	Seats   map[string]lease
	Primary lease
	Pods    []dataplanePod
}

type leaseList struct {
	Items []struct {
		Metadata struct {
			Name        string
			Annotations map[string]string
		}
		Spec struct {
			HolderIdentity       *string
			LeaseTransitions     *int64
			LeaseDurationSeconds *int64
			RenewTime            *time.Time
		}
	}
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name              string
			UID               string
			DeletionTimestamp *string
		}
		Spec   struct{ NodeName string }
		Status struct {
			PodIP      string
			Conditions []struct{ Type, Status string }
		}
	}
}

// controlView reads the seat and primary Leases and the racer pods.
// Read errors yield an empty view so callers polling with waitFor retry.
func (h *harness) controlView() controlView {
	h.t.Helper()

	v := controlView{Now: time.Now(), Seats: map[string]lease{}}

	var leases leaseList
	if !h.getJSON(&leases, "-n", operatorNamespace, "get", "leases") {
		return v
	}

	for _, it := range leases.Items {
		l := lease{
			Name:      it.Metadata.Name,
			HolderUID: it.Metadata.Annotations["racer.unbounded-cloud.io/holder-uid"],
		}
		if s := it.Spec; s.HolderIdentity != nil {
			l.Holder = *s.HolderIdentity
		}

		if s := it.Spec; s.LeaseTransitions != nil {
			l.Transitions = *s.LeaseTransitions
		}

		if s := it.Spec; s.LeaseDurationSeconds != nil {
			l.Duration = time.Duration(*s.LeaseDurationSeconds) * time.Second
		}

		if s := it.Spec; s.RenewTime != nil {
			l.RenewTime = *s.RenewTime
		}

		switch {
		case l.Name == primaryLease:
			v.Primary = l
		case strings.HasPrefix(l.Name, "racer-seat-"):
			v.Seats[l.Name] = l
		}
	}

	var pods podList
	if !h.getJSON(&pods, "-n", operatorNamespace, "get", "pods", "-l", dataplaneLabel) {
		return v
	}

	for _, it := range pods.Items {
		if it.Metadata.DeletionTimestamp != nil {
			continue
		}

		p := dataplanePod{Name: it.Metadata.Name, UID: it.Metadata.UID, Node: it.Spec.NodeName, IP: it.Status.PodIP}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				p.Ready = true
			}
		}

		v.Pods = append(v.Pods, p)
	}

	return v
}

// getJSON runs kubectl with -o json and decodes the result into out.
func (h *harness) getJSON(out any, args ...string) bool {
	h.t.Helper()

	raw, err := h.exec(time.Minute, nil, "kubectl", append(args, "-o", "json")...)
	if err != nil {
		return false
	}

	return json.Unmarshal([]byte(raw), out) == nil
}

// livePod checks the holder name and the bound Pod UID from the Lease.
func (v controlView) livePod(l lease) (dataplanePod, bool) {
	if l.Holder == "" || l.HolderUID == "" {
		return dataplanePod{}, false
	}

	for _, p := range v.Pods {
		if l.Holder == p.Name && l.HolderUID == p.UID {
			return p, true
		}
	}

	return dataplanePod{}, false
}

// validSeats returns unexpired seat Leases held by live, scheduled pods,
// sorted by Lease name. Duplicates are kept so seatProblems can report them.
func (v controlView) validSeats() []seat {
	var seats []seat

	for name, l := range v.Seats {
		p, ok := v.livePod(l)
		if ok && p.Node != "" && l.unexpired(v.Now) {
			seats = append(seats, seat{Lease: name, Pod: p})
		}
	}

	sort.Slice(seats, func(i, j int) bool { return seats[i].Lease < seats[j].Lease })

	return seats
}

// seatProblems lists invariant violations among valid seats: one pod or one
// node holding more than one seat.
func (v controlView) seatProblems() []string {
	var problems []string

	byPod := map[string]string{}
	byNode := map[string]string{}

	for _, s := range v.validSeats() {
		if prev, ok := byPod[s.Pod.UID]; ok {
			problems = append(problems, fmt.Sprintf("pod %s holds %s and %s", s.Pod.Name, prev, s.Lease))
		}

		if prev, ok := byNode[s.Pod.Node]; ok {
			problems = append(problems, fmt.Sprintf("node %s holds %s and %s", s.Pod.Node, prev, s.Lease))
		}

		byPod[s.Pod.UID] = s.Lease
		byNode[s.Pod.Node] = s.Lease
	}

	return problems
}

// readyPods counts ready racer pods.
func (v controlView) readyPods() int {
	n := 0

	for _, p := range v.Pods {
		if p.Ready {
			n++
		}
	}

	return n
}

// wantSeats is the minimum number of valid seats for the ready pods.
func (v controlView) wantSeats() int {
	return min(seatFloor, v.readyPods())
}

// primarySeat returns the pod holding an unexpired racer-primary Lease when
// that pod also holds a valid seat.
func (v controlView) primarySeat() (dataplanePod, bool) {
	p, ok := v.livePod(v.Primary)
	if !ok || !v.Primary.unexpired(v.Now) {
		return dataplanePod{}, false
	}

	for _, s := range v.validSeats() {
		if s.Pod.UID == p.UID {
			return p, true
		}
	}

	return dataplanePod{}, false
}

// healthy reports whether the view has a seated primary and enough valid
// seats on distinct pods and nodes.
func (v controlView) healthy() bool {
	_, ok := v.primarySeat()

	return ok && v.wantSeats() > 0 && len(v.validSeats()) >= v.wantSeats() && len(v.seatProblems()) == 0
}

// seatIPs returns the pod IPs of valid seat holders.
func (v controlView) seatIPs() map[string]bool {
	ips := map[string]bool{}

	for _, s := range v.validSeats() {
		if s.Pod.IP != "" {
			ips[s.Pod.IP] = true
		}
	}

	return ips
}

func (v controlView) String() string {
	seats := make([]string, 0, len(v.Seats))
	for _, s := range v.validSeats() {
		seats = append(seats, s.Lease+"="+s.Pod.Name+"@"+s.Pod.Node)
	}

	return fmt.Sprintf("primary=%q transitions=%d validSeats=%v problems=%v readyPods=%d/%d",
		v.Primary.Holder, v.Primary.Transitions, seats, v.seatProblems(), v.readyPods(), len(v.Pods))
}
