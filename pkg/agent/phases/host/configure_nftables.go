// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

const (
	nftablesFlushUnit = goalstates.NFTablesFlushUnit
	nftablesClearPath = goalstates.NFTablesClearPath
)

//go:embed assets/nftables-flush.service
var nftablesFlushServiceTmpl string

var nftablesFlushServiceTemplate = template.Must(
	template.New("nftables-flush.service").Parse(nftablesFlushServiceTmpl),
)

//go:embed assets/nftables-flush-node.service
var nftablesFlushNodeServiceTmpl string

var nftablesFlushNodeServiceTemplate = template.Must(
	template.New("nftables-flush-node.service").Parse(nftablesFlushNodeServiceTmpl),
)

//go:embed assets/nftables-clear.nft
var nftablesClearRules []byte

type configureNFTables struct {
	log *slog.Logger
	// machineRegistered reports whether a node is already registered (an nspawn
	// machine, or the running node units in the other modes), and is injectable
	// so the replay behavior can be tested without a host.
	machineRegistered func(context.Context, *slog.Logger) (bool, error)
	// nodeUnits are the host units of the node stack in the isolation modes
	// that do not run an nspawn machine. Empty selects the nspawn unit, which
	// the machines order themselves after.
	nodeUnits []string
}

// ConfigureNFTables returns a task that installs a oneshot systemd unit which
// flushes all nftables rules to a clean state before kubelet starts.
// This ensures stale rules (e.g. left behind by Docker) do not interfere with
// Kubernetes networking.
func ConfigureNFTables(log *slog.Logger) phases.Task {
	return ConfigureNFTablesFor(log, isolation.ModeNSpawn)
}

// ConfigureNFTablesFor is ConfigureNFTables for an isolation mode. The nspawn
// mode, and the empty mode, install exactly the unit they always did. The other
// modes have no nspawn machine to order against and no machined to ask about
// registered machines (their hosts do not install systemd-container), so the
// unit is ordered before the node's own units and "is a node running" is
// answered from those units.
func ConfigureNFTablesFor(log *slog.Logger, mode isolation.Mode) phases.Task {
	switch mode {
	case isolation.ModeNone:
		units := []string{none.UnitContainerd, none.UnitKubelet}

		return &configureNFTables{log: log, nodeUnits: units, machineRegistered: anyUnitActive(units)}
	default:
		return &configureNFTables{log: log, machineRegistered: anyMachineRegistered}
	}
}

// anyUnitActive reports whether any of the units is active or changing state.
// It fails closed like anyMachineRegistered: only a unit systemctl reports as
// inactive or failed (which includes a unit that does not exist) counts as not
// running. Any other answer, or a systemctl that cannot be asked, is an error,
// so the live ruleset of a running node is never flushed on a guess.
func anyUnitActive(units []string) func(context.Context, *slog.Logger) (bool, error) {
	return anyUnitActiveWith(units, systemctlIsActive)
}

// unitStateFunc returns what `systemctl is-active <unit>` printed and the error
// it exited with.
type unitStateFunc func(ctx context.Context, unit string) (string, error)

func anyUnitActiveWith(units []string, state unitStateFunc) func(context.Context, *slog.Logger) (bool, error) {
	return func(ctx context.Context, _ *slog.Logger) (bool, error) {
		for _, unit := range units {
			out, err := state(ctx, unit)

			switch strings.TrimSpace(out) {
			case "active", "activating", "deactivating", "reloading", "refreshing":
				return true, nil
			case "inactive", "failed":
				// is-active exits non-zero for these; that is the answer, not
				// a failure to get one.
				continue
			}

			if err == nil {
				err = errors.New("unexpected state")
			}

			return false, fmt.Errorf("determine whether %s is running (systemctl is-active printed %q): %w", unit, out, err)
		}

		return false, nil
	}
}

// systemctlIsActive runs `systemctl is-active <unit>` and returns its output
// whatever its exit status.
func systemctlIsActive(ctx context.Context, unit string) (string, error) {
	cmd := executil.Systemctl()(ctx)
	cmd.Args = append(cmd.Args, "is-active", unit)

	out, err := cmd.Output()

	return string(out), err
}

// anyMachineRegistered reports whether either node slot is registered. It fails
// closed: an uninspectable host is not reported as having no machine.
func anyMachineRegistered(ctx context.Context, log *slog.Logger) (bool, error) {
	name, err := reset.FirstRegisteredMachine(ctx, log)
	if err != nil {
		return false, err
	}

	return name != "", nil
}

func (c *configureNFTables) Name() string { return "configure-nftables" }

func (c *configureNFTables) Do(ctx context.Context) error {
	if err := c.ensureNFTablesClearRules(); err != nil {
		return fmt.Errorf("installing nftables-clear rules: %w", err)
	}

	if err := c.ensureNFTablesFlushUnit(ctx); err != nil {
		return fmt.Errorf("configuring nftables-flush service: %w", err)
	}

	return nil
}

// ensureNFTablesClearRules writes the nftables rules file to
// <ConfigDir>/nftables-clear.nft. The file flushes the entire nftables
// ruleset, which resets all tables to a clean state (nftables defaults to
// accept when no rules are loaded).
func (c *configureNFTables) ensureNFTablesClearRules() error {
	return utilio.WriteFile(nftablesClearPath, nftablesClearRules, 0o600)
}

// renderFlushUnit renders the flush unit for the node's isolation mode.
func (c *configureNFTables) renderFlushUnit() (*bytes.Buffer, error) {
	var buf bytes.Buffer

	tmpl, data := nftablesFlushServiceTemplate, map[string]string{"NFTablesClearPath": nftablesClearPath}
	if len(c.nodeUnits) > 0 {
		tmpl = nftablesFlushNodeServiceTemplate
		data["Before"] = strings.Join(c.nodeUnits, " ")
	}

	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering %s template: %w", nftablesFlushUnit, err)
	}

	return &buf, nil
}

// ensureNFTablesFlushUnit installs, enables, and starts the nftables-flush.service
// oneshot unit. The unit runs nft with the clean rules file before any
// systemd-nspawn machine starts.
func (c *configureNFTables) ensureNFTablesFlushUnit(ctx context.Context) error {
	buf, err := c.renderFlushUnit()
	if err != nil {
		return err
	}

	unitPath := filepath.Join(goalstates.SystemdSystemDir, nftablesFlushUnit)

	if err := utilio.WriteFile(unitPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	systemctl := executil.Systemctl()

	if err := executil.RunCmd(ctx, c.log, systemctl, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	if err := executil.RunCmdAt(ctx, c.log, slog.LevelInfo, systemctl, "enable", nftablesFlushUnit); err != nil {
		return fmt.Errorf("systemctl enable %s: %w", nftablesFlushUnit, err)
	}

	// Starting the unit applies `flush ruleset`, which erases every nftables
	// rule on the host. See shouldStartFlush for why that is conditional.
	start, err := c.shouldStartFlush(ctx)
	if err != nil {
		return err
	}

	if !start {
		return nil
	}

	if err := executil.RunCmd(ctx, c.log, systemctl, "start", nftablesFlushUnit); err != nil {
		return fmt.Errorf("systemctl start %s: %w", nftablesFlushUnit, err)
	}

	return nil
}

// shouldStartFlush reports whether the flush unit may be started now.
//
// The flush erases every nftables rule on the host. That is the point on a
// fresh host, and it is safe at boot because the unit is ordered before the
// nspawn machine and LocalDNS re-adds its table after it.
//
// Starting it here is a different thing entirely. The nspawn container shares
// the host network namespace, so a running node's kube-proxy and CNI rules live
// in the ruleset being erased, and LocalDNS's NOTRACK table goes with them.
// Nothing puts it back: systemd ordering only sequences units within a single
// transaction, so starting this unit alone does not pull in
// unbounded-localdns-network.service, and that unit otherwise runs only when
// the machine starts. kube-proxy resyncs on its own; LocalDNS does not.
//
// The flush exists to hand a clean slate to a node that has not started yet.
// Once one is registered it has already served that purpose, so the live
// ruleset is left alone. The unit stays installed and enabled either way, so
// the next boot still gets its clean slate in the correct order.
func (c *configureNFTables) shouldStartFlush(ctx context.Context) (bool, error) {
	registered, err := c.machineRegistered(ctx, c.log)
	if err != nil {
		return false, fmt.Errorf("inspect registered machines before flushing nftables: %w", err)
	}

	if registered {
		what := "nspawn machine"
		if len(c.nodeUnits) > 0 {
			what = "node"
		}

		c.log.Info(what+" is registered; leaving the live nftables ruleset alone",
			"unit", nftablesFlushUnit,
		)

		return false, nil
	}

	return true, nil
}
