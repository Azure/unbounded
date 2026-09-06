// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chairs

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

type ManagerOptions struct {
	Store               *Store
	Cache               *Cache
	Self                Holder
	Rotation            ifaces.ChairRotationCoordinator
	Candidates          func() []Holder
	Connect             func(context.Context, []string) int
	Now                 func() time.Time
	Logger              *slog.Logger
	LeaseDuration       time.Duration
	RenewPeriod         time.Duration
	RotationPeriod      time.Duration
	RotationLead        time.Duration
	StartupJitter       time.Duration
	ClaimRoundPeriod    time.Duration
	ClaimJitter         time.Duration
	ClaimInitialDivisor uint64
	APITimeout          time.Duration
	ClusterSizeEstimate int
}

type reservation struct {
	assignment ifaces.ChairAssignment
}

type Manager struct {
	opts ManagerOptions

	mu                sync.Mutex
	held              *Chair
	reserved          *reservation
	claimRound        uint64
	knownFull         bool
	initialized       bool
	claiming          bool
	participating     bool
	selectionReady    bool
	electionEpoch     int64
	bootstrapReady    bool
	observationRounds uint64
	duplicateChairs   []ID
}

const claimStageRounds uint64 = 8

func NewManager(opts ManagerOptions) *Manager {
	if opts.Store == nil {
		panic("chairs.NewManager: Store is required")
	}

	if HolderEmpty(opts.Self) {
		panic("chairs.NewManager: Self is required")
	}

	if opts.Cache == nil {
		opts.Cache = NewCache(opts.Store)
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = time.Minute
	}

	if opts.RenewPeriod <= 0 {
		opts.RenewPeriod = 20 * time.Second
	}

	if opts.RotationPeriod <= 0 {
		opts.RotationPeriod = 6 * time.Hour
	}

	if opts.RotationLead <= 0 {
		opts.RotationLead = 5 * time.Minute
	}

	if opts.StartupJitter <= 0 {
		opts.StartupJitter = 30 * time.Second
	}

	if opts.ClaimRoundPeriod <= 0 {
		opts.ClaimRoundPeriod = time.Second
	}

	if opts.ClaimJitter <= 0 {
		opts.ClaimJitter = 750 * time.Millisecond
	}

	if opts.ClaimInitialDivisor == 0 {
		opts.ClaimInitialDivisor = 2048
	}

	if opts.APITimeout <= 0 {
		opts.APITimeout = 5 * time.Second
	}

	observationRounds := uint64(1)
	if opts.ClusterSizeEstimate > 500 {
		observationRounds = uint64((opts.ClusterSizeEstimate + 499) / 500)
	}

	opts.Logger = opts.Logger.With(slog.String("subsystem", "chairs"))

	return &Manager{
		opts:              opts,
		electionEpoch:     CurrentEpoch(opts.Now(), opts.RotationPeriod),
		observationRounds: observationRounds,
	}
}

func (m *Manager) CurrentEpoch() int64 {
	return CurrentEpoch(m.opts.Now(), m.opts.RotationPeriod)
}

func (m *Manager) Held() (Chair, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.held == nil {
		return Chair{}, false
	}

	return *m.held, true
}

func (m *Manager) Ready() bool {
	snapshot := m.opts.Cache.Peek()
	epoch := m.CurrentEpoch()

	return (snapshot.Epoch == epoch || snapshot.Epoch == epoch-1) &&
		snapshot.SelectableCount() >= SeedCount
}

func (m *Manager) Run(ctx context.Context) {
	if !waitDeterministic(ctx, deterministicDelay(m.opts.StartupJitter, string(m.opts.Self.PeerID), "startup")) {
		return
	}

	if err := m.Initialize(ctx); err != nil {
		m.opts.Logger.Warn("chair startup snapshot failed", slog.Any("err", err))
	} else {
		// Recovering holders publish a changed Pod address immediately instead
		// of waiting one renewal period. The subsequent full-snapshot refresh
		// lets a simultaneous DaemonSet restart rebuild DHT connectivity.
		m.maintain(ctx)
	}

	renewTicker := time.NewTicker(m.opts.RenewPeriod)
	defer renewTicker.Stop()

	claimTicker := time.NewTicker(m.opts.ClaimRoundPeriod)
	defer claimTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-renewTicker.C:
			m.maintain(ctx)
		case <-claimTicker.C:
			m.attemptClaim(ctx)
		}
	}
}

func (m *Manager) Initialize(ctx context.Context) error {
	return m.recover(ctx)
}

func (m *Manager) ValidateChair(_ context.Context, assignment ifaces.ChairAssignment) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	epoch := m.CurrentEpoch()

	return m.held != nil &&
		uint32(m.held.ID) == assignment.ChairID &&
		m.held.Generation == assignment.Generation &&
		m.held.AssignmentEpoch == assignment.AssignmentEpoch &&
		(assignment.AssignmentEpoch == epoch || assignment.AssignmentEpoch == epoch-1)
}

func (m *Manager) AcceptChair(ctx context.Context, proposer ifaces.NodeID, assignment ifaces.ChairAssignment) (ifaces.PeerEndpoint, bool) {
	if assignment.ChairID >= Count || assignment.AssignmentEpoch != m.CurrentEpoch()+1 {
		return ifaces.PeerEndpoint{}, false
	}

	apiCtx, cancel := m.apiContext(ctx)
	chair, err := m.opts.Store.Get(apiCtx, ID(assignment.ChairID))

	cancel()

	if err != nil || chair.Holder.PeerID != proposer || chair.Generation+1 != assignment.Generation || chair.AssignmentEpoch != m.CurrentEpoch() || !HolderEmpty(chair.NextHolder) {
		return ifaces.PeerEndpoint{}, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized || m.held != nil || m.claiming {
		return ifaces.PeerEndpoint{}, false
	}

	if m.reserved != nil && m.reserved.assignment != assignment {
		return ifaces.PeerEndpoint{}, false
	}

	m.reserved = &reservation{assignment: assignment}

	return cloneHolder(m.opts.Self), true
}

func (m *Manager) ClaimFailed(ctx context.Context, chair Chair) (Chair, bool, error) {
	if !chair.Expired(m.opts.Now()) {
		return Chair{}, false, nil
	}

	m.mu.Lock()

	alreadyHolding := !m.initialized || m.held != nil || m.reserved != nil || m.claiming
	if !alreadyHolding {
		m.claiming = true
	}
	m.mu.Unlock()

	if alreadyHolding {
		return Chair{}, false, nil
	}

	defer m.finishClaim()

	delay := deterministicDelay(m.opts.ClaimJitter, string(m.opts.Self.PeerID), chair.ID.Name(), strconv.FormatInt(chair.Generation, 10))
	if !waitDeterministic(ctx, delay) {
		return Chair{}, false, ctx.Err()
	}

	apiCtx, cancel := m.apiContext(ctx)
	claimed, err := m.opts.Store.Claim(apiCtx, chair.ID, m.opts.Self, m.CurrentEpoch(), m.opts.LeaseDuration, true, m.opts.Now())

	cancel()

	if err != nil {
		if errors.Is(err, ErrNotClaimable) {
			return Chair{}, false, nil
		}

		return Chair{}, false, err
	}

	m.setHeld(claimed)
	m.opts.Cache.UpdateChair(claimed)

	return claimed, true, nil
}

func (m *Manager) recover(ctx context.Context) error {
	apiCtx, cancel := m.apiContext(ctx)
	snapshot, err := m.opts.Store.Snapshot(apiCtx, m.CurrentEpoch())

	cancel()

	if err != nil {
		return err
	}

	m.observe(ctx, snapshot)

	held := make([]Chair, 0, 1)
	successors := make([]Chair, 0, 1)

	for _, chair := range snapshot.Chairs {
		if chair.Holder.PeerID == m.opts.Self.PeerID {
			held = append(held, chair)
		}

		if chair.NextHolder.PeerID == m.opts.Self.PeerID {
			successors = append(successors, chair)
		}
	}

	sort.Slice(held, func(left, right int) bool { return held[left].ID < held[right].ID })

	if len(held) > 0 {
		m.setHeld(held[0])
	} else if len(successors) > 0 {
		sort.Slice(successors, func(left, right int) bool { return successors[left].ID < successors[right].ID })
		m.mu.Lock()
		m.reserved = &reservation{assignment: ifaces.ChairAssignment{
			ChairID:         uint32(successors[0].ID),
			Generation:      successors[0].Generation + 1,
			AssignmentEpoch: successors[0].AssignmentEpoch + 1,
		}}
		m.mu.Unlock()
	}

	for index := 1; index < len(held); index++ {
		duplicate := held[index]
		apiCtx, cancel := m.apiContext(ctx)
		err := m.opts.Store.Vacate(apiCtx, duplicate.ID, m.opts.Self.PeerID)

		cancel()

		if err != nil {
			m.opts.Logger.Warn("duplicate chair vacate failed", slog.String("chair", duplicate.ID.Name()), slog.Any("err", err))

			m.mu.Lock()
			m.duplicateChairs = append(m.duplicateChairs, duplicate.ID)
			m.mu.Unlock()
		}
	}

	return nil
}

func (m *Manager) attemptClaim(ctx context.Context) {
	m.mu.Lock()

	epoch := m.CurrentEpoch()
	if m.electionEpoch != epoch {
		m.electionEpoch = epoch
		m.claimRound = 0
		m.knownFull = false
		m.participating = false
		m.selectionReady = false
	}

	if m.held != nil || m.reserved != nil || m.knownFull || m.claiming || (m.selectionReady && m.bootstrapReady && !m.participating) {
		m.mu.Unlock()
		return
	}

	m.claiming = true
	round := m.claimRound
	m.claimRound++

	m.mu.Unlock()
	defer m.finishClaim()

	stage := round / claimStageRounds

	eligible := claimEligible(m.opts.Self.PeerID, epoch, stage, m.opts.ClaimInitialDivisor)
	if eligible {
		m.mu.Lock()
		m.participating = true
		m.mu.Unlock()
	}

	// Nonparticipants refresh on a stable slot in a cluster-sized window. At
	// the shipped 100,000-node estimate this targets about 500 follow-up Lease
	// lists per second, while eligibility itself continues widening every stage.
	observerSlot := stableHash(string(m.opts.Self.PeerID), strconv.FormatInt(epoch, 10), "observe") % m.observationRounds
	if !eligible && round%m.observationRounds != observerSlot {
		return
	}

	apiCtx, cancel := m.apiContext(ctx)
	snapshot, err := m.opts.Store.Snapshot(apiCtx, epoch)

	cancel()

	if err != nil {
		return
	}

	m.observe(ctx, snapshot)

	if held, ok := snapshot.HolderChair(m.opts.Self.PeerID); ok {
		m.setHeld(held)
		return
	}

	if !eligible {
		return
	}

	empty := make([]ID, 0, Count-snapshot.OccupiedCount())

	occupied := make(map[ID]struct{}, len(snapshot.Chairs))
	for _, chair := range snapshot.Chairs {
		if chair.Occupied() {
			occupied[chair.ID] = struct{}{}
		}
	}

	for index := range Count {
		id := ID(index)
		if _, ok := occupied[id]; !ok {
			empty = append(empty, id)
		}
	}

	if len(empty) == 0 {
		m.mu.Lock()
		m.knownFull = true
		m.mu.Unlock()

		return
	}

	choice := stableHash(string(m.opts.Self.PeerID), strconv.FormatInt(epoch, 10), strconv.FormatUint(round, 10)) % uint64(len(empty))
	id := empty[choice]

	delay := deterministicDelay(m.opts.ClaimJitter, string(m.opts.Self.PeerID), id.Name(), strconv.FormatUint(round, 10))
	if !waitDeterministic(ctx, delay) {
		return
	}

	apiCtx, cancel = m.apiContext(ctx)
	claimed, err := m.opts.Store.Claim(apiCtx, id, m.opts.Self, epoch, m.opts.LeaseDuration, false, m.opts.Now())

	cancel()

	if err != nil {
		return
	}

	m.setHeld(claimed)
	m.opts.Cache.UpdateChair(claimed)
}

func (m *Manager) maintain(ctx context.Context) {
	m.adoptReservation(ctx)
	m.retryDuplicateVacates(ctx)

	m.mu.Lock()
	if m.held == nil {
		m.mu.Unlock()
		return
	}

	held := *m.held
	m.mu.Unlock()

	epoch := m.CurrentEpoch()

	var (
		updated Chair
		err     error
	)

	if epoch > held.AssignmentEpoch && !HolderEmpty(held.NextHolder) {
		apiCtx, cancel := m.apiContext(ctx)
		updated, err = m.opts.Store.Rotate(apiCtx, held.ID, m.opts.Self.PeerID, held.Generation, epoch, m.opts.LeaseDuration, m.opts.Now())

		cancel()

		if err == nil {
			m.mu.Lock()
			m.held = nil
			m.mu.Unlock()
			m.opts.Cache.Invalidate()

			return
		}
	} else {
		apiCtx, cancel := m.apiContext(ctx)
		updated, err = m.opts.Store.Renew(apiCtx, held.ID, m.opts.Self, epoch, m.opts.LeaseDuration, m.opts.Now())

		cancel()
	}

	if err != nil {
		if errors.Is(err, ErrNotClaimable) {
			m.mu.Lock()
			m.held = nil
			m.mu.Unlock()
		}

		return
	}

	m.setHeld(updated)
	m.opts.Cache.UpdateChair(updated)

	cached := m.opts.Cache.Peek()
	m.mu.Lock()
	bootstrapReady := m.bootstrapReady
	m.mu.Unlock()

	if cached.Epoch != epoch || cached.SelectableCount() < SeedCount || !bootstrapReady {
		apiCtx, cancel := m.apiContext(ctx)
		snapshot, snapshotErr := m.opts.Store.Snapshot(apiCtx, m.CurrentEpoch())

		cancel()

		if snapshotErr == nil {
			m.observe(ctx, snapshot)
		}
	}

	m.prepareRotation(ctx, updated)
}

func (m *Manager) retryDuplicateVacates(ctx context.Context) {
	m.mu.Lock()
	pending := append([]ID(nil), m.duplicateChairs...)
	m.mu.Unlock()

	if len(pending) == 0 {
		return
	}

	remaining := pending[:0]
	for _, id := range pending {
		apiCtx, cancel := m.apiContext(ctx)
		err := m.opts.Store.Vacate(apiCtx, id, m.opts.Self.PeerID)

		cancel()

		if err != nil && !errors.Is(err, ErrNotClaimable) {
			remaining = append(remaining, id)
		}
	}

	m.mu.Lock()
	m.duplicateChairs = append(m.duplicateChairs[:0], remaining...)
	m.mu.Unlock()
}

func (m *Manager) prepareRotation(ctx context.Context, held Chair) {
	if m.opts.Rotation == nil || m.opts.Candidates == nil || !HolderEmpty(held.NextHolder) {
		return
	}

	nextEpoch := m.CurrentEpoch() + 1
	boundary := time.Unix(0, nextEpoch*m.opts.RotationPeriod.Nanoseconds())

	untilBoundary := boundary.Sub(m.opts.Now())
	if untilBoundary > m.opts.RotationLead || untilBoundary <= 0 {
		return
	}

	apiCtx, cancel := m.apiContext(ctx)
	snapshot, err := m.opts.Store.Snapshot(apiCtx, m.CurrentEpoch())

	cancel()

	if err != nil {
		return
	}

	m.observe(ctx, snapshot)

	chairHolders := make(map[ifaces.NodeID]struct{}, len(snapshot.Chairs))
	for _, chair := range snapshot.Chairs {
		if chair.Occupied() {
			chairHolders[chair.Holder.PeerID] = struct{}{}
		}
	}

	candidates := m.opts.Candidates()

	filtered := candidates[:0]
	for _, candidate := range candidates {
		if candidate.PeerID == "" || candidate.PeerID == m.opts.Self.PeerID {
			continue
		}

		if _, isHolder := chairHolders[candidate.PeerID]; isHolder {
			continue
		}

		filtered = append(filtered, candidate)
	}

	sort.Slice(filtered, func(left, right int) bool {
		leftScore := stableHash(held.ID.Name(), strconv.FormatInt(nextEpoch, 10), string(filtered[left].PeerID))
		rightScore := stableHash(held.ID.Name(), strconv.FormatInt(nextEpoch, 10), string(filtered[right].PeerID))

		return leftScore > rightScore
	})

	assignment := ifaces.ChairAssignment{
		ChairID:         uint32(held.ID),
		Generation:      held.Generation + 1,
		AssignmentEpoch: nextEpoch,
	}

	for _, candidate := range filtered {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		next, accepted, offerErr := m.opts.Rotation.OfferChair(callCtx, candidate.PeerID, assignment)

		cancel()

		if offerErr != nil || !accepted || next.PeerID != candidate.PeerID {
			continue
		}

		apiCtx, cancel := m.apiContext(ctx)
		updated, err := m.opts.Store.SetNextHolder(apiCtx, held.ID, m.opts.Self.PeerID, held.Generation, next)

		cancel()

		if err == nil {
			m.setHeld(updated)
			m.opts.Cache.UpdateChair(updated)
		}

		return
	}
}

func (m *Manager) adoptReservation(ctx context.Context) {
	m.mu.Lock()
	reserved := m.reserved
	m.mu.Unlock()

	if reserved == nil || m.CurrentEpoch() < reserved.assignment.AssignmentEpoch {
		return
	}

	apiCtx, cancel := m.apiContext(ctx)
	chair, err := m.opts.Store.Get(apiCtx, ID(reserved.assignment.ChairID))

	cancel()

	if err != nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if chair.Holder.PeerID == m.opts.Self.PeerID &&
		chair.Generation == reserved.assignment.Generation &&
		chair.AssignmentEpoch == reserved.assignment.AssignmentEpoch {
		chairCopy := chair
		m.held = &chairCopy
		m.reserved = nil

		return
	}

	// Keep waiting while the Lease still records this node as the accepted
	// successor. Holder and successor heartbeats are independent, so the
	// successor may observe the epoch boundary before the holder performs the
	// rotation update.
	if chair.NextHolder.PeerID == m.opts.Self.PeerID &&
		chair.Generation+1 == reserved.assignment.Generation {
		return
	}

	if m.CurrentEpoch() >= reserved.assignment.AssignmentEpoch ||
		chair.NextHolder.PeerID != m.opts.Self.PeerID {
		m.reserved = nil
	}
}

func (m *Manager) observe(ctx context.Context, snapshot Snapshot) {
	m.opts.Cache.Observe(snapshot)

	connected := 0

	if m.opts.Connect != nil {
		addresses := make([]string, 0, len(snapshot.Chairs))
		for _, chair := range snapshot.Chairs {
			addresses = append(addresses, chair.Holder.P2PAddrs...)
		}

		connected = m.opts.Connect(ctx, addresses)
	}

	m.mu.Lock()
	m.knownFull = snapshot.OccupiedCount() == Count
	m.initialized = true

	m.selectionReady = snapshot.SelectableCount() >= SeedCount
	if m.opts.Connect == nil || connected > 0 {
		m.bootstrapReady = true
	}
	m.mu.Unlock()
}

func (m *Manager) setHeld(chair Chair) {
	m.mu.Lock()
	chairCopy := chair
	m.held = &chairCopy
	m.reserved = nil
	m.knownFull = false
	m.initialized = true
	m.mu.Unlock()
}

func (m *Manager) finishClaim() {
	m.mu.Lock()
	m.claiming = false
	m.mu.Unlock()
}

func (m *Manager) apiContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, m.opts.APITimeout)
}

func claimEligible(peerID ifaces.NodeID, epoch int64, round, initialDivisor uint64) bool {
	divisor := initialDivisor
	for index := uint64(0); index < round && divisor > 1; index++ {
		divisor = (divisor + 1) / 2
	}

	return stableHash(string(peerID), strconv.FormatInt(epoch, 10))%divisor == 0
}

func deterministicDelay(max time.Duration, parts ...string) time.Duration {
	if max <= 0 {
		return 0
	}

	return time.Duration(stableHash(parts...) % uint64(max))
}

func stableHash(parts ...string) uint64 {
	hasher := sha256.New()
	for _, part := range parts {
		_, _ = hasher.Write([]byte(part))
		_, _ = hasher.Write([]byte{0})
	}

	return binary.BigEndian.Uint64(hasher.Sum(nil)[:8])
}

func waitDeterministic(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func cloneHolder(holder Holder) Holder {
	holder.P2PAddrs = append([]string(nil), holder.P2PAddrs...)
	return holder
}
