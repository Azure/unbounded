//! The primary: one seat holder that observes the cluster, commits durable
//! state, and maintains discovery, webhook trust, and webhook endpoints.
//!
//! Holding the primary Lease does not authorize writes by itself. The first
//! commit records this term in the state Secret. Every commit re-reads the
//! Secret, checks the term (see [`state::Writer`]), and checks the Lease is
//! still fresh by the monotonic clock.

use crate::step::primary::{Contend, Inventory, PassInputs, PassPlan, discovery, plan_pass};
use crate::{
    annotations,
    authority::{
        catalog::CacheInfo,
        election::{self, LeaseObservation, Observer},
        keyring::Policy,
        members::{NodeInfo, Ownership, PodInfo},
        publication::Image,
        state::{self, Freshness, StateError, StateRecord, Term, Writer, lease_authority},
    },
    exec::{self, Signal},
    kube::{KubeError, paths, str_at},
    lease, limits,
    seat::{self, install_record, state_markers_match},
    shared::{Seat, SeatHolder, Shared},
};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde_json::{Value, json};
use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
    net::IpAddr,
    rc::Rc,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

/// Reconcile at least this often.
const RESYNC: Duration = Duration::from_secs(30);

/// Coalesce bursts of changes.
const DEBOUNCE: Duration = Duration::from_secs(1);

fn unix_now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

fn seat_mine(shared: &Shared, seat: &Rc<Seat>) -> bool {
    shared
        .seat
        .borrow()
        .as_ref()
        .is_some_and(|s| Rc::ptr_eq(s, seat))
}

/// Contend for the primary Lease while holding `seat`.
pub async fn contend(shared: Rc<Shared>, seat: Rc<Seat>) {
    let ns = shared.config.namespace.clone();
    let mut contend = Contend::default();
    while seat_mine(&shared, &seat) && !shared.stopping.get() {
        if let Ok(object) = lease::get(&shared.lease_api, &ns, annotations::PRIMARY_LEASE).await {
            let obs = lease::observe(&object);
            if contend.observe(&obs, Instant::now(), seat_mine(&shared, &seat)) {
                let spec = election::claim(
                    &obs.spec,
                    &shared.config.pod_name,
                    &election::now_micro(SystemTime::now()),
                );
                let started = Instant::now();
                let body = lease::with(
                    &object,
                    &spec,
                    &primary_annotations(&shared, &seat, &BTreeMap::new()),
                );
                if let Ok(v) = lease::put(&shared.lease_api, &ns, &body).await {
                    lead(&shared, &seat, v, started).await;
                    contend.term_ended();
                }
            }
        }
        let wait = Contend::wait(&mut **shared.rng.borrow_mut());
        exec::sleep(wait).await;
    }
}

fn primary_annotations(
    shared: &Shared,
    seat: &Seat,
    extra: &BTreeMap<&'static str, Option<String>>,
) -> BTreeMap<&'static str, Option<String>> {
    let mut a = BTreeMap::new();
    a.insert(annotations::HOLDER_UID, Some(shared.config.pod_uid.clone()));
    a.insert(annotations::HOLDER_NONCE, Some(shared.nonce.clone()));
    a.insert(
        annotations::HOLDER_NODE,
        Some(shared.config.node_name.clone()),
    );
    a.insert(
        annotations::CLUSTER_ID,
        Some(shared.cluster.borrow().clone()),
    );
    a.insert(annotations::ENDPOINT, Some(seat::endpoint_url(shared)));
    a.insert(
        annotations::SERVING_CERTIFICATE,
        Some(STANDARD.encode(&seat.control.borrow().ca_der)),
    );
    for (k, v) in extra {
        a.insert(k, v.clone());
    }
    a
}

/// One primary term.
struct Term_ {
    shared: Rc<Shared>,
    seat: Rc<Seat>,
    writer: RefCell<Writer>,
    lease: Cell<Freshness>,
    object: RefCell<Value>,
    alive: Rc<Cell<bool>>,
    inventory: Rc<RefCell<Inventory>>,
    changed: Signal,
    extra: RefCell<BTreeMap<&'static str, Option<String>>>,
    /// Notified when `extra` changes, so renew publishes it without waiting
    /// a full renew period.
    publish: Signal,
    seat_observer: RefCell<Observer>,
    webhook_retired: RetiredTrust,
    /// After a commit: (committed sequence, replaced sequence, grace end).
    /// Seats still at the replaced sequence count as ready until then.
    ready_floor: Cell<Option<(u64, u64, Instant)>>,
    /// Wall time (Unix seconds) this term last committed a new membership
    /// version; drives the join hold-down (`step::primary::JOIN_HOLD_SECS`).
    membership_changed: Cell<Option<u64>>,
}

/// How long after a commit seats still READY at the replaced sequence count
/// as ready. Covers seats' rate-limited READY updates and pass debouncing so
/// discovery and the webhook EndpointSlice do not briefly empty.
const READY_FLOOR_GRACE: Duration = limits::LEASE_RENEW;

/// The replaced sequence still accepted as ready, if the stored floor is for
/// the sequence currently served and its grace has not ended.
pub(crate) fn ready_floor(
    stored: Option<(u64, u64, Instant)>,
    current: Option<u64>,
    now: Instant,
) -> Option<u64> {
    let (committed, previous, until) = stored?;
    (current == Some(committed) && now < until).then_some(previous)
}

async fn lead(shared: &Rc<Shared>, seat: &Rc<Seat>, object: Value, started: Instant) {
    let obs = lease::observe(&object);
    let term = Term {
        lease_uid: obs.uid.clone(),
        transitions: obs.spec.transitions,
        holder: shared.identity.clone(),
    };
    let t = Rc::new(Term_ {
        shared: shared.clone(),
        seat: seat.clone(),
        writer: RefCell::new(Writer::new(term)),
        lease: Cell::new(lease_authority(started)),
        object: RefCell::new(object),
        alive: Rc::new(Cell::new(true)),
        inventory: Rc::new(RefCell::new(Inventory::default())),
        changed: Signal::default(),
        extra: RefCell::new(BTreeMap::new()),
        publish: Signal::default(),
        seat_observer: RefCell::new(Observer::default()),
        webhook_retired: RefCell::new(BTreeMap::new()),
        ready_floor: Cell::new(None),
        membership_changed: Cell::new(None),
    });
    // Held for the whole term: shutdown waits until the loop exits and the
    // release PUT below completes, even if the seat is cleared first.
    let _releasing = shared.releasing();
    shared.is_primary.set(true);
    shared.changed.notify();
    let _ = exec::spawn(renew(t.clone()));
    for kind in [Kind::Nodes, Kind::Pods, Kind::Caches] {
        let _ = exec::spawn(watch(t.clone(), kind));
    }
    let mut next = Instant::now();
    while t.alive.get() && seat_mine(shared, seat) && !shared.stopping.get() {
        let generation = t.changed.generation();
        let _ = exec::deadline(next, wake(shared, seat, &t.changed, generation)).await;
        if shared.stopping.get() || !seat_mine(shared, seat) {
            break;
        }
        exec::sleep(DEBOUNCE).await;
        if !t.alive.get() {
            break;
        }
        if let Err(
            StateError::StaleTerm
            | StateError::LeaseReplaced
            | StateError::Wiped
            | StateError::Invalid
            | StateError::Rollback
            | StateError::ClusterMismatch,
        ) = reconcile(&t).await
        {
            // Fenced or durable state is unusable. Stop writing for good.
            seat::fail_closed(shared);
            t.alive.set(false);
            break;
        }
        next = Instant::now() + limits::LEASE_RENEW;
    }
    t.alive.set(false);
    shared.is_primary.set(false);
    shared.changed.notify();
    if shared.stopping.get() || !seat_mine(shared, seat) {
        let object = t.object.borrow().clone();
        let spec = election::release(&lease::observe(&object).spec);
        let a: BTreeMap<_, _> = primary_annotations(shared, seat, &t.extra.borrow())
            .into_keys()
            .map(|k| (k, None))
            .collect();
        let _ = exec::timeout(
            limits::API_TIMEOUT,
            lease::put(
                &shared.lease_api,
                &shared.config.namespace,
                &lease::with(&object, &spec, &a),
            ),
        )
        .await;
    }
}

/// Wait for `changed` past `generation`, or for shutdown or loss of `seat`,
/// so the term ends promptly and releases the Lease.
async fn wake(shared: &Shared, seat: &Rc<Seat>, changed: &Signal, generation: u64) {
    loop {
        let outer = shared.changed.generation();
        if shared.stopping.get() || !seat_mine(shared, seat) {
            return;
        }
        let ours = std::pin::pin!(changed.changed(generation));
        let theirs = std::pin::pin!(shared.changed.changed(outer));
        if let futures::future::Either::Left(_) = futures::future::select(ours, theirs).await {
            return;
        }
    }
}

async fn renew(t: Rc<Term_>) {
    let ns = t.shared.config.namespace.clone();
    while t.alive.get() {
        let started = Instant::now();
        // Taken before reading `extra`: a change from here on renews again.
        let publish = t.publish.generation();
        let object = t.object.borrow().clone();
        let spec = election::renew(
            &lease::observe(&object).spec,
            &election::now_micro(SystemTime::now()),
        );
        let body = lease::with(
            &object,
            &spec,
            &primary_annotations(&t.shared, &t.seat, &t.extra.borrow()),
        );
        match exec::timeout(
            limits::LEASE_RENEW,
            lease::put(&t.shared.lease_api, &ns, &body),
        )
        .await
        {
            Ok(Ok(v)) => {
                *t.object.borrow_mut() = v;
                t.lease.set(lease_authority(started));
            }
            Ok(Err(KubeError::Conflict)) => {
                match lease::get(&t.shared.lease_api, &ns, annotations::PRIMARY_LEASE).await {
                    // Same holder key is not enough: a recreated Lease or a
                    // new claim is another term.
                    Ok(v)
                        if same_term(
                            &lease::observe(&v),
                            &t.shared.identity,
                            t.writer.borrow().term(),
                        ) =>
                    {
                        *t.object.borrow_mut() = v;
                        continue;
                    }
                    Ok(_) => t.alive.set(false),
                    Err(_) => {}
                }
            }
            _ => {}
        }
        if !t.lease.get().fresh(Instant::now()) || !seat_mine(&t.shared, &t.seat) {
            t.alive.set(false);
            t.changed.notify();
            return;
        }
        // Renew on schedule, or early to publish changed discovery.
        let _ = exec::deadline(started + limits::LEASE_RENEW, t.publish.changed(publish)).await;
    }
}

/// The live primary Lease UID, read only when `current` was stamped under
/// another Lease, so a term on a recreated Lease can take the record over.
/// Read after `current`: a UID read before it could predate the record.
async fn live_lease_uid(
    t: &Term_,
    current: Option<&StateRecord>,
) -> Result<Option<String>, StateError> {
    let ours = t.writer.borrow().term().lease_uid.clone();
    if current.is_none_or(|c| c.term.lease_uid == ours) {
        return Ok(None);
    }
    let ns = &t.shared.config.namespace;
    match lease::get(&t.shared.lease_api, ns, annotations::PRIMARY_LEASE).await {
        Ok(v) => Ok(Some(lease::observe(&v).uid)),
        // Missing Lease: no UID is live, so the record is not ours to take.
        Err(KubeError::NotFound) => Ok(None),
        // Transient (non-fatal, like a failed Secret read): retry later
        // rather than fence on an unknown answer.
        Err(_) => Err(StateError::TooLarge),
    }
}

/// Whether `o` is still the Lease term `term` held by `identity`.
pub(crate) fn same_term(o: &LeaseObservation, identity: &str, term: &Term) -> bool {
    lease::holder_key(o).as_deref() == Some(identity)
        && o.uid == term.lease_uid
        && o.spec.transitions == term.transitions
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Kind {
    Nodes,
    Pods,
    Caches,
}

fn collection(t: &Term_, kind: Kind) -> String {
    match kind {
        Kind::Nodes => paths::NODES.into(),
        Kind::Pods => paths::pods(&t.shared.config.namespace),
        Kind::Caches => paths::CACHES.into(),
    }
}

fn apply(inv: &mut Inventory, kind: Kind, event: &str, v: &Value) {
    let key = str_at(v, &["metadata", "uid"]).to_owned();
    let deleted = event == "DELETED";
    match kind {
        Kind::Nodes => {
            let n = NodeInfo::from_json(v);
            let name = n.name.clone();
            if deleted {
                inv.nodes.remove(&name);
            } else {
                inv.nodes.insert(name, n);
            }
        }
        Kind::Pods => {
            if deleted {
                inv.pods.remove(&key);
            } else {
                inv.pods.insert(key, PodInfo::from_json(v));
            }
        }
        Kind::Caches => {
            if deleted {
                inv.caches.remove(&key);
            } else {
                inv.caches.insert(key, CacheInfo::from_json(v));
            }
        }
    }
}

/// List then watch one collection, relisting on 410 or errors.
async fn watch(t: Rc<Term_>, kind: Kind) {
    let path = collection(&t, kind);
    let mut backoff = 1u64;
    while t.alive.get() {
        let (items, mut rv) = match t.shared.api.list(&path, "").await {
            Ok(r) => r,
            Err(KubeError::NotFound) if kind == Kind::Caches => {
                // CRD not installed: an empty catalog.
                {
                    let mut inv = t.inventory.borrow_mut();
                    inv.caches.clear();
                    inv.caches_synced = true;
                }
                t.changed.notify();
                exec::sleep(RESYNC).await;
                continue;
            }
            Err(_) => {
                exec::sleep(Duration::from_secs(backoff)).await;
                backoff = (backoff * 2).min(30);
                continue;
            }
        };
        backoff = 1;
        {
            let mut inv = t.inventory.borrow_mut();
            match kind {
                Kind::Nodes => inv.nodes.clear(),
                Kind::Pods => inv.pods.clear(),
                Kind::Caches => inv.caches.clear(),
            }
            for v in &items {
                apply(&mut inv, kind, "ADDED", v);
            }
            match kind {
                Kind::Nodes => inv.nodes_synced = true,
                Kind::Pods => inv.pods_synced = true,
                Kind::Caches => inv.caches_synced = true,
            }
        }
        drop(items);
        t.changed.notify();
        loop {
            let Ok(mut w) = t.shared.api.watch(&path, "", &rv).await else {
                break;
            };
            let mut relist = false;
            loop {
                if !t.alive.get() {
                    return;
                }
                match w.next().await {
                    Ok(Some(e)) => {
                        rv = str_at(&e.object, &["metadata", "resourceVersion"]).to_owned();
                        if e.kind != "BOOKMARK" {
                            apply(&mut t.inventory.borrow_mut(), kind, &e.kind, &e.object);
                            t.changed.notify();
                        }
                    }
                    Ok(None) => break,
                    Err(_) => {
                        relist = true;
                        break;
                    }
                }
            }
            if relist || !t.alive.get() {
                break;
            }
        }
    }
}

/// Read, decode, and check the state Secret.
async fn read_state(shared: &Shared) -> Result<(Value, Option<StateRecord>), StateError> {
    let secret = shared
        .lease_api
        .get(&paths::state(&shared.config.namespace))
        .await
        .map_err(|_| StateError::TooLarge)?;
    let uid = str_at(&secret, &["metadata", "uid"]).to_owned();
    if uid != *shared.cluster.borrow() {
        return Err(StateError::ClusterMismatch);
    }
    let initialized = !str_at(
        &secret,
        &["metadata", "annotations", annotations::INITIALIZED],
    )
    .is_empty();
    if initialized && !state_markers_match(&secret, &uid) {
        return Err(StateError::Invalid);
    }
    let data = match secret["data"][annotations::STATE_KEY].as_str() {
        Some(s) => Some(STANDARD.decode(s).map_err(|_| StateError::Invalid)?),
        None => None,
    };
    let record = state::decode(&uid, initialized, data.as_deref())?;
    // Never build on state older than what this process already accepted.
    if let Some(hw) = &shared.serving.borrow().high_water {
        match &record {
            None => return Err(StateError::Wiped),
            Some(r) => state::check_monotonic(hw, r)?,
        }
    }
    Ok((secret, record))
}

/// Seats as seen by the primary.
struct SeatSnapshot {
    holders: BTreeMap<usize, (LeaseObservation, Value)>,
}

async fn read_seats(t: &Term_) -> SeatSnapshot {
    let ns = &t.shared.config.namespace;
    let mut holders = BTreeMap::new();
    for i in 0..annotations::MAX_SEATS {
        let Ok(v) = lease::get(&t.shared.lease_api, ns, &annotations::seat_lease(i)).await else {
            continue;
        };
        let o = lease::observe(&v);
        t.seat_observer.borrow_mut().observe(&o, Instant::now());
        holders.insert(i, (o, v));
    }
    SeatSnapshot { holders }
}

fn live_seats(t: &Term_, s: &SeatSnapshot) -> BTreeMap<usize, LeaseObservation> {
    let now = Instant::now();
    let cluster = t.shared.cluster.borrow().clone();
    s.holders
        .iter()
        .filter(|(_, (o, _))| {
            o.spec.holder.is_some()
                && !t.seat_observer.borrow().expired(o, now)
                && o.annotations.get(annotations::CLUSTER_ID) == Some(&cluster)
                && o.annotations.contains_key(annotations::HOLDER_UID)
        })
        .map(|(i, (o, _))| (*i, o.clone()))
        .collect()
}

/// One reconcile pass and, when needed, one commit.
async fn reconcile(t: &Rc<Term_>) -> Result<(), StateError> {
    let shared = &t.shared;
    let ready = {
        let inv = t.inventory.borrow();
        inv.nodes_synced && inv.pods_synced && inv.caches_synced
    };
    let seats = read_seats(t).await;
    let live = live_seats(t, &seats);
    *shared.seat_holders.borrow_mut() = live
        .iter()
        .map(|(i, o)| {
            (
                *i,
                SeatHolder {
                    pod_name: o.spec.holder.clone().unwrap_or_default(),
                    pod_uid: o
                        .annotations
                        .get(annotations::HOLDER_UID)
                        .cloned()
                        .unwrap_or_default(),
                },
            )
        })
        .collect();
    let ds = shared
        .api
        .get(&paths::daemonset(
            &shared.config.namespace,
            &shared.config.daemonset,
        ))
        .await;
    let ownership = Ownership {
        namespace: shared.config.namespace.clone(),
        daemonset: shared.config.daemonset.clone(),
        uid: match &ds {
            Ok(v) if str_at(v, &["metadata", "deletionTimestamp"]).is_empty() => {
                str_at(v, &["metadata", "uid"]).into()
            }
            Ok(_) | Err(KubeError::NotFound) => String::new(),
            Err(_) => return Ok(()),
        },
        service_account: shared.config.service_account.clone(),
    };

    let mut attempts = 0;
    loop {
        attempts += 1;
        let read_started = Instant::now();
        let (secret, current) = read_state(shared).await?;
        let live_uid = live_lease_uid(t, current.as_ref()).await?;
        t.writer
            .borrow_mut()
            .observe(current.as_ref(), live_uid.as_deref())?;
        let cluster = shared.cluster.borrow().clone();
        let previous_image = shared.serving.borrow().image.clone();
        let policy = Policy::from(shared.config.rotation);
        let plan = {
            let inventory = t.inventory.borrow();
            let writer = t.writer.borrow();
            plan_pass(
                &PassInputs {
                    cluster: &cluster,
                    pod_uid: &shared.config.pod_uid,
                    seats: shared.config.seats,
                    peer_port: shared.config.peer_port,
                    policy: &policy,
                    inventory: &inventory,
                    synced: ready,
                    ownership: &ownership,
                    current: current.as_ref(),
                    previous_image: previous_image.as_ref(),
                    live: &live,
                    writer: &writer,
                    wall_unix: unix_now(),
                    membership_changed_unix: t.membership_changed.get(),
                },
                &mut **shared.rng.borrow_mut(),
            )?
        };
        if plan.refused_roots > 0 {
            // Root slots are full. Those seats stay unready until slots free up.
            shared.last_error.set(Some(crate::Error::Unavailable));
        }
        let PassPlan {
            next,
            image,
            desired,
            eligible,
            changed,
            ..
        } = plan;

        if !changed {
            if let Some(c) = current {
                install_record(shared, c, read_started)?;
                install_image(shared, image);
            }
            update_discovery(t, &live, desired, eligible).await;
            return Ok(());
        }
        if let Some(c) = &current {
            state::check_monotonic(c, &next)?;
        }
        if !t.lease.get().fresh(Instant::now()) || !t.writer.borrow().may_write() {
            return Err(StateError::StaleTerm);
        }
        let encoded = state::encode(&next)?;
        let mut body = secret.clone();
        body["data"] = json!({
            annotations::CLUSTER_KEY: STANDARD.encode(cluster.as_bytes()),
            annotations::STATE_KEY: STANDARD.encode(&encoded),
        });
        if !body["metadata"]["annotations"].is_object() {
            body["metadata"]["annotations"] = json!({});
        }
        body["metadata"]["annotations"][annotations::INITIALIZED] = cluster.clone().into();
        body["metadata"]["annotations"][annotations::CLUSTER_ID] = cluster.clone().into();
        let started = Instant::now();
        // Recheck right before the write.
        if !t.lease.get().fresh(started) {
            return Err(StateError::StaleTerm);
        }
        match shared
            .lease_api
            .put(&paths::state(&shared.config.namespace), &body)
            .await
        {
            Ok(_) => {
                if current
                    .as_ref()
                    .is_none_or(|c| c.membership_version != next.membership_version)
                {
                    t.membership_changed.set(Some(unix_now()));
                }
                // Seats learn the new record after this pass, so a seat
                // still at the sequence this commit replaced keeps serving.
                t.ready_floor.set(
                    current
                        .as_ref()
                        .map(|c| (next.sequence, c.sequence, started + READY_FLOOR_GRACE)),
                );
                install_record(shared, next, started)?;
                install_image(shared, image);
                update_discovery(t, &live, desired, eligible).await;
                return Ok(());
            }
            Err(KubeError::Conflict) if attempts < 3 => continue,
            Err(_) => return Ok(()),
        }
    }
}

fn install_image(shared: &Shared, image: Option<Image>) {
    let Some(image) = image else { return };
    let mut s = shared.serving.borrow_mut();
    let matches = s.record.as_ref().is_some_and(|r| {
        r.sequence == image.record.sequence && r.content_hash == image.record.content_hash
    });
    if matches && s.image.as_ref().is_none_or(|i| i.record != image.record) {
        s.image = Some(image);
        drop(s);
        shared.changed.notify();
    }
}

/// Publish seat count, vacancies, and ready endpoints on the primary Lease,
/// and keep webhook trust and endpoints in order.
async fn update_discovery(
    t: &Term_,
    live: &BTreeMap<usize, LeaseObservation>,
    desired: usize,
    eligible: usize,
) {
    let shared = &t.shared;
    let sequence = shared.serving.borrow().record.as_ref().map(|r| r.sequence);
    let previous = ready_floor(t.ready_floor.get(), sequence, Instant::now());
    let d = discovery(live, desired, eligible, sequence, previous);
    {
        let mut extra = t.extra.borrow_mut();
        let before = extra.clone();
        extra.extend(d.extra);
        if *extra != before {
            drop(extra);
            t.publish.notify();
        }
    }
    let webhooks = d.webhooks;
    let fresh =
        || t.alive.get() && t.writer.borrow().may_write() && t.lease.get().fresh(Instant::now());
    let _ = reconcile_webhook(
        shared,
        &webhooks,
        &t.webhook_retired,
        Instant::now(),
        &fresh,
    )
    .await;
}

/// Whether a seat's READY sequence counts as serving. A seat at the current
/// sequence is ready; right after a commit, a seat still at the sequence the
/// commit replaced also stays ready, since seats only learn new records after
/// the commit and a stale-by-one seat serves a consistent older snapshot
/// (clients ahead of it get Unavailable and retry elsewhere). Missing,
/// malformed, older, or newer-than-current READY values are not ready.
pub(crate) fn seat_ready(ready: Option<&str>, current: Option<u64>, previous: Option<u64>) -> bool {
    let (Some(current), Some(seat)) = (current, ready.and_then(|r| r.parse::<u64>().ok())) else {
        return false;
    };
    seat <= current && seat >= previous.unwrap_or(current).min(current)
}

/// Webhook IPs a seat published, one per family, falling back to its
/// endpoint IP when the annotation is absent or malformed.
pub(crate) fn webhook_ips(published: Option<&String>, endpoint: IpAddr) -> Vec<IpAddr> {
    let parsed: Option<Vec<IpAddr>> =
        published.and_then(|v| v.split(',').map(|s| s.parse().ok()).collect());
    match parsed {
        Some(ips)
            if !ips.is_empty()
                && ips.len() <= 2
                && !(ips.len() == 2 && ips[0].is_ipv6() == ips[1].is_ipv6()) =>
        {
            ips
        }
        _ => vec![endpoint],
    }
}

/// A ready seat serving the webhook.
#[derive(Clone, Debug)]
pub struct WebhookSeat {
    pub pod: String,
    pub uid: String,
    /// At most one IP per address family.
    pub ips: Vec<IpAddr>,
    pub ca: Vec<u8>,
}

/// PEM encoding of DER certificates.
pub fn pem(ders: &[Vec<u8>]) -> String {
    let mut out = String::new();
    for d in ders {
        out.push_str("-----BEGIN CERTIFICATE-----\n");
        let b = STANDARD.encode(d);
        for chunk in b.as_bytes().chunks(64) {
            out.push_str(std::str::from_utf8(chunk).unwrap_or_default());
            out.push('\n');
        }
        out.push_str("-----END CERTIFICATE-----\n");
    }
    out
}

fn parse_pem(bundle: &[u8]) -> Vec<Vec<u8>> {
    let mut r = bundle;
    rustls_pemfile::certs(&mut r)
        .filter_map(Result::ok)
        .map(|c| c.to_vec())
        .collect()
}

async fn patch_ca_bundle(shared: &Shared, mwc: &Value, cas: &[Vec<u8>]) -> Result<(), KubeError> {
    let encoded = STANDARD.encode(pem(cas));
    let mut ops = Vec::new();
    for (i, w) in mwc["webhooks"].as_array().into_iter().flatten().enumerate() {
        if w["clientConfig"]["caBundle"].as_str() != Some(encoded.as_str()) {
            ops.push(
                json!({"op": "test", "path": format!("/webhooks/{i}/name"), "value": w["name"]}),
            );
            let op = if w["clientConfig"].get("caBundle").is_some() {
                "replace"
            } else {
                "add"
            };
            ops.push(json!({"op": op, "path": format!("/webhooks/{i}/clientConfig/caBundle"), "value": encoded}));
        }
    }
    if ops.is_empty() {
        return Ok(());
    }
    shared
        .api
        .json_patch(&paths::webhook_configuration(), &Value::Array(ops))
        .await
        .map(|_| ())
}

/// How long webhook trust outlives the endpoint that used it. EndpointSlice
/// propagation to every API server is not atomic with the slice write.
pub const WEBHOOK_TRUST_GRACE: Duration = Duration::from_secs(120);

/// Removed webhook CAs, by DER, with the time they left the endpoints.
pub type RetiredTrust = RefCell<BTreeMap<Vec<u8>, Instant>>;

/// Add trust, then set endpoints, then remove trust no longer used once it
/// has been out of the endpoints for [`WEBHOOK_TRUST_GRACE`]. `fresh` is
/// checked before every write so a stale primary stops mid-way.
pub async fn reconcile_webhook(
    shared: &Shared,
    seats: &[WebhookSeat],
    retired: &RetiredTrust,
    now: Instant,
    fresh: &dyn Fn() -> bool,
) -> Result<(), KubeError> {
    if !fresh() {
        return Ok(());
    }
    let mwc = match shared.api.get(&paths::webhook_configuration()).await {
        Ok(v) => v,
        Err(KubeError::NotFound) => return Ok(()),
        Err(e) => return Err(e),
    };
    let current: Vec<Vec<u8>> = mwc["webhooks"][0]["clientConfig"]["caBundle"]
        .as_str()
        .and_then(|b| STANDARD.decode(b).ok())
        .map(|b| parse_pem(&b))
        .unwrap_or_default();
    let desired: Vec<Vec<u8>> = seats.iter().map(|s| s.ca.clone()).collect();
    let mut union = current.clone();
    for d in &desired {
        if !union.contains(d) {
            union.push(d.clone());
        }
    }
    if !fresh() {
        return Ok(());
    }
    patch_ca_bundle(shared, &mwc, &union).await?;
    if !fresh() {
        return Ok(());
    }
    let mwc = shared.api.get(&paths::webhook_configuration()).await?;
    let slice_path = paths::endpoint_slice(&shared.config.namespace);
    match shared.api.get(&slice_path).await {
        Ok(mut slice) => {
            // The address type is immutable and follows the Service primary
            // family, the family of its ClusterIP. Publish each seat's IP of
            // that family; a seat without one cannot be reached and is skipped.
            let v6 = slice["addressType"].as_str() == Some("IPv6");
            let endpoints: Vec<Value> = seats
                .iter()
                .filter_map(|s| Some((s, s.ips.iter().find(|ip| ip.is_ipv6() == v6)?)))
                .take(annotations::MAX_SEATS)
                .map(|(s, ip)| {
                    json!({
                        "addresses": [ip.to_string()],
                        "conditions": {"ready": true},
                        "targetRef": {"kind": "Pod", "namespace": shared.config.namespace, "name": s.pod, "uid": s.uid},
                    })
                })
                .collect();
            if slice["endpoints"] != Value::Array(endpoints.clone()) {
                if !fresh() {
                    return Ok(());
                }
                slice["endpoints"] = Value::Array(endpoints);
                shared.api.put(&slice_path, &slice).await?;
            }
        }
        Err(KubeError::NotFound) => {}
        Err(e) => return Err(e),
    }
    // Keep removed trust for a bounded grace. At most one retired CA per
    // seat slot is kept; the oldest go first.
    let keep = {
        let mut r = retired.borrow_mut();
        r.retain(|ca, _| !desired.contains(ca) && union.contains(ca));
        for ca in union.iter().filter(|c| !desired.contains(c)) {
            r.entry(ca.clone()).or_insert(now);
        }
        r.retain(|_, at| now.duration_since(*at) < WEBHOOK_TRUST_GRACE);
        while r.len() > annotations::MAX_SEATS {
            let oldest = r.iter().min_by_key(|(_, at)| **at).map(|(k, _)| k.clone());
            if let Some(k) = oldest {
                r.remove(&k);
            }
        }
        r.keys().cloned().collect::<Vec<_>>()
    };
    // Keep union order so an unchanged set is not rewritten.
    let target: Vec<Vec<u8>> = union
        .iter()
        .filter(|c| desired.contains(c) || keep.contains(c))
        .cloned()
        .collect();
    if union != target && fresh() {
        patch_ca_bundle(shared, &mwc, &target).await?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    #[test]
    fn ready_floor_grace() {
        use super::ready_floor;
        let now = std::time::Instant::now();
        let later = now + std::time::Duration::from_secs(5);
        assert_eq!(ready_floor(Some((7, 6, later)), Some(7), now), Some(6));
        assert_eq!(ready_floor(Some((7, 6, now)), Some(7), later), None);
        assert_eq!(ready_floor(Some((7, 6, later)), Some(8), now), None);
        assert_eq!(ready_floor(None, Some(7), now), None);
    }

    use super::*;

    #[test]
    fn webhook_trust_added_before_endpoints_and_removed_after() {
        crate::kube::fake::run(|api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let mwc = paths::webhook_configuration();
            let slice = paths::endpoint_slice(&shared.config.namespace);
            api.put_object(&mwc, json!({"metadata":{"name":"racer-object-sidecar-injector"},"webhooks":[{"name":"a.racer","clientConfig":{"service":{"name":"racer-webhook"}}}]}));
            api.put_object(&slice, json!({"metadata":{"name":"racer-webhook"},"addressType":"IPv4","ports":[{"port":9443}],"endpoints":[]}));
            let seat = |n: u8, ip: &str| WebhookSeat {
                pod: format!("racer-{n}"),
                uid: format!("u{n}"),
                ips: vec![ip.parse().unwrap()],
                ca: crate::authority::keyring::tests::root(1),
            };
            let a = seat(1, "10.0.0.1");
            let v6 = seat(3, "fd00::1");
            let retired = RetiredTrust::default();
            let t0 = Instant::now();
            let yes = || true;
            reconcile_webhook(&shared, &[a.clone(), v6], &retired, t0, &yes)
                .await
                .unwrap();
            let bundle = |api: &crate::kube::fake::FakeApi| {
                let v = api.object(&mwc).unwrap();
                parse_pem(
                    &STANDARD
                        .decode(
                            v["webhooks"][0]["clientConfig"]["caBundle"]
                                .as_str()
                                .unwrap(),
                        )
                        .unwrap(),
                )
            };
            assert!(bundle(&api).contains(&a.ca));
            let eps = api.object(&slice).unwrap()["endpoints"].clone();
            assert_eq!(eps.as_array().unwrap().len(), 1, "other family skipped");
            assert_eq!(eps[0]["targetRef"]["name"], "racer-1");
            assert_eq!(api.object(&slice).unwrap()["ports"][0]["port"], 9443);

            // Replace seat a with b. Order: add b trust, swap endpoints, drop a trust.
            let b = seat(2, "10.0.0.2");
            let start = api.requests().len();
            reconcile_webhook(&shared, std::slice::from_ref(&b), &retired, t0, &yes)
                .await
                .unwrap();
            let writes: Vec<String> = api.requests()[start..]
                .iter()
                .filter(|(m, _)| m != "GET")
                .map(|(m, p)| {
                    format!(
                        "{m} {}",
                        if p.contains("endpointslices") {
                            "slice"
                        } else {
                            "mwc"
                        }
                    )
                })
                .collect();
            // Old trust is kept for the grace after the endpoint swap.
            assert_eq!(writes, ["PATCH mwc", "PUT slice"]);
            assert!(bundle(&api).contains(&a.ca) && bundle(&api).contains(&b.ca));
            // A stale primary writes nothing.
            let start = api.requests().len();
            let no = || false;
            reconcile_webhook(&shared, &[], &retired, t0 + WEBHOOK_TRUST_GRACE, &no)
                .await
                .unwrap();
            assert!(api.requests()[start..].iter().all(|(m, _)| m == "GET"));
            // After the grace, old trust goes.
            let later = t0 + WEBHOOK_TRUST_GRACE;
            reconcile_webhook(&shared, std::slice::from_ref(&b), &retired, later, &yes)
                .await
                .unwrap();
            assert_eq!(bundle(&api), vec![b.ca.clone()]);
            // Steady state writes nothing.
            let start = api.requests().len();
            reconcile_webhook(&shared, &[b], &retired, later, &yes)
                .await
                .unwrap();
            assert!(api.requests()[start..].iter().all(|(m, _)| m == "GET"));
        });
    }

    #[test]
    fn webhook_trust_patched_into_every_webhook() {
        crate::kube::fake::run(|api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let mwc = paths::webhook_configuration();
            let slice = paths::endpoint_slice(&shared.config.namespace);
            api.put_object(&mwc, json!({"metadata":{"name":"racer-object-sidecar-injector"},"webhooks":[
                {"name":"a.racer","clientConfig":{"service":{"name":"racer-webhook"}}},
                {"name":"b.racer","clientConfig":{"service":{"name":"racer-webhook"},"caBundle":"c3RhbGU="}}]}));
            api.put_object(&slice, json!({"metadata":{"name":"racer-webhook"},"addressType":"IPv4","ports":[{"port":9443}],"endpoints":[]}));
            let seat = WebhookSeat {
                pod: "racer-1".into(),
                uid: "u1".into(),
                ips: vec!["10.0.0.1".parse().unwrap()],
                ca: crate::authority::keyring::tests::root(1),
            };
            let yes = || true;
            reconcile_webhook(
                &shared,
                std::slice::from_ref(&seat),
                &RetiredTrust::default(),
                Instant::now(),
                &yes,
            )
            .await
            .unwrap();
            let v = api.object(&mwc).unwrap();
            let first = v["webhooks"][0]["clientConfig"]["caBundle"].clone();
            assert_eq!(first, v["webhooks"][1]["clientConfig"]["caBundle"]);
            let pem = parse_pem(&STANDARD.decode(first.as_str().unwrap()).unwrap());
            assert!(pem.contains(&seat.ca));
        });
    }

    #[test]
    fn webhook_expiry_during_read_prevents_the_first_write() {
        crate::kube::fake::run(|api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let mwc = paths::webhook_configuration();
            api.put_object(
                &mwc,
                json!({
                    "metadata": {"name": "racer-object-sidecar-injector"},
                    "webhooks": [{"name": "a.racer", "clientConfig": {"service": {"name": "racer-webhook"}}}],
                }),
            );
            let seat = WebhookSeat {
                pod: "racer-1".into(),
                uid: "u1".into(),
                ips: vec!["10.0.0.1".parse().unwrap()],
                ca: crate::authority::keyring::tests::root(1),
            };
            let checks = Cell::new(0);
            let fresh = || {
                let before_read = checks.get() == 0;
                checks.set(checks.get() + 1);
                before_read
            };
            let start = api.requests().len();
            reconcile_webhook(
                &shared,
                &[seat],
                &RetiredTrust::default(),
                Instant::now(),
                &fresh,
            )
            .await
            .unwrap();
            assert_eq!(checks.get(), 2);
            assert!(
                api.requests()[start..]
                    .iter()
                    .all(|(method, _)| method == "GET"),
                "authority must be rechecked after the read, before adding trust",
            );
        });
    }

    #[test]
    fn webhook_endpoints_use_the_slice_family_of_dual_stack_seats() {
        crate::kube::fake::run(|api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let mwc = paths::webhook_configuration();
            let slice = paths::endpoint_slice(&shared.config.namespace);
            api.put_object(&mwc, json!({"metadata":{"name":"racer-object-sidecar-injector"},"webhooks":[{"name":"a.racer","clientConfig":{"service":{"name":"racer-webhook"}}}]}));
            // IPv6-primary Service; status.podIP of the seats is IPv4.
            api.put_object(&slice, json!({"metadata":{"name":"racer-webhook"},"addressType":"IPv6","ports":[{"port":9443}],"endpoints":[]}));
            let seat = |n: u8, published: Option<&str>| WebhookSeat {
                pod: format!("racer-{n}"),
                uid: format!("u{n}"),
                ips: webhook_ips(
                    published.map(str::to_owned).as_ref(),
                    format!("10.0.0.{n}").parse().unwrap(),
                ),
                ca: crate::authority::keyring::tests::root(1),
            };
            let seats = [
                seat(1, Some("10.0.0.1,fd00::1")),
                // An older seat publishes no webhook addresses.
                seat(2, None),
            ];
            reconcile_webhook(
                &shared,
                &seats,
                &RetiredTrust::default(),
                Instant::now(),
                &|| true,
            )
            .await
            .unwrap();
            let eps = api.object(&slice).unwrap()["endpoints"].clone();
            assert_eq!(eps.as_array().unwrap().len(), 1);
            assert_eq!(eps[0]["addresses"], json!(["fd00::1"]));
            assert_eq!(eps[0]["targetRef"]["name"], "racer-1");
        });
    }

    #[test]
    fn webhook_ips_are_one_per_family_with_endpoint_fallback() {
        let endpoint: IpAddr = "10.0.0.1".parse().unwrap();
        let ips = |v: Option<&str>| webhook_ips(v.map(str::to_owned).as_ref(), endpoint);
        let p = |v: &str| v.parse::<IpAddr>().unwrap();
        assert_eq!(ips(Some("10.0.0.1,fd00::1")), [p("10.0.0.1"), p("fd00::1")]);
        assert_eq!(ips(Some("fd00::1")), [p("fd00::1")]);
        for bad in [
            None,
            Some(""),
            Some("x"),
            Some("10.0.0.1,10.0.0.2"),
            Some("10.0.0.1,fd00::1,fd00::2"),
        ] {
            assert_eq!(ips(bad), [endpoint], "{bad:?}");
        }
    }

    #[test]
    fn pem_roundtrip() {
        let a = crate::authority::keyring::tests::root(1);
        let b = crate::authority::keyring::tests::root(1);
        let p = pem(&[a.clone(), b.clone()]);
        assert_eq!(parse_pem(p.as_bytes()), vec![a, b]);
        assert!(parse_pem(b"").is_empty());
    }

    #[test]
    fn renew_conflict_requires_same_term() {
        let term = Term {
            lease_uid: "l1".into(),
            transitions: 4,
            holder: "p1/00".into(),
        };
        let o = |uid: &str, transitions: u64, nonce: &str| {
            lease::observe(&json!({"metadata":{"name":"racer-primary","uid":uid,
                "annotations":{annotations::HOLDER_NONCE: nonce}},
                "spec":{"holderIdentity":"p1","leaseTransitions":transitions}}))
        };
        assert!(same_term(&o("l1", 4, "00"), "p1/00", &term));
        assert!(
            !same_term(&o("l2", 4, "00"), "p1/00", &term),
            "recreated Lease"
        );
        assert!(!same_term(&o("l1", 5, "00"), "p1/00", &term), "new claim");
        assert!(
            !same_term(&o("l1", 4, "01"), "p1/00", &term),
            "other process"
        );
    }
}

#[cfg(test)]
mod seat_ready_tests {
    use super::seat_ready;

    #[test]
    fn current_sequence_is_ready() {
        assert!(seat_ready(Some("5"), Some(5), None));
        assert!(!seat_ready(Some("4"), Some(5), None));
    }

    #[test]
    fn seats_lagging_a_fresh_commit_stay_ready() {
        assert!(seat_ready(Some("4"), Some(5), Some(4)));
        assert!(seat_ready(Some("5"), Some(5), Some(4)));
        assert!(!seat_ready(Some("3"), Some(5), Some(4)));
    }

    #[test]
    fn missing_malformed_or_future_ready_is_excluded() {
        assert!(!seat_ready(None, Some(5), Some(4)));
        assert!(!seat_ready(Some(""), Some(5), Some(4)));
        assert!(!seat_ready(Some("x"), Some(5), Some(4)));
        assert!(!seat_ready(Some("6"), Some(5), Some(4)));
        assert!(!seat_ready(Some("5"), None, Some(4)));
    }
}
