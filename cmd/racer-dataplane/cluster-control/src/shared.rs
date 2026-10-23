//! State shared by tasks on the control thread.

use crate::{
    Config,
    authority::{
        publication::Image,
        state::{Freshness, StateRecord},
    },
    exec::Signal,
    kube::Kube,
    limits,
    pki::{DynamicClientVerifier, SeatIssuer, ServingIdentity},
};
use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
    rc::Rc,
    sync::Arc,
    time::Instant,
};

/// A seat this process holds.
pub struct Seat {
    pub index: usize,
    pub issuer: RefCell<Rc<SeatIssuer>>,
    /// Next issuer, offered until its root is committed.
    pub pending: RefCell<Option<Rc<SeatIssuer>>>,
    pub control: RefCell<Rc<ServingIdentity>>,
    pub webhook: RefCell<Rc<ServingIdentity>>,
    pub control_tls: RefCell<Arc<rustls::ServerConfig>>,
    pub webhook_tls: RefCell<Arc<rustls::ServerConfig>>,
    /// Lease authority. Updated on every successful renew.
    pub lease: Cell<Freshness>,
    pub lease_uid: RefCell<String>,
}

/// Committed state and the image that matches it.
#[derive(Default)]
pub struct Serving {
    pub record: Option<StateRecord>,
    /// When the record was last read from or written to the API.
    pub confirmed: Option<Instant>,
    pub image: Option<Image>,
    /// `(generation, encoded bundle)`.
    pub bundle: Option<(u64, Arc<Vec<u8>>)>,
    pub roots: Vec<Vec<u8>>,
    /// Newest record this process ever accepted. Never cleared, also not by
    /// [`crate::seat::fail_closed`], so a rolled back or wiped Secret is
    /// refused for the life of the process.
    pub high_water: Option<StateRecord>,
}

/// The issuer this process offers, reused across seat claims so that
/// reclaims, drains, and index changes do not mint new keyring roots.
pub struct CachedIssuer {
    pub cluster: String,
    pub issuer: Rc<SeatIssuer>,
    /// Last time the root was offered through a seat Lease.
    pub offered: std::time::SystemTime,
}

/// A live seat holder, as last observed by the primary.
#[derive(Clone, Debug, Default)]
pub struct SeatHolder {
    pub pod_name: String,
    pub pod_uid: String,
}

/// What every Pod knows from the primary Lease.
#[derive(Clone, Debug, Default)]
pub struct PrimaryView {
    pub holder: Option<String>,
    pub desired: usize,
    /// Node of each seat, empty when vacant.
    pub seat_nodes: Vec<String>,
    pub claim_window: u64,
    /// Control endpoint and pinned certificate of the primary.
    pub endpoint: Option<(String, Vec<u8>)>,
}

/// Shared runtime state.
pub struct Shared {
    pub config: Config,
    pub cluster: RefCell<String>,
    /// Lease renewals and small named reads.
    pub lease_api: Rc<Kube>,
    /// TokenReview, lists, watches, and other bulk calls.
    pub api: Rc<Kube>,
    pub seat: RefCell<Option<Rc<Seat>>>,
    /// Issuer reused by the next seat claim, see [`crate::seat::new_seat`].
    pub issuer: RefCell<Option<CachedIssuer>>,
    pub serving: RefCell<Serving>,
    /// Notified when the image, bundle, or authority changes.
    pub changed: Signal,
    pub verifier: Arc<DynamicClientVerifier>,
    /// Live seat holders by seat index, kept by the primary.
    pub seat_holders: RefCell<BTreeMap<usize, SeatHolder>>,
    pub is_primary: Cell<bool>,
    pub primary: RefCell<PrimaryView>,
    /// Holder key `<pod name>/<nonce>`, see [`crate::lease::holder_key`].
    pub identity: String,
    /// Per-process nonce, hex.
    pub nonce: String,
    pub stopping: Cell<bool>,
    /// Lease release PUTs in flight. Shutdown waits for these, bounded by
    /// its grace period, so held Leases are freed rather than left to expire.
    pub releasing: Cell<usize>,
    pub last_error: Cell<Option<crate::Error>>,
    /// Jitter and claim-delay salt. Production: [`crate::entropy::OsEntropy`].
    pub rng: RefCell<Box<dyn crate::entropy::Entropy>>,
}

impl Shared {
    /// Deadline of current serving authority: a fresh seat Lease and fresh
    /// committed state. `None` means no authority.
    pub fn authority(&self) -> Freshness {
        let Some(seat) = self.seat.borrow().clone() else {
            return Freshness::none();
        };
        let serving = self.serving.borrow();
        let state = match serving.confirmed {
            Some(at) => Freshness::confirmed(at, limits::STATE_FRESHNESS),
            None => Freshness::none(),
        };
        seat.lease.get().and(state)
    }

    pub fn has_authority(&self) -> bool {
        !self.stopping.get() && self.authority().fresh(Instant::now())
    }

    /// Ready to serve: authority, an image matching the committed record, a
    /// bundle, and this seat's issuer root committed.
    pub fn ready(&self) -> bool {
        if !self.has_authority() {
            return false;
        }
        let Some(seat) = self.seat.borrow().clone() else {
            return false;
        };
        let serving = self.serving.borrow();
        let (Some(record), Some(image)) = (&serving.record, &serving.image) else {
            return false;
        };
        image.record.sequence == record.sequence
            && image.record.content_hash == record.content_hash
            && serving.bundle.is_some()
            && record.keyring.root_committed(seat.issuer.borrow().der())
    }

    /// Current image when ready.
    pub fn image(&self) -> Option<Image> {
        if !self.ready() {
            return None;
        }
        self.serving.borrow().image.clone()
    }

    /// Current bundle when ready.
    pub fn bundle(&self) -> Option<(u64, Arc<Vec<u8>>)> {
        if !self.ready() {
            return None;
        }
        self.serving.borrow().bundle.clone()
    }

    pub fn roots(&self) -> Vec<Vec<u8>> {
        self.serving.borrow().roots.clone()
    }

    /// Count a Lease release in flight until the guard drops.
    pub fn releasing(self: &Rc<Self>) -> Releasing {
        self.releasing.set(self.releasing.get() + 1);
        Releasing(self.clone())
    }

    /// Drop the seat and stop serving.
    pub fn lose_seat(&self) {
        *self.seat.borrow_mut() = None;
        self.is_primary.set(false);
        self.changed.notify();
    }
}

/// In-flight Lease release; see [`Shared::releasing`].
pub struct Releasing(Rc<Shared>);

impl Drop for Releasing {
    fn drop(&mut self) {
        self.0.releasing.set(self.0.releasing.get() - 1);
        self.0.changed.notify();
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    /// Shared state for handler tests against the fake API.
    pub fn shared(kube: Rc<Kube>) -> Rc<Shared> {
        let config = crate::Config {
            namespace: "racer-system".into(),
            pod_name: "p1".into(),
            pod_uid: "00000001-0000-4000-8000-0000000000bb".into(),
            node_name: "n1".into(),
            pod_ip: "127.0.0.2".parse().unwrap(),
            seats: crate::SeatPolicy::Auto,
            control_listen: "127.0.0.2:0".parse().unwrap(),
            webhook_listen: "127.0.0.2:0".parse().unwrap(),
            webhook_alternate: None,
            object_image: None,
            api_server: String::new(),
            api_ca: Default::default(),
            api_token: Default::default(),
            control_token: Default::default(),
            daemonset: "racer-dataplane".into(),
            service_account: "racer-dataplane".into(),
            peer_port: 8082,
            rotation: Default::default(),
        };
        Rc::new(Shared {
            identity: "p1/00".into(),
            nonce: "00".into(),
            config,
            cluster: RefCell::new("c1c1c1c1-0000-4000-8000-000000000001".into()),
            lease_api: kube.clone(),
            api: kube,
            seat: RefCell::new(None),
            issuer: RefCell::new(None),
            serving: RefCell::new(Serving::default()),
            changed: Signal::default(),
            verifier: DynamicClientVerifier::new(),
            seat_holders: RefCell::new(BTreeMap::new()),
            is_primary: Cell::new(false),
            primary: RefCell::new(PrimaryView::default()),
            stopping: Cell::new(false),
            releasing: Cell::new(0),
            last_error: Cell::new(None),
            rng: RefCell::new(Box::new(crate::entropy::Seeded::new(1))),
        })
    }
}
