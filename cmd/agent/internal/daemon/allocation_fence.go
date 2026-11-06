// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	// allocationLaunchCheckInterval is how often a guard reads again after a
	// transient failure.
	allocationLaunchCheckInterval = 10 * time.Second

	// allocationLaunchCheckTimeout bounds how long one guard call keeps reading
	// before it refuses.
	allocationLaunchCheckTimeout = 5 * time.Minute

	// AllocationLaunchRefusedReason is the condition and MachineOperation
	// reason reported when the guard refuses to start an allocation host's
	// node.
	AllocationLaunchRefusedReason = "AllocationLaunchRefused"

	allocationCredentialNone              = "none"
	allocationCredentialDaemonCertificate = "daemon-certificate"
	allocationCredentialBootstrapToken    = "bootstrap-token"
)

// allocationLaunchConfigPath is the allocation launch record. Bootstrap writes
// it before the node first starts, so every later start can tell that the host
// is allocation-backed even when the starting slot has no applied config yet.
// Tests override it.
var allocationLaunchConfigPath = filepath.Join(goalstates.AgentConfigDir, "allocation-launch-config.json")

// ErrAllocationLaunchRefused reports that an allocation-backed host must not
// start its node: its allocation is fenced, or that could not be ruled out.
var ErrAllocationLaunchRefused = errors.New("allocation launch refused")

var (
	errAllocationIdentityMalformed = errors.New("providerID names no allocation UID")
	errAllocationMachineFenced     = errors.New("machine is fenced")
	errAllocationMachineDeleting   = errors.New("machine is being deleted")
	errAllocationMachineForeign    = errors.New("machine does not belong to this allocation")

	// errAllocationLaunchUnconfirmed marks a refusal because no read answered
	// within the budget, rather than because the cluster said no.
	errAllocationLaunchUnconfirmed = errors.New("allocation could not be confirmed")
)

// IsFinalAllocationLaunchRefusal reports whether err is a refusal that reading
// again cannot change: the cluster answered no, or the host cannot ask. It is
// false for a refusal because no read answered within the budget or the
// context ended, and for any error that is not a refusal.
func IsFinalAllocationLaunchRefusal(err error) bool {
	return errors.Is(err, ErrAllocationLaunchRefused) && !errors.Is(err, errAllocationLaunchUnconfirmed)
}

// AllocationUID reports whether cfg belongs to a host launched for a
// MachineAllocation, and that allocation's UID. A host is allocation-backed
// when its kubelet providerID carries v1alpha3.AllocationProviderIDPrefix. A
// backed config with an empty uid is a malformed identity.
func AllocationUID(cfg *provision.AgentConfig) (uid string, backed bool) {
	providerID, ok := cfg.Kubelet.Configuration["providerID"].(string)
	if !ok {
		return "", false
	}

	uid, backed = strings.CutPrefix(providerID, v1alpha3.AllocationProviderIDPrefix)
	if !backed {
		return "", false
	}

	return uid, true
}

// PersistAllocationLaunchConfig writes the allocation launch record for an
// allocation-backed config. It does nothing for any other config.
func PersistAllocationLaunchConfig(cfg *provision.AgentConfig) error {
	if _, backed := AllocationUID(cfg); !backed {
		return nil
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal allocation launch config: %w", err)
	}

	if err := writeFile(allocationLaunchConfigPath, data, 0o600); err != nil {
		return fmt.Errorf("write allocation launch config to %s: %w", allocationLaunchConfigPath, err)
	}

	return nil
}

// LoadAllocationLaunchConfig reads the allocation launch record. found is false
// when the host has none.
func LoadAllocationLaunchConfig() (*provision.AgentConfig, bool, error) {
	data, err := os.ReadFile(allocationLaunchConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("read allocation launch config %s: %w", allocationLaunchConfigPath, err)
	}

	var cfg provision.AgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, false, fmt.Errorf("decode allocation launch config %s: %w", allocationLaunchConfigPath, err)
	}

	if _, err := provision.ResolveMachineName(&cfg); err != nil {
		return nil, false, fmt.Errorf("resolve allocation launch config machine name %s: %w", allocationLaunchConfigPath, err)
	}

	if err := cfg.BackfillNodeName(); err != nil {
		return nil, false, fmt.Errorf("backfill allocation launch config node name %s: %w", allocationLaunchConfigPath, err)
	}

	return &cfg, true, nil
}

// allocationLaunchOptions configures the guard. Tests replace every field.
type allocationLaunchOptions struct {
	// credentialDir holds the stored daemon-controller certificate.
	credentialDir string
	// newClient builds the client for the chosen credential.
	newClient kubeClientFunc
	// interval is the wait between reads after a transient failure.
	interval time.Duration
	// timeout bounds all reads of one guard call.
	timeout time.Duration
}

func defaultAllocationLaunchOptions() allocationLaunchOptions {
	return allocationLaunchOptions{
		credentialDir: filepath.Join(goalstates.AgentConfigDir, "daemon-controller"),
		newClient:     client.NewWithWatch,
		interval:      allocationLaunchCheckInterval,
		timeout:       allocationLaunchCheckTimeout,
	}
}

// VerifyAllocationLaunch decides whether an allocation-backed host may start
// its node. It returns nil at once for any other host.
//
// Every read selects its credential again. With a stored, unexpired
// daemon-controller certificate the read gets the Machine live and allows only
// a Machine that binds this allocation UID, is not fenced and is not being
// deleted. Otherwise it asks the API server to review the bootstrap token,
// which is revoked when the allocation is fenced. A read never falls back from
// the certificate to the token; a certificate that expires during a call only
// moves the next read to the token.
//
// It fails closed: every refusal wraps ErrAllocationLaunchRefused, and reads
// that keep failing transiently refuse once the retry budget is spent.
func VerifyAllocationLaunch(ctx context.Context, log *slog.Logger, cfg *provision.AgentConfig) error {
	return verifyAllocationLaunch(ctx, log, cfg, defaultAllocationLaunchOptions())
}

func verifyAllocationLaunch(ctx context.Context, log *slog.Logger, cfg *provision.AgentConfig, opts allocationLaunchOptions) error {
	uid, backed := AllocationUID(cfg)
	if !backed {
		return nil
	}

	if uid == "" {
		return refuseAllocationLaunch(log, cfg, uid, allocationCredentialNone, errAllocationIdentityMalformed)
	}

	bootstrapCfg, err := buildBootstrapRESTConfig(cfg)
	if err != nil {
		return refuseAllocationLaunch(log, cfg, uid, allocationCredentialNone, fmt.Errorf("build bootstrap rest config: %w", err))
	}

	storedCertificate := func() (*rest.Config, bool, error) {
		return daemoncred.StoredRESTConfig(bootstrapCfg, cfg.NodeName, daemoncred.ControllerCertificateOptions{
			Name:          daemonControllerCertificateName,
			SignerName:    daemoncred.DefaultControllerCertificateSignerName,
			DaemonGroup:   daemonControllerGroup,
			CredentialDir: opts.credentialDir,
			WaitTimeout:   daemonControllerCertWaitTimeout,
		})
	}

	read := func(ctx context.Context) allocationRead {
		certCfg, found, err := storedCertificate()
		if err != nil {
			return finalAllocationRead(allocationCredentialDaemonCertificate, fmt.Errorf("load stored daemon controller certificate: %w", err))
		}

		if !found {
			return readWithBootstrapToken(ctx, opts, bootstrapCfg)
		}

		return readWithDaemonCertificate(ctx, opts, certCfg, cfg.MachineName, uid, storedCertificate)
	}

	return checkAllocationLaunch(ctx, log, cfg, uid, opts, read)
}

// readWithDaemonCertificate reads the Machine with the stored certificate.
//
// The client presents no certificate once the stored one has expired, so a
// rejection is final only while the certificate is still stored and
// unexpired. A certificate that expired during the read makes the read
// transient, and the next read selects the token.
func readWithDaemonCertificate(
	ctx context.Context,
	opts allocationLaunchOptions,
	certCfg *rest.Config,
	machineName, uid string,
	storedCertificate func() (*rest.Config, bool, error),
) allocationRead {
	c, err := opts.newClient(certCfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return finalAllocationRead(allocationCredentialDaemonCertificate, fmt.Errorf("build daemon controller client: %w", err))
	}

	err = checkAllocationMachine(ctx, c, machineName, uid)
	if err == nil || (!apierrors.IsUnauthorized(err) && !apierrors.IsForbidden(err)) {
		return machineAllocationRead(err)
	}

	_, stillFound, recheckErr := storedCertificate()

	switch {
	case recheckErr != nil:
		return finalAllocationRead(allocationCredentialDaemonCertificate, fmt.Errorf("load stored daemon controller certificate: %w", recheckErr))
	case !stillFound:
		return allocationRead{
			credential: allocationCredentialDaemonCertificate,
			err:        fmt.Errorf("daemon controller certificate expired during the read: %w", err),
		}
	default:
		return finalAllocationRead(allocationCredentialDaemonCertificate, err)
	}
}

// readWithBootstrapToken reviews the bootstrap token.
func readWithBootstrapToken(ctx context.Context, opts allocationLaunchOptions, bootstrapCfg *rest.Config) allocationRead {
	c, err := opts.newClient(bootstrapCfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return finalAllocationRead(allocationCredentialBootstrapToken, fmt.Errorf("build bootstrap client: %w", err))
	}

	err = reviewBootstrapToken(ctx, c)

	return allocationRead{
		credential: allocationCredentialBootstrapToken,
		err:        err,
		final:      err != nil && bootstrapTokenRefusalIsFinal(err),
	}
}

// verifyAllocationMachine runs the guard with the daemon's own credential:
// reader must read the API server live, not from a cache. It returns nil at
// once for a host that is not allocation-backed.
func verifyAllocationMachine(ctx context.Context, log *slog.Logger, reader client.Reader, cfg *provision.AgentConfig, opts allocationLaunchOptions) error {
	uid, backed := AllocationUID(cfg)
	if !backed {
		return nil
	}

	if uid == "" {
		return refuseAllocationLaunch(log, cfg, uid, allocationCredentialDaemonCertificate, errAllocationIdentityMalformed)
	}

	read := func(ctx context.Context) allocationRead {
		return machineAllocationRead(checkAllocationMachine(ctx, reader, cfg.MachineName, uid))
	}

	return checkAllocationLaunch(ctx, log, cfg, uid, opts, read)
}

// allocationRead is the outcome of one guard read.
type allocationRead struct {
	// credential is the credential the read used.
	credential string
	// err is nil when the read allows.
	err error
	// final reports that reading again cannot change a refusal.
	final bool
}

func finalAllocationRead(credential string, err error) allocationRead {
	return allocationRead{credential: credential, err: err, final: true}
}

func machineAllocationRead(err error) allocationRead {
	return allocationRead{
		credential: allocationCredentialDaemonCertificate,
		err:        err,
		final:      err != nil && machineRefusalIsFinal(err),
	}
}

// allocationAllowedCause says why a read with credential allowed.
func allocationAllowedCause(credential string) string {
	if credential == allocationCredentialBootstrapToken {
		return "the API server accepts the bootstrap token"
	}

	return "the Machine binds this allocation, is not fenced and is not being deleted"
}

// checkAllocationLaunch reads until one read allows, a read refuses for good,
// or the budget is spent. The budget covers every read of the call, whichever
// credential each read selected.
func checkAllocationLaunch(
	ctx context.Context,
	log *slog.Logger,
	cfg *provision.AgentConfig,
	uid string,
	opts allocationLaunchOptions,
	read func(context.Context) allocationRead,
) error {
	readCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	last := allocationRead{credential: allocationCredentialNone}

	for {
		if err := readCtx.Err(); err != nil {
			cause := fmt.Errorf("%w: %w", errAllocationLaunchUnconfirmed, err)
			if last.err != nil {
				cause = fmt.Errorf("%w: %w: last error: %w", errAllocationLaunchUnconfirmed, err, last.err)
			}

			return refuseAllocationLaunch(log, cfg, uid, last.credential, cause)
		}

		last = read(readCtx)
		if last.err == nil {
			log.Info("allocation launch allowed",
				"allocation_uid", uid,
				"machine", cfg.MachineName,
				"credential", last.credential,
				"cause", allocationAllowedCause(last.credential))

			return nil
		}

		if last.final {
			return refuseAllocationLaunch(log, cfg, uid, last.credential, last.err)
		}

		log.Warn("cannot confirm the allocation yet; reading again",
			"allocation_uid", uid,
			"machine", cfg.MachineName,
			"credential", last.credential,
			"error", last.err)

		timer := time.NewTimer(opts.interval)
		select {
		case <-readCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func refuseAllocationLaunch(log *slog.Logger, cfg *provision.AgentConfig, uid, credential string, cause error) error {
	log.Error("allocation launch refused",
		"allocation_uid", uid,
		"machine", cfg.MachineName,
		"credential", credential,
		"cause", cause)

	return fmt.Errorf("%w for allocation %q, machine %s: %w", ErrAllocationLaunchRefused, uid, cfg.MachineName, cause)
}

// checkAllocationMachine allows only a Machine that names this allocation's
// UID, is not fenced and is not being deleted. A Machine created under the
// same name for another allocation, or by the agent without allocationRef,
// authorizes nothing.
func checkAllocationMachine(ctx context.Context, reader client.Reader, machineName, uid string) error {
	var machine v1alpha3.Machine
	if err := reader.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
		return fmt.Errorf("get Machine %s: %w", machineName, err)
	}

	if machine.Spec.AllocationRef == nil {
		return fmt.Errorf("%w: it has no allocationRef", errAllocationMachineForeign)
	}

	if got := string(machine.Spec.AllocationRef.UID); got != uid {
		return fmt.Errorf("%w: its allocationRef names UID %q", errAllocationMachineForeign, got)
	}

	if machine.Spec.Fenced {
		return errAllocationMachineFenced
	}

	if machine.DeletionTimestamp != nil {
		return errAllocationMachineDeleting
	}

	return nil
}

// reviewBootstrapToken asks the API server who the bootstrap token belongs
// to. Any authenticated identity may ask, so only a revoked or rejected token
// fails.
func reviewBootstrapToken(ctx context.Context, c client.Client) error {
	if err := c.Create(ctx, &authenticationv1.SelfSubjectReview{}); err != nil {
		return fmt.Errorf("review bootstrap token: %w", err)
	}

	return nil
}

// machineRefusalIsFinal separates answers from failures to ask. The Machine's
// state and a rejected credential do not change by reading again; anything
// else may be an outage.
func machineRefusalIsFinal(err error) bool {
	switch {
	case errors.Is(err, errAllocationMachineFenced),
		errors.Is(err, errAllocationMachineDeleting),
		errors.Is(err, errAllocationMachineForeign):
		return true
	default:
		return apierrors.IsNotFound(err) || apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err)
	}
}

// bootstrapTokenRefusalIsFinal treats only a rejected token as an answer.
// NotFound means the API server does not serve SelfSubjectReview, which says
// nothing about the allocation.
func bootstrapTokenRefusalIsFinal(err error) bool {
	return apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err)
}
