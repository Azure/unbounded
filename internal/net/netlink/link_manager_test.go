// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/vishvananda/netlink"
)

// These tests swap the package-level netlinkLink* seams, so none of them may
// call t.Parallel(): parallel subtests would overwrite each other's stubs and
// the failures would look like the code under test misbehaving.

// errTransient stands in for a netlink failure that says nothing about
// whether the interface exists: a busy socket, a permissions problem, a
// truncated response. The distinction under test is between this and the
// "gone" errors that isLinkGoneError recognises.
var errTransient = fmt.Errorf("netlink busy: %w", syscall.EBUSY)

// goneErrors are the three shapes isLinkGoneError treats as "not there".
//
// netlink.LinkNotFoundError embeds an error interface that is unexported, so
// a composite literal from outside that package can only leave it nil, and
// calling Error() on this value would panic. That is safe here only because
// every path these tests reach checks isLinkGoneError before formatting the
// error. Adding a log line on a gone path would turn these tests into panics.
func goneErrors() map[string]error {
	return map[string]error{
		"LinkNotFoundError": netlink.LinkNotFoundError{},
		"ENODEV":            syscall.ENODEV,
		"ENOENT":            syscall.ENOENT,
	}
}

// stubLinks redirects the three netlink link seams for the duration of a test
// and restores them afterwards, following the convention in
// gateway_policy_manager_mock_test.go.
//
// addErr and delErr default to a sentinel, so a test that does not opt in
// fails loudly if the code under test mutates anything. Tests that exercise
// the delete-and-recreate paths set delErr to nil so execution carries on to
// the create.
type linkStubs struct {
	addCalls int
	delCalls int
	addErr   error
	delErr   error
}

var errStubNotExpected = errors.New("netlink mutation should not have been reached")

func stubLinks(t *testing.T, byName func(string) (netlink.Link, error)) *linkStubs {
	t.Helper()

	origByName, origAdd, origDel := netlinkLinkByName, netlinkLinkAdd, netlinkLinkDel

	t.Cleanup(func() {
		netlinkLinkByName, netlinkLinkAdd, netlinkLinkDel = origByName, origAdd, origDel
	})

	s := &linkStubs{addErr: errStubNotExpected, delErr: errStubNotExpected}

	netlinkLinkByName = byName
	netlinkLinkAdd = func(netlink.Link) error {
		s.addCalls++

		return s.addErr
	}
	netlinkLinkDel = func(netlink.Link) error {
		s.delCalls++

		return s.delErr
	}

	return s
}

func lookupFails(err error) func(string) (netlink.Link, error) {
	return func(string) (netlink.Link, error) { return nil, err }
}

func lookupReturns(link netlink.Link) func(string) (netlink.Link, error) {
	return func(string) (netlink.Link, error) { return link, nil }
}

// ensureFuncs are the interface-creating methods that decide whether to
// create based on a link lookup. Each must refuse to create when the lookup
// fails for a reason other than the interface being absent.
func ensureFuncs() map[string]func(*LinkManager) error {
	return map[string]func(*LinkManager) error{
		"EnsureIPIPInterfaceWithRemote": func(lm *LinkManager) error {
			return lm.EnsureIPIPInterfaceWithRemote(net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2))
		},
		"EnsureIPIPExternalInterface": func(lm *LinkManager) error {
			return lm.EnsureIPIPExternalInterface()
		},
		"EnsureGeneveInterfaceWithRemote": func(lm *LinkManager) error {
			return lm.EnsureGeneveInterfaceWithRemote(1, 6081, net.IPv4(10, 0, 0, 2))
		},
		"EnsureGeneveInterface": func(lm *LinkManager) error {
			return lm.EnsureGeneveInterface(0, 6081, nil)
		},
		"EnsureVXLANInterface": func(lm *LinkManager) error {
			return lm.EnsureVXLANInterface(4789, 30000, 40000, nil)
		},
		"EnsureWireGuardInterface": func(lm *LinkManager) error {
			return lm.EnsureWireGuardInterface()
		},
		"EnsureBridge": func(lm *LinkManager) error {
			return lm.EnsureBridge()
		},
		"EnsureDummyInterface": func(lm *LinkManager) error {
			return lm.EnsureDummyInterface()
		},
		"EnsureGeneveInterfaceWithCache": func(lm *LinkManager) error {
			return lm.EnsureGeneveInterfaceWithCache(nil, 1, 6081, net.IPv4(10, 0, 0, 2))
		},
		"EnsureIPIPInterfaceWithCache": func(lm *LinkManager) error {
			return lm.EnsureIPIPInterfaceWithCache(nil, net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2))
		},
	}
}

// A lookup failure that does not mean "absent" must not be read as "absent".
// Before this was fixed every one of these would fall through and try to
// create an interface that may well already exist.
func TestEnsureRefusesToCreateWhenLookupFails(t *testing.T) {
	for name, ensure := range ensureFuncs() {
		t.Run(name, func(t *testing.T) {
			stubs := stubLinks(t, lookupFails(errTransient))

			err := ensure(NewLinkManager("unb0"))
			if err == nil {
				t.Fatal("expected an error when the link lookup fails for a non-absent reason")
			}

			if !errors.Is(err, syscall.EBUSY) {
				t.Fatalf("expected the underlying lookup error to be wrapped, got %v", err)
			}

			if stubs.addCalls != 0 {
				t.Fatalf("interface creation attempted despite a failed lookup (%d calls)", stubs.addCalls)
			}

			if stubs.delCalls != 0 {
				t.Fatalf("interface deletion attempted despite a failed lookup (%d calls)", stubs.delCalls)
			}
		})
	}
}

// The absent case must still reach creation, otherwise the fix above would
// have disabled interface creation entirely.
func TestEnsureStillCreatesWhenLinkIsAbsent(t *testing.T) {
	for name, ensure := range ensureFuncs() {
		for errName, goneErr := range goneErrors() {
			t.Run(name+"/"+errName, func(t *testing.T) {
				stubs := stubLinks(t, lookupFails(goneErr))

				// stubLinks makes LinkAdd fail, so the error we expect back is
				// that one. Reaching it is the point: it proves the method got
				// past the lookup and tried to create.
				if err := ensure(NewLinkManager("unb0")); err == nil {
					t.Fatal("expected the stubbed LinkAdd error to surface")
				}

				if stubs.addCalls != 1 {
					t.Fatalf("expected exactly one creation attempt, got %d", stubs.addCalls)
				}
			})
		}
	}
}

// DeleteLink is the site that reported success without doing anything: any
// lookup error was read as "already gone".
func TestDeleteLinkFailsWhenLookupFails(t *testing.T) {
	stubs := stubLinks(t, lookupFails(errTransient))

	err := NewLinkManager("unb0").DeleteLink()
	if err == nil {
		t.Fatal("expected an error rather than a silent success when the lookup fails")
	}

	if !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("expected the underlying lookup error to be wrapped, got %v", err)
	}

	if stubs.delCalls != 0 {
		t.Fatalf("deletion attempted despite a failed lookup (%d calls)", stubs.delCalls)
	}
}

// Deleting an interface that is already absent is still success: the caller
// asked for it to be gone and it is.
func TestDeleteLinkIsNilWhenAlreadyAbsent(t *testing.T) {
	for errName, goneErr := range goneErrors() {
		t.Run(errName, func(t *testing.T) {
			stubs := stubLinks(t, lookupFails(goneErr))

			if err := NewLinkManager("unb0").DeleteLink(); err != nil {
				t.Fatalf("expected nil for an already-absent interface, got %v", err)
			}

			if stubs.delCalls != 0 {
				t.Fatalf("deletion attempted for an absent interface (%d calls)", stubs.delCalls)
			}
		})
	}
}

// Exists deliberately keeps its bool signature: every caller is a reconcile
// predicate that retries. It must still report false rather than panic or
// report true when the lookup breaks.
func TestExistsReportsFalseOnAnyLookupFailure(t *testing.T) {
	cases := goneErrors()
	cases["transient"] = errTransient

	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			stubLinks(t, lookupFails(err))

			if NewLinkManager("unb0").Exists() {
				t.Fatal("expected Exists to report false when the lookup fails")
			}
		})
	}
}

func TestExistsReportsTrueWhenLookupSucceeds(t *testing.T) {
	stubLinks(t, func(string) (netlink.Link, error) {
		return &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "unb0"}}, nil
	})

	if !NewLinkManager("unb0").Exists() {
		t.Fatal("expected Exists to report true when the lookup succeeds")
	}
}

// lookupError is what the call sites above share, so pin its contract
// directly as well.
func TestLookupErrorDistinguishesAbsentFromBroken(t *testing.T) {
	lm := NewLinkManager("unb0")

	for name, goneErr := range goneErrors() {
		t.Run(name+"/absent is not an error", func(t *testing.T) {
			if err := lm.lookupError(goneErr); err != nil {
				t.Fatalf("expected nil for an absent interface, got %v", err)
			}
		})
	}

	t.Run("broken lookup is an error naming the interface", func(t *testing.T) {
		err := lm.lookupError(errTransient)
		if err == nil {
			t.Fatal("expected an error for a lookup that failed for another reason")
		}

		if !errors.Is(err, syscall.EBUSY) {
			t.Fatalf("expected the cause to be wrapped, got %v", err)
		}
	})
}

// A nil error is not a failed lookup. isLinkGoneError(nil) is false, so a
// lookupError that only consulted it would wrap nil and hand back a non-nil
// error rendering as "%!w(<nil>)". The recreate paths below reach lookupError
// with err == nil after deleting an interface they are about to recreate, and
// would abort on that phantom error instead of recreating.
func TestLookupErrorIsNilForNilError(t *testing.T) {
	if err := NewLinkManager("unb0").lookupError(nil); err != nil {
		t.Fatalf("expected nil for a lookup that succeeded, got %v", err)
	}
}

// recreateCase describes an interface that already exists but in the wrong
// mode, so the manager has to delete it and create it again.
type recreateCase struct {
	name        string
	existing    netlink.Link
	ensure      func(*LinkManager) error
	wantDelete  bool
	description string
}

func recreateCases() []recreateCase {
	ipipExternal := func(lm *LinkManager) error { return lm.EnsureIPIPExternalInterface() }
	geneveExternal := func(lm *LinkManager) error { return lm.EnsureGeneveInterface(0, 6081, nil) }

	return []recreateCase{
		{
			name:        "IPIP not flow-based is recreated",
			existing:    &netlink.Iptun{LinkAttrs: netlink.LinkAttrs{Name: "unb0"}, FlowBased: false},
			ensure:      ipipExternal,
			wantDelete:  true,
			description: "an IPIP interface left over in point-to-point mode has to be replaced",
		},
		{
			name:        "IPIP already flow-based is left alone",
			existing:    &netlink.Iptun{LinkAttrs: netlink.LinkAttrs{Name: "unb0"}, FlowBased: true},
			ensure:      ipipExternal,
			wantDelete:  false,
			description: "already in the desired mode, so nothing should be touched",
		},
		{
			name:        "GENEVE not flow-based is recreated",
			existing:    &netlink.Geneve{LinkAttrs: netlink.LinkAttrs{Name: "unb0"}, FlowBased: false},
			ensure:      geneveExternal,
			wantDelete:  true,
			description: "a GENEVE interface left over in point-to-point mode has to be replaced",
		},
		{
			name:        "GENEVE with a fixed VNI is recreated",
			existing:    &netlink.Geneve{LinkAttrs: netlink.LinkAttrs{Name: "unb0"}, FlowBased: true, ID: 5},
			ensure:      geneveExternal,
			wantDelete:  true,
			description: "flow-based but pinned to a VNI, which the eBPF dataplane cannot use",
		},
		{
			name:        "GENEVE already external is left alone",
			existing:    &netlink.Geneve{LinkAttrs: netlink.LinkAttrs{Name: "unb0"}, FlowBased: true, ID: 0},
			ensure:      geneveExternal,
			wantDelete:  false,
			description: "already in the desired mode, so nothing should be touched",
		},
	}
}

// The delete-and-recreate paths reach the lookup guard with err == nil. They
// must carry on to the create rather than report a lookup failure that did
// not happen, and the paths that need no change must touch nothing at all.
func TestEnsureRecreatesInterfaceInTheWrongMode(t *testing.T) {
	for _, tc := range recreateCases() {
		t.Run(tc.name, func(t *testing.T) {
			stubs := stubLinks(t, lookupReturns(tc.existing))
			// Let the delete succeed so execution reaches the create.
			stubs.delErr = nil
			stubs.addErr = nil

			if err := tc.ensure(NewLinkManager("unb0")); err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.description, err)
			}

			wantDeletes, wantAdds := 0, 0
			if tc.wantDelete {
				wantDeletes, wantAdds = 1, 1
			}

			if stubs.delCalls != wantDeletes {
				t.Fatalf("%s: expected %d deletes, got %d", tc.description, wantDeletes, stubs.delCalls)
			}

			if stubs.addCalls != wantAdds {
				t.Fatalf("%s: expected %d creates, got %d", tc.description, wantAdds, stubs.addCalls)
			}
		})
	}
}

// A delete that fails must abort rather than fall through to the create,
// which would leave the old interface in place and report success.
func TestEnsureStopsWhenRecreateDeleteFails(t *testing.T) {
	for _, tc := range recreateCases() {
		if !tc.wantDelete {
			continue
		}

		t.Run(tc.name, func(t *testing.T) {
			stubs := stubLinks(t, lookupReturns(tc.existing))
			stubs.delErr = errTransient

			if err := tc.ensure(NewLinkManager("unb0")); err == nil {
				t.Fatal("expected the delete failure to surface")
			}

			if stubs.addCalls != 0 {
				t.Fatalf("creation attempted after a failed delete (%d calls)", stubs.addCalls)
			}
		})
	}
}

// The counter exists so that lookups failing for a reason other than absence
// are visible in Prometheus, not only in logs. Exists() is the path where that
// matters most: it swallows the condition entirely and returns a plain bool,
// so without the counter a node whose netlink lookups are failing looks
// identical to one whose interfaces are legitimately absent.
func TestExistsCountsLookupFailuresThatAreNotAbsence(t *testing.T) {
	counter := InterfaceOperationErrors.WithLabelValues("lookup")

	t.Run("a broken lookup is counted", func(t *testing.T) {
		stubLinks(t, lookupFails(errTransient))

		before := testutil.ToFloat64(counter)

		NewLinkManager("unb0").Exists()

		if got := testutil.ToFloat64(counter) - before; got != 1 {
			t.Fatalf("expected the lookup failure to be counted once, got %v", got)
		}
	})

	t.Run("an absent interface is not counted", func(t *testing.T) {
		for name, goneErr := range goneErrors() {
			t.Run(name, func(t *testing.T) {
				stubLinks(t, lookupFails(goneErr))

				before := testutil.ToFloat64(counter)

				NewLinkManager("unb0").Exists()

				if got := testutil.ToFloat64(counter) - before; got != 0 {
					t.Fatalf("an absent interface is not an error and must not be counted, got %v", got)
				}
			})
		}
	})
}
