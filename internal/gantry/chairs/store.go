// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chairs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

const (
	LabelChair             = "gantry.io/chair"
	AnnotationP2PAddrs     = "gantry.io/holder-p2p-addrs"
	AnnotationTransferAddr = "gantry.io/holder-transfer-addr"
	AnnotationEpoch        = "gantry.io/assignment-epoch"
	AnnotationNextPeerID   = "gantry.io/next-holder-peer-id"
	AnnotationNextP2PAddrs = "gantry.io/next-holder-p2p-addrs"
	AnnotationNextTransfer = "gantry.io/next-holder-transfer-addr"
)

var ErrNotClaimable = errors.New("chair is not claimable")

type LeaseClient interface {
	Create(ctx context.Context, lease *coordinationv1.Lease, opts metav1.CreateOptions) (*coordinationv1.Lease, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*coordinationv1.Lease, error)
	List(ctx context.Context, opts metav1.ListOptions) (*coordinationv1.LeaseList, error)
	Update(ctx context.Context, lease *coordinationv1.Lease, opts metav1.UpdateOptions) (*coordinationv1.Lease, error)
}

type Store struct {
	leases LeaseClient
}

func NewStore(leases LeaseClient) *Store {
	return &Store{leases: leases}
}

func (s *Store) Snapshot(ctx context.Context, epoch int64) (Snapshot, error) {
	list, err := s.leases.List(ctx, metav1.ListOptions{LabelSelector: LabelChair + "=true"})
	if err != nil {
		return Snapshot{}, fmt.Errorf("list chairs: %w", err)
	}

	snapshot := Snapshot{Epoch: epoch, FetchedAt: time.Now(), Chairs: make([]Chair, 0, Count)}

	for index := range list.Items {
		chair, err := DecodeLease(&list.Items[index])
		if err != nil {
			return Snapshot{}, err
		}

		snapshot.Chairs = append(snapshot.Chairs, chair)
	}

	return snapshot, nil
}

func (s *Store) Get(ctx context.Context, id ID) (Chair, error) {
	lease, err := s.leases.Get(ctx, id.Name(), metav1.GetOptions{})
	if err != nil {
		return Chair{}, fmt.Errorf("get chair %s: %w", id.Name(), err)
	}

	return DecodeLease(lease)
}

func (s *Store) Claim(ctx context.Context, id ID, holder Holder, epoch int64, duration time.Duration, unresponsive bool, now time.Time) (Chair, error) {
	lease, err := s.leases.Get(ctx, id.Name(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Name:   id.Name(),
			Labels: map[string]string{LabelChair: "true"},
		}}
		applyHolder(lease, holder, epoch, duration, now, true)

		created, createErr := s.leases.Create(ctx, lease, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(createErr) || apierrors.IsConflict(createErr) {
			return Chair{}, ErrNotClaimable
		}

		if createErr != nil {
			return Chair{}, fmt.Errorf("create chair %s: %w", id.Name(), createErr)
		}

		return DecodeLease(created)
	}

	if err != nil {
		return Chair{}, fmt.Errorf("get chair %s for claim: %w", id.Name(), err)
	}

	current, err := DecodeLease(lease)
	if err != nil {
		return Chair{}, err
	}

	if current.Occupied() && current.Holder.PeerID != holder.PeerID && (!current.Expired(now) || !unresponsive) {
		return current, ErrNotClaimable
	}

	applyHolder(lease, holder, epoch, duration, now, current.Holder.PeerID != holder.PeerID)

	updated, err := s.leases.Update(ctx, lease, metav1.UpdateOptions{})
	if err != nil {
		if apierrors.IsConflict(err) {
			return Chair{}, ErrNotClaimable
		}

		return Chair{}, fmt.Errorf("claim chair %s: %w", id.Name(), err)
	}

	return DecodeLease(updated)
}

func (s *Store) Renew(ctx context.Context, id ID, holder Holder, epoch int64, duration time.Duration, now time.Time) (Chair, error) {
	lease, err := s.leases.Get(ctx, id.Name(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Chair{}, ErrNotClaimable
	}

	if err != nil {
		return Chair{}, fmt.Errorf("get chair %s for renewal: %w", id.Name(), err)
	}

	current, err := DecodeLease(lease)
	if err != nil {
		return Chair{}, err
	}

	if current.Holder.PeerID != holder.PeerID {
		return current, ErrNotClaimable
	}

	applyHolder(lease, holder, epoch, duration, now, false)

	updated, err := s.leases.Update(ctx, lease, metav1.UpdateOptions{})
	if err != nil {
		return Chair{}, fmt.Errorf("renew chair %s: %w", id.Name(), err)
	}

	return DecodeLease(updated)
}

func (s *Store) SetNextHolder(ctx context.Context, id ID, holder ifaces.NodeID, generation int64, next Holder) (Chair, error) {
	lease, err := s.leases.Get(ctx, id.Name(), metav1.GetOptions{})
	if err != nil {
		return Chair{}, fmt.Errorf("get chair %s for successor: %w", id.Name(), err)
	}

	current, err := DecodeLease(lease)
	if err != nil {
		return Chair{}, err
	}

	if current.Holder.PeerID != holder || current.Generation != generation {
		return current, ErrNotClaimable
	}

	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}

	lease.Annotations[AnnotationNextPeerID] = string(next.PeerID)
	lease.Annotations[AnnotationNextP2PAddrs] = encodeAddrs(next.P2PAddrs)
	lease.Annotations[AnnotationNextTransfer] = next.TransferAddr

	updated, err := s.leases.Update(ctx, lease, metav1.UpdateOptions{})
	if err != nil {
		return Chair{}, fmt.Errorf("set chair %s successor: %w", id.Name(), err)
	}

	return DecodeLease(updated)
}

func (s *Store) Rotate(ctx context.Context, id ID, holder ifaces.NodeID, generation, epoch int64, duration time.Duration, now time.Time) (Chair, error) {
	lease, err := s.leases.Get(ctx, id.Name(), metav1.GetOptions{})
	if err != nil {
		return Chair{}, fmt.Errorf("get chair %s for rotation: %w", id.Name(), err)
	}

	current, err := DecodeLease(lease)
	if err != nil {
		return Chair{}, err
	}

	if current.Holder.PeerID != holder || current.Generation != generation || HolderEmpty(current.NextHolder) {
		return current, ErrNotClaimable
	}

	applyHolder(lease, current.NextHolder, epoch, duration, now, true)

	updated, err := s.leases.Update(ctx, lease, metav1.UpdateOptions{})
	if err != nil {
		return Chair{}, fmt.Errorf("rotate chair %s: %w", id.Name(), err)
	}

	return DecodeLease(updated)
}

func (s *Store) Vacate(ctx context.Context, id ID, holder ifaces.NodeID) error {
	lease, err := s.leases.Get(ctx, id.Name(), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get chair %s for vacate: %w", id.Name(), err)
	}

	current, err := DecodeLease(lease)
	if err != nil {
		return err
	}

	if current.Holder.PeerID != holder {
		return ErrNotClaimable
	}

	lease.Spec.HolderIdentity = nil
	lease.Spec.AcquireTime = nil
	lease.Spec.RenewTime = nil
	transitions := int32(current.Generation + 1)

	lease.Spec.LeaseTransitions = &transitions
	for _, key := range []string{
		AnnotationP2PAddrs,
		AnnotationTransferAddr,
		AnnotationEpoch,
		AnnotationNextPeerID,
		AnnotationNextP2PAddrs,
		AnnotationNextTransfer,
	} {
		delete(lease.Annotations, key)
	}

	if _, err := s.leases.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("vacate chair %s: %w", id.Name(), err)
	}

	return nil
}

func DecodeLease(lease *coordinationv1.Lease) (Chair, error) {
	id, err := ParseName(lease.Name)
	if err != nil {
		return Chair{}, err
	}

	chair := Chair{ID: id}
	if lease.Spec.HolderIdentity != nil {
		chair.Holder.PeerID = ifaces.NodeID(*lease.Spec.HolderIdentity)
	}

	if lease.Spec.RenewTime != nil {
		chair.RenewTime = lease.Spec.RenewTime.Time
	}

	if lease.Spec.LeaseDurationSeconds != nil {
		chair.LeaseDuration = time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	}

	if lease.Spec.LeaseTransitions != nil {
		chair.Generation = int64(*lease.Spec.LeaseTransitions)
	}

	annotations := lease.Annotations
	if annotations == nil {
		return chair, nil
	}

	chair.Holder.P2PAddrs = decodeAddrs(annotations[AnnotationP2PAddrs])
	chair.Holder.TransferAddr = annotations[AnnotationTransferAddr]
	chair.NextHolder.PeerID = ifaces.NodeID(annotations[AnnotationNextPeerID])
	chair.NextHolder.P2PAddrs = decodeAddrs(annotations[AnnotationNextP2PAddrs])
	chair.NextHolder.TransferAddr = annotations[AnnotationNextTransfer]

	if raw := annotations[AnnotationEpoch]; raw != "" {
		epoch, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Chair{}, fmt.Errorf("chair %s assignment epoch %q: %w", lease.Name, raw, err)
		}

		chair.AssignmentEpoch = epoch
	}

	return chair, nil
}

func applyHolder(lease *coordinationv1.Lease, holder Holder, epoch int64, duration time.Duration, now time.Time, transition bool) {
	peerID := string(holder.PeerID)
	seconds := int32(duration / time.Second)
	renewTime := metav1.NewMicroTime(now)
	lease.Spec.HolderIdentity = &peerID
	lease.Spec.LeaseDurationSeconds = &seconds

	lease.Spec.RenewTime = &renewTime
	if lease.Spec.AcquireTime == nil || transition {
		acquireTime := metav1.NewMicroTime(now)
		lease.Spec.AcquireTime = &acquireTime
	}

	transitions := int32(1)
	if lease.Spec.LeaseTransitions != nil {
		transitions = *lease.Spec.LeaseTransitions
		if transition {
			transitions++
		}
	}

	lease.Spec.LeaseTransitions = &transitions
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}

	lease.Annotations[AnnotationP2PAddrs] = encodeAddrs(holder.P2PAddrs)
	lease.Annotations[AnnotationTransferAddr] = holder.TransferAddr

	lease.Annotations[AnnotationEpoch] = strconv.FormatInt(epoch, 10)
	if transition {
		delete(lease.Annotations, AnnotationNextPeerID)
		delete(lease.Annotations, AnnotationNextP2PAddrs)
		delete(lease.Annotations, AnnotationNextTransfer)
	}
}

func encodeAddrs(addrs []string) string {
	raw, err := json.Marshal(addrs)
	if err != nil {
		return "[]"
	}

	return string(raw)
}

func decodeAddrs(raw string) []string {
	if raw == "" {
		return nil
	}

	var addrs []string
	if err := json.Unmarshal([]byte(raw), &addrs); err != nil {
		return nil
	}

	return addrs
}
