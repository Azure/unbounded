//! Receiver-bound, node-wide atomic replay admission after signature verification.
//!
//! Capacity includes every live nonce across workers and signers. Saturation never
//! evicts a live nonce. A receiver challenge is generated once per shared state;
//! discarding/restarting that state invalidates all previously signed envelopes.
use crate::{
    error::{Error, Result},
    model::identity::NodeId,
};
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeSet, HashSet},
    sync::{Arc, Mutex},
    time::{Duration, SystemTime, UNIX_EPOCH},
};

pub const MAX_AGE: Duration = Duration::from_secs(60);
pub const MAX_FUTURE_SKEW: Duration = Duration::from_secs(5);
const MAX_EPOCH_BYTES: usize = 64 * 1024;

/// Shared by all reactors. Construct once per process, never once per connection.
#[derive(Default)]
pub struct ReplayState {
    inner: Mutex<Option<Table>>,
}

// Preserve the scaffold's `Arc::new(ReplayState)` construction while also offering
// `ReplayState::default()`. This value has independent state on each evaluation.
#[allow(non_upper_case_globals)]
pub const ReplayState: ReplayState = ReplayState {
    inner: Mutex::new(None),
};

struct Table {
    challenge: [u8; 32],
    capacity: usize,
    high_water: SystemTime,
    seen: HashSet<ReplayKey>,
    expiry: BTreeSet<(SystemTime, ReplayKey)>,
}

#[derive(Clone, Copy, Eq, PartialEq, Hash, Ord, PartialOrd)]
struct ReplayKey {
    signer_epoch: [u8; 32],
    nonce: [u8; 24],
}

pub struct ReplayWindow {
    state: Arc<ReplayState>,
    capacity: usize,
}

#[derive(Clone, Copy)]
pub struct ReplayNonce(pub [u8; 24]);

pub struct Freshness {
    pub nonce: ReplayNonce,
    pub timestamp: SystemTime,
    pub session_challenge: [u8; 32],
}

impl Freshness {
    /// The challenge must have been obtained through an authenticated peer session.
    /// Nonces are generated independently of page/credential nonces and attempt IDs.
    pub fn generate(session_challenge: [u8; 32]) -> Result<Self> {
        let mut nonce = [0; 24];
        getrandom::getrandom(&mut nonce).map_err(|_| Error::Unavailable)?;
        let elapsed = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_err(|_| Error::Unavailable)?;
        let millis = u64::try_from(elapsed.as_millis()).map_err(|_| Error::Unavailable)?;
        Ok(Self {
            nonce: ReplayNonce(nonce),
            timestamp: UNIX_EPOCH + Duration::from_millis(millis),
            session_challenge,
        })
    }
}

impl ReplayWindow {
    pub fn new(state: Arc<ReplayState>, capacity: usize) -> Self {
        Self { state, capacity }
    }

    fn with_table<T>(
        &self,
        now: SystemTime,
        operation: impl FnOnce(&mut Table) -> Result<T>,
    ) -> Result<T> {
        if self.capacity == 0 {
            return Err(Error::InvalidConfiguration);
        }
        let mut guard = self.state.inner.lock().map_err(|_| Error::Unavailable)?;
        if guard.is_none() {
            let mut challenge = [0; 32];
            getrandom::getrandom(&mut challenge).map_err(|_| Error::Unavailable)?;
            *guard = Some(Table {
                challenge,
                capacity: self.capacity,
                high_water: now,
                seen: HashSet::new(),
                expiry: BTreeSet::new(),
            });
        }
        let table = guard.as_mut().ok_or(Error::Unavailable)?;
        // Never multiply the node budget by reactor count. A smaller later handle
        // reduces admission without throwing away entries already protecting work.
        table.capacity = table.capacity.min(self.capacity);
        operation(table)
    }

    /// Publish this challenge only in a certificate-authenticated handshake. A
    /// challenge is public randomness, not a replacement for peer authentication.
    pub fn challenge(&self) -> Result<[u8; 32]> {
        self.with_table(SystemTime::now(), |table| Ok(table.challenge))
    }

    /// Compatibility admission using a stable node epoch. Signature verification
    /// should prefer `admit_verified` with the verified leaf-certificate digest.
    pub fn admit(&self, signer: &NodeId, freshness: &Freshness) -> Result<()> {
        self.admit_verified(signer, &[], freshness)
    }

    /// Call only after authenticating the complete receiver-addressed envelope.
    /// Historical signatures in a forwarding chain are verified, not readmitted
    /// here. `epoch` identifies the verified signer certificate (or its digest).
    pub fn admit_verified(
        &self,
        signer: &NodeId,
        epoch: &[u8],
        freshness: &Freshness,
    ) -> Result<()> {
        self.admit_at(signer, epoch, freshness, SystemTime::now())
    }

    /// Explicit time input supports deterministic protocol verification/tests.
    /// The table's wall-clock high water prevents replay resurrection on rollback.
    pub fn admit_at(
        &self,
        signer: &NodeId,
        epoch: &[u8],
        freshness: &Freshness,
        now: SystemTime,
    ) -> Result<()> {
        if !canonical_node(&signer.0) || epoch.len() > MAX_EPOCH_BYTES {
            return Err(Error::InvalidRequest);
        }
        let millis = freshness
            .timestamp
            .duration_since(UNIX_EPOCH)
            .map_err(|_| Error::Replay)?;
        if millis.subsec_nanos() % 1_000_000 != 0 {
            return Err(Error::Replay);
        }
        let mut hash = Sha256::new();
        hash.update(b"racer-replay-v1\0");
        hash.update(signer.0.as_bytes());
        hash.update((epoch.len() as u64).to_be_bytes());
        hash.update(epoch);
        let key = ReplayKey {
            signer_epoch: hash.finalize().into(),
            nonce: freshness.nonce.0,
        };
        self.with_table(now, |table| {
            if freshness.session_challenge != table.challenge {
                return Err(Error::Replay);
            }
            table.high_water = table.high_water.max(now);
            let now = table.high_water;
            if freshness.timestamp > now.checked_add(MAX_FUTURE_SKEW).ok_or(Error::Replay)? {
                return Err(Error::Replay);
            }
            let expires = freshness
                .timestamp
                .checked_add(MAX_AGE)
                .ok_or(Error::Replay)?;
            // Expiry is inclusive. A nonce expires exactly when its envelope can
            // no longer pass freshness, including at the timestamp boundary.
            if now >= expires {
                return Err(Error::Replay);
            }
            while let Some((expiry, old)) = table.expiry.first().copied() {
                if expiry > now {
                    break;
                }
                table.expiry.pop_first();
                table.seen.remove(&old);
            }
            if table.seen.contains(&key) {
                return Err(Error::Replay);
            }
            if table.seen.len() >= table.capacity {
                return Err(Error::Overloaded);
            }
            table.seen.try_reserve(1).map_err(|_| Error::Overloaded)?;
            table.seen.insert(key);
            table.expiry.insert((expires, key));
            Ok(())
        })
    }
}

fn canonical_node(node: &str) -> bool {
    node.len() == 36
        && node.bytes().enumerate().all(|(i, b)| {
            if matches!(i, 8 | 13 | 18 | 23) {
                b == b'-'
            } else {
                b.is_ascii_digit() || (b'a'..=b'f').contains(&b)
            }
        })
}

#[cfg(test)]
mod tests {
    use super::*;
    fn node() -> NodeId {
        NodeId("11111111-1111-4111-8111-111111111111".into())
    }
    fn time() -> SystemTime {
        UNIX_EPOCH + Duration::from_secs(1_000)
    }
    fn window(capacity: usize) -> ReplayWindow {
        ReplayWindow::new(
            Arc::new(ReplayState {
                inner: Mutex::new(Some(Table {
                    challenge: [7; 32],
                    capacity,
                    high_water: time(),
                    seen: HashSet::new(),
                    expiry: BTreeSet::new(),
                })),
            }),
            capacity,
        )
    }
    fn fresh(nonce: u8) -> Freshness {
        Freshness {
            nonce: ReplayNonce([nonce; 24]),
            timestamp: time(),
            session_challenge: [7; 32],
        }
    }

    #[test]
    fn duplicate_epoch_binding_and_saturation_do_not_evict_live_entries() {
        let window = window(2);
        assert_eq!(
            window.admit_at(&node(), b"epoch-a", &fresh(1), time()),
            Ok(())
        );
        assert_eq!(
            window.admit_at(&node(), b"epoch-a", &fresh(1), time()),
            Err(Error::Replay)
        );
        assert_eq!(
            window.admit_at(&node(), b"epoch-b", &fresh(1), time()),
            Ok(())
        );
        assert_eq!(
            window.admit_at(&node(), b"epoch-a", &fresh(2), time()),
            Err(Error::Overloaded)
        );
        assert_eq!(
            window.admit_at(&node(), b"epoch-a", &fresh(1), time()),
            Err(Error::Replay)
        );
    }

    #[test]
    fn exact_expiry_future_skew_and_clock_rollback_fail_closed() {
        let window = window(1);
        let mut future = fresh(1);
        future.timestamp += MAX_FUTURE_SKEW + Duration::from_millis(1);
        assert_eq!(
            window.admit_at(&node(), b"", &future, time()),
            Err(Error::Replay)
        );
        future.timestamp -= Duration::from_millis(1);
        assert_eq!(window.admit_at(&node(), b"", &future, time()), Ok(()));
        let expiration = future.timestamp + MAX_AGE;
        assert_eq!(
            window.admit_at(&node(), b"", &future, expiration),
            Err(Error::Replay)
        );
        assert_eq!(
            window.admit_at(&node(), b"", &future, time()),
            Err(Error::Replay)
        );
        let mut next = fresh(2);
        next.timestamp = expiration;
        assert_eq!(window.admit_at(&node(), b"", &next, expiration), Ok(()));
    }

    #[test]
    fn atomic_across_workers_and_node_capacity_not_per_handle() {
        let window = window(16);
        let mut threads = Vec::new();
        for _ in 0..16 {
            let state = window.state.clone();
            threads.push(std::thread::spawn(move || {
                ReplayWindow::new(state, 16).admit_at(&node(), b"cert", &fresh(3), time())
            }));
        }
        let accepted = threads
            .into_iter()
            .map(|t| t.join().unwrap())
            .filter(|r| r.is_ok())
            .count();
        assert_eq!(accepted, 1);
        let small = ReplayWindow::new(window.state.clone(), 1);
        assert_eq!(
            small.admit_at(&node(), b"cert", &fresh(4), time()),
            Err(Error::Overloaded)
        );
    }

    #[test]
    fn challenges_are_shared_random_and_restart_invalidates_old_envelopes() {
        let state = Arc::new(ReplayState::default());
        let first = ReplayWindow::new(state.clone(), 8);
        let second = ReplayWindow::new(state, 8);
        let challenge = first.challenge().unwrap();
        assert_eq!(challenge, second.challenge().unwrap());
        let restarted = ReplayWindow::new(Arc::new(ReplayState::default()), 8);
        assert_ne!(challenge, restarted.challenge().unwrap());
        let freshness = Freshness::generate(challenge).unwrap();
        assert_eq!(restarted.admit(&node(), &freshness), Err(Error::Replay));
        assert_eq!(first.admit(&node(), &freshness), Ok(()));
        assert_eq!(second.admit(&node(), &freshness), Err(Error::Replay));
        assert_ne!(
            Freshness::generate(challenge).unwrap().nonce.0,
            freshness.nonce.0
        );
    }

    #[test]
    fn rejects_malformed_identity_epoch_time_and_zero_capacity() {
        let window = window(4);
        assert_eq!(
            window.admit_at(&NodeId("bad".into()), b"", &fresh(1), time()),
            Err(Error::InvalidRequest)
        );
        assert_eq!(
            window.admit_at(&node(), &vec![0; MAX_EPOCH_BYTES + 1], &fresh(1), time()),
            Err(Error::InvalidRequest)
        );
        let mut bad = fresh(1);
        bad.timestamp += Duration::from_nanos(1);
        assert_eq!(
            window.admit_at(&node(), b"", &bad, time()),
            Err(Error::Replay)
        );
        let zero = ReplayWindow::new(Arc::new(ReplayState::default()), 0);
        assert_eq!(zero.challenge(), Err(Error::InvalidConfiguration));
    }
}
