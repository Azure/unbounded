// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const holderNonce = "racer.unbounded-cloud.io/holder-nonce"

// TestEnvtestRacerLeaseReacquire checks that the same Pod can reacquire its
// own seat and primary Lease with a new term, as the runtime does after a
// container restart or after its Lease went stale, while renewals keep the
// term and nonce.
func TestEnvtestRacerLeaseReacquire(t *testing.T) {
	f := startAdmissionFixture(t)
	a, b := f.a, f.b

	for _, name := range []string{"racer-seat-1", "racer-primary"} {
		t.Run(name, func(t *testing.T) {
			lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.env.Namespace}}

			// claim mirrors election::claim: a new acquireTime and the next term.
			claim := func(p racerPod, nonce string, at time.Time, bump int32) func(*coordinationv1.Lease) {
				return func(l *coordinationv1.Lease) {
					l.Spec.HolderIdentity = ptr.To(p.pod.Name)
					l.Spec.LeaseDurationSeconds = ptr.To(int32(15))
					l.Spec.LeaseTransitions = ptr.To(ptr.Deref(l.Spec.LeaseTransitions, 0) + bump)
					l.Spec.AcquireTime = &metav1.MicroTime{Time: at}
					l.Spec.RenewTime = &metav1.MicroTime{Time: at}

					if l.Annotations == nil {
						l.Annotations = map[string]string{}
					}

					l.Annotations["racer.unbounded-cloud.io/holder-uid"] = string(p.pod.UID)
					l.Annotations["racer.unbounded-cloud.io/holder-node"] = p.pod.Spec.NodeName
					l.Annotations["racer.unbounded-cloud.io/endpoint"] = "https://10.0.0.1:8443"
					l.Annotations[holderNonce] = nonce
				}
			}
			renew := func(l *coordinationv1.Lease) { l.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()} }
			t0 := time.Now().Add(-time.Minute)

			require.NoError(t, update(t, f, a.client, lease, claim(a, "nonce-1", t0, 1)))
			require.NoError(t, update(t, f, a.client, lease, renew))

			// Renewal keeps the term and the nonce.
			denied(t, update(t, f, a.client, lease, func(l *coordinationv1.Lease) {
				renew(l)
				l.Spec.LeaseTransitions = ptr.To(*l.Spec.LeaseTransitions + 1)
			}))
			denied(t, update(t, f, a.client, lease, func(l *coordinationv1.Lease) {
				renew(l)
				l.Annotations[holderNonce] = "nonce-2"
			}))
			denied(t, update(t, f, a.client, lease, func(l *coordinationv1.Lease) {
				renew(l)
				l.Spec.AcquireTime = nil
			}))

			// The same Pod restarted with a new process nonce reacquires.
			denied(t, update(t, f, a.client, lease, claim(a, "nonce-2", t0.Add(time.Second), 0)))
			denied(t, update(t, f, a.client, lease, claim(a, "nonce-2", t0.Add(time.Second), -1)))
			require.NoError(t, update(t, f, a.client, lease, claim(a, "nonce-2", t0.Add(time.Second), 1)))
			require.NoError(t, update(t, f, a.client, lease, renew))

			// The same process reacquires its stale Lease with the same nonce.
			require.NoError(t, update(t, f, a.client, lease, claim(a, "nonce-2", t0.Add(2*time.Second), 1)))
			require.NoError(t, update(t, f, a.client, lease, renew))

			// Other Pods still need their own identity and a new term.
			denied(t, update(t, f, b.client, lease, renew))
			denied(t, update(t, f, b.client, lease, claim(a, "nonce-3", t0.Add(3*time.Second), 1)))
			denied(t, update(t, f, b.client, lease, claim(b, "nonce-3", t0.Add(3*time.Second), 0)))
			require.NoError(t, update(t, f, b.client, lease, claim(b, "nonce-3", t0.Add(3*time.Second), 1)))
			denied(t, update(t, f, a.client, lease, renew))
			denied(t, update(t, f, a.client, lease, func(l *coordinationv1.Lease) { l.Spec.HolderIdentity = nil }))

			// Release stays with the holder.
			require.NoError(t, update(t, f, b.client, lease, func(l *coordinationv1.Lease) {
				l.Spec.HolderIdentity = nil
				l.Spec.AcquireTime = nil
				l.Spec.RenewTime = nil
			}))
		})
	}
}
