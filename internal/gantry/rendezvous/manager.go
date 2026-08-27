// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package rendezvous implements bounded first-contact discovery through a
// fixed set of Kubernetes Lease objects.
package rendezvous

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

const (
	AnnotationP2PAddrs        = "gantry.io/p2p-addrs"
	AnnotationBootstrapSample = "gantry.io/bootstrap-sample"

	defaultSlotPrefix    = "gantry-rendezvous-"
	defaultMaxBundleSize = 16 << 10
	contactBundleVersion = 1
)

// Metrics records Lease rendezvous operations without coupling the package to
// a metrics implementation.
type Metrics struct {
	OnSlotGet   func(outcome string)
	OnSlotClaim func(outcome string)
	OnSlotRenew func(outcome string)
	OnContact   func(freshness string)
	OnSlotHeld  func(held bool)
}

// Options configures a Manager. All operation bounds are independent of the
// number of Gantry agents.
type Options struct {
	Leases coordinationclient.LeaseInterface
	PeerID peer.ID
	Addrs  func() []multiaddr.Multiaddr

	SlotCount             int
	ReadsPerRound         int
	ClaimAttemptsPerRound int
	ContactsPerSlot       int
	FullScanAfter         int
	LeaseDuration         time.Duration
	StaleContactGrace     time.Duration
	SlotPrefix            string
	MaxBundleSize         int
	Now                   func() time.Time
	Metrics               Metrics
	Logger                *slog.Logger
}

// Contact is an untrusted dial hint parsed from one Lease. The libp2p secure
// channel still verifies that the remote owns Info.ID.
type Contact struct {
	Info      peer.AddrInfo
	Slot      string
	Freshness string
	Sampled   bool
}

// Manager reads, claims, and renews at most one of a fixed number of Lease slots.
type Manager struct {
	leases coordinationclient.LeaseInterface
	peerID peer.ID
	addrs  func() []multiaddr.Multiaddr

	slotCount             int
	readsPerRound         int
	claimAttemptsPerRound int
	contactsPerSlot       int
	fullScanAfter         int
	leaseDuration         time.Duration
	staleContactGrace     time.Duration
	slotPrefix            string
	maxBundleSize         int
	now                   func() time.Time
	metrics               Metrics
	logger                *slog.Logger

	mu       sync.RWMutex
	heldSlot string
}

// New constructs a fixed-slot rendezvous manager.
func New(opts Options) (*Manager, error) {
	if opts.Leases == nil {
		return nil, errors.New("rendezvous: Lease client required")
	}

	if opts.PeerID == "" {
		return nil, errors.New("rendezvous: peer ID required")
	}

	if opts.Addrs == nil {
		return nil, errors.New("rendezvous: address source required")
	}

	if opts.SlotCount < 1 {
		return nil, errors.New("rendezvous: slot count must be positive")
	}

	if opts.ReadsPerRound < 1 || opts.ReadsPerRound > opts.SlotCount {
		return nil, fmt.Errorf("rendezvous: reads per round must be in [1,%d]", opts.SlotCount)
	}

	if opts.ClaimAttemptsPerRound < 1 || opts.ClaimAttemptsPerRound > opts.SlotCount {
		return nil, fmt.Errorf("rendezvous: claim attempts per round must be in [1,%d]", opts.SlotCount)
	}

	if opts.ContactsPerSlot < 1 {
		return nil, errors.New("rendezvous: contacts per slot must be positive")
	}

	if opts.LeaseDuration < time.Second {
		return nil, errors.New("rendezvous: lease duration must be at least one second")
	}

	if opts.FullScanAfter < 1 {
		opts.FullScanAfter = 3
	}

	if opts.SlotPrefix == "" {
		opts.SlotPrefix = defaultSlotPrefix
	}

	if opts.MaxBundleSize < 1 {
		opts.MaxBundleSize = defaultMaxBundleSize
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	return &Manager{
		leases:                opts.Leases,
		peerID:                opts.PeerID,
		addrs:                 opts.Addrs,
		slotCount:             opts.SlotCount,
		readsPerRound:         opts.ReadsPerRound,
		claimAttemptsPerRound: opts.ClaimAttemptsPerRound,
		contactsPerSlot:       opts.ContactsPerSlot,
		fullScanAfter:         opts.FullScanAfter,
		leaseDuration:         opts.LeaseDuration,
		staleContactGrace:     opts.StaleContactGrace,
		slotPrefix:            opts.SlotPrefix,
		maxBundleSize:         opts.MaxBundleSize,
		now:                   opts.Now,
		metrics:               opts.Metrics,
		logger:                opts.Logger.With(slog.String("subsystem", "rendezvous")),
	}, nil
}

// SlotNames returns the complete fixed key space without calling Kubernetes.
func (m *Manager) SlotNames() []string {
	names := make([]string, m.slotCount)

	width := max(4, len(strconv.Itoa(m.slotCount-1)))
	for i := range names {
		names[i] = m.slotPrefix + fmt.Sprintf("%0*d", width, i)
	}

	return names
}

// ReadContacts directly GETs a bounded deterministic sample. Every
// fullScanAfter round performs a complete fixed-slot scan so repeated retries
// eventually inspect every published contact without a Lease LIST.
func (m *Manager) ReadContacts(ctx context.Context, round uint64) ([]Contact, error) {
	names := m.readOrder(round)

	limit := m.readsPerRound
	if (round+1)%uint64(m.fullScanAfter) == 0 {
		limit = len(names)
	}

	contacts := make([]Contact, 0, limit)
	seen := make(map[peer.ID]int, limit)

	var errs []error

	for _, name := range names[:limit] {
		lease, err := m.leases.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			outcome := metricOutcome(err)
			m.slotGet(outcome)
			m.logSlotFailure("get", name, outcome, err)
			errs = append(errs, fmt.Errorf("get %s: %w", name, err))

			continue
		}

		m.slotGet("success")

		parsed, err := m.contactsFromLease(lease)
		if err != nil {
			m.logSlotFailure("parse", name, "invalid", err)
			errs = append(errs, fmt.Errorf("parse %s: %w", name, err))

			continue
		}

		if holder(lease) != "" && m.leaseFreshness(lease) == "expired" {
			m.contact("expired")
		}

		for _, contact := range parsed {
			if contact.Info.ID == m.peerID {
				continue
			}

			if index, ok := seen[contact.Info.ID]; ok {
				contacts[index].Info.Addrs = appendUniqueAddrs(contacts[index].Info.Addrs, contact.Info.Addrs...)
				continue
			}

			seen[contact.Info.ID] = len(contacts)
			contacts = append(contacts, contact)
			m.contact(contact.Freshness)
		}
	}

	return contacts, errors.Join(errs...)
}

// Claim examines a stable bounded slot prefix and updates the first empty or
// expired Lease using its observed resourceVersion.
func (m *Manager) Claim(ctx context.Context) (string, error) {
	names := m.claimOrder()

	var errs []error

	for _, name := range names[:m.claimAttemptsPerRound] {
		lease, err := m.leases.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			outcome := metricOutcome(err)
			m.slotGet(outcome)
			m.logSlotFailure("claim_get", name, outcome, err)
			errs = append(errs, fmt.Errorf("get claim candidate %s: %w", name, err))

			continue
		}

		m.slotGet("success")

		if holder(lease) == m.peerID.String() && m.leaseActive(lease) {
			m.setHeldSlot(name)
			m.slotClaim("resumed")

			return name, nil
		}

		if holder(lease) != "" && m.leaseActive(lease) {
			m.slotClaim("occupied")
			continue
		}

		if err := m.applyHolder(lease); err != nil {
			m.slotClaim("no_address")

			errs = append(errs, fmt.Errorf("claim %s: %w", name, err))

			continue
		}

		if _, err := m.leases.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
			outcome := metricOutcome(err)
			m.slotClaim(outcome)
			m.logSlotFailure("claim_update", name, outcome, err)

			if apierrors.IsConflict(err) {
				errs = append(errs, fmt.Errorf("claim %s: %w", name, err))
				continue
			}

			confirmed, confirmErr := m.leases.Get(ctx, name, metav1.GetOptions{})
			if confirmErr != nil {
				m.slotGet(metricOutcome(confirmErr))
			} else {
				m.slotGet("success")
			}

			if confirmErr == nil && holder(confirmed) == m.peerID.String() {
				m.setHeldSlot(name)
				m.slotClaim("confirmed")

				return name, nil
			}

			if confirmErr != nil {
				return "", errors.Join(
					fmt.Errorf("claim %s update: %w", name, err),
					fmt.Errorf("claim %s confirmation: %w", name, confirmErr),
				)
			}

			return "", fmt.Errorf("claim %s update was ambiguous and holder is %q: %w", name, holder(confirmed), err)
		}

		m.setHeldSlot(name)
		m.slotClaim("success")

		return name, nil
	}

	return "", errors.Join(errs...)
}

// Renew refreshes the current slot using a resource-version guarded Update.
func (m *Manager) Renew(ctx context.Context) error {
	name := m.HeldSlot()
	if name == "" {
		return errors.New("rendezvous: no held slot")
	}

	lease, err := m.leases.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		m.slotGet(metricOutcome(err))
		m.slotRenew(metricOutcome(err))

		return fmt.Errorf("rendezvous: get held slot %s: %w", name, err)
	}

	m.slotGet("success")

	if holder(lease) != m.peerID.String() {
		m.clearHeldSlot()
		m.slotRenew("lost")

		return fmt.Errorf("rendezvous: slot %s holder changed to %q", name, holder(lease))
	}

	if err := m.applyHolder(lease); err != nil {
		m.slotRenew("no_address")
		return fmt.Errorf("rendezvous: renew %s: %w", name, err)
	}

	if _, err := m.leases.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
		m.slotRenew(metricOutcome(err))
		return fmt.Errorf("rendezvous: renew %s: %w", name, err)
	}

	m.slotRenew("success")

	return nil
}

// HeldSlot returns the name of the slot currently owned by this manager.
func (m *Manager) HeldSlot() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.heldSlot
}

func (m *Manager) leaseFreshness(lease *coordinationv1.Lease) string {
	if m.leaseActive(lease) {
		return "fresh"
	}

	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return "expired"
	}

	expires := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	if m.staleContactGrace > 0 && !m.now().After(expires.Add(m.staleContactGrace)) {
		return "stale"
	}

	return "expired"
}

func (m *Manager) leaseActive(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return false
	}

	expires := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)

	return !m.now().After(expires)
}

func (m *Manager) applyHolder(lease *coordinationv1.Lease) error {
	addrs := m.advertisedAddrs()
	if len(addrs) == 0 {
		return errors.New("no dialable libp2p addresses")
	}

	encodedAddrs := strings.Join(addrs, ",")
	if len(encodedAddrs) > m.maxBundleSize {
		return fmt.Errorf("advertised address bundle exceeds %d bytes", m.maxBundleSize)
	}

	now := metav1.NewMicroTime(m.now().UTC())
	holderIdentity := m.peerID.String()
	duration := int32(m.leaseDuration / time.Second)
	lease.Spec.HolderIdentity = &holderIdentity
	lease.Spec.LeaseDurationSeconds = &duration

	lease.Spec.RenewTime = &now
	if lease.Annotations == nil {
		lease.Annotations = make(map[string]string)
	}

	lease.Annotations[AnnotationP2PAddrs] = encodedAddrs

	return nil
}

func (m *Manager) advertisedAddrs() []string {
	result := make([]string, 0)

	peerComponent := multiaddr.StringCast("/p2p/" + m.peerID.String())
	for _, addr := range m.addrs() {
		if addr != nil {
			result = append(result, addr.Encapsulate(peerComponent).String())
		}
	}

	return result
}

func (m *Manager) setHeldSlot(name string) {
	m.mu.Lock()
	m.heldSlot = name
	m.mu.Unlock()

	if m.metrics.OnSlotHeld != nil {
		m.metrics.OnSlotHeld(true)
	}
}

func (m *Manager) clearHeldSlot() {
	m.mu.Lock()
	m.heldSlot = ""
	m.mu.Unlock()

	if m.metrics.OnSlotHeld != nil {
		m.metrics.OnSlotHeld(false)
	}
}

func (m *Manager) slotGet(outcome string) {
	if m.metrics.OnSlotGet != nil {
		m.metrics.OnSlotGet(outcome)
	}
}

func (m *Manager) slotClaim(outcome string) {
	if m.metrics.OnSlotClaim != nil {
		m.metrics.OnSlotClaim(outcome)
	}
}

func (m *Manager) slotRenew(outcome string) {
	if m.metrics.OnSlotRenew != nil {
		m.metrics.OnSlotRenew(outcome)
	}
}

func (m *Manager) contact(freshness string) {
	if m.metrics.OnContact != nil {
		m.metrics.OnContact(freshness)
	}
}

func (m *Manager) logSlotFailure(operation, slot, outcome string, err error) {
	m.logger.Debug("rendezvous slot operation failed",
		slog.String("slot", slot),
		slog.String("operation", operation),
		slog.String("outcome", outcome),
		slog.Any("err", err),
	)
}
