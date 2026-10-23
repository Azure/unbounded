//! Seat lifecycle on every Pod: primary Lease discovery, the seat lottery,
//! seat renewal, and following committed state.
//!
//! Ordinary Pods only watch the single `racer-primary` Lease with an exact
//! name field selector. They read a seat Lease only after a paced, jittered
//! delay when the primary reports a vacancy, or when no primary is live.

use crate::{
    DiscoveredEndpoint, DiscoveredEndpoints, Error, annotations,
    authority::{
        election::{self, LeaseObservation, Observer, SeatView},
        publication::{self, Image},
        state::{self, lease_authority},
    },
    exec::{self, Tcp},
    http::{self, Conn, TlsStream},
    kube::{KubeError, escape, paths, str_at},
    lease, limits,
    pki::{self, SeatIssuer, ServingIdentity},
    server,
    shared::{CachedIssuer, PrimaryView, Seat, Shared},
};
use base64::{Engine, engine::general_purpose::STANDARD};
use racer_control_wire as wire;
use std::{
    cell::RefCell,
    collections::BTreeMap,
    net::SocketAddr,
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant, SystemTime},
};

/// Interval between state Secret reads by a seat.
pub const FOLLOW_INTERVAL: Duration = Duration::from_secs(5);

/// Longest cold start claim window.
const MAX_COLD_WINDOW: u64 = 600;

impl PrimaryView {
    /// Whether `node` holds a seat that is kept. A seat at or beyond
    /// `desired` is being shrunk away (possibly ours, just released), so it
    /// does not make the Node seated.
    pub fn node_seated(&self, node: &str) -> bool {
        self.seat_nodes.iter().take(self.desired).any(|n| n == node)
    }

    /// Whether the primary Lease looks held and renewed.
    pub fn live(&self, changed_at: Option<Instant>, now: Instant) -> bool {
        self.holder.is_some()
            && changed_at.is_some_and(|t| now.duration_since(t) < limits::LEASE_DURATION)
    }
}

/// Parse the endpoints annotation.
pub fn parse_endpoints(value: Option<&String>) -> Vec<DiscoveredEndpoint> {
    let Some(v) = value else { return Vec::new() };
    let Ok(serde_json::Value::Array(items)) = serde_json::from_str::<serde_json::Value>(v) else {
        return Vec::new();
    };
    items
        .iter()
        .filter_map(|i| {
            Some(DiscoveredEndpoint {
                url: i["url"].as_str()?.to_owned(),
                certificate: STANDARD.decode(i["certificate"].as_str()?).ok()?,
                generation: i["generation"].as_str()?.to_owned(),
            })
        })
        .take(wire_codec::MAX_DISCOVERED_ENDPOINTS)
        .collect()
}

/// Encode the endpoints annotation.
pub fn encode_endpoints(endpoints: &[DiscoveredEndpoint]) -> String {
    let v: Vec<_> = endpoints
        .iter()
        .map(|e| serde_json::json!({"url": e.url, "certificate": STANDARD.encode(&e.certificate), "generation": e.generation}))
        .collect();
    serde_json::Value::Array(v).to_string()
}

/// Apply a primary Lease to the shared view and the endpoint list.
pub fn apply_primary(
    shared: &Shared,
    obs: &LeaseObservation,
    endpoints: &DiscoveredEndpoints,
    changed: &RefCell<(String, Option<Instant>)>,
) {
    let now = Instant::now();
    {
        let mut c = changed.borrow_mut();
        if c.0 != obs.resource_version {
            *c = (obs.resource_version.clone(), Some(now));
        }
    }
    let a = &obs.annotations;
    let desired = a
        .get(annotations::SEATS)
        .and_then(|v| v.parse().ok())
        .filter(|n| (1..=annotations::MAX_SEATS).contains(n));
    let endpoint = match (
        a.get(annotations::ENDPOINT),
        a.get(annotations::SERVING_CERTIFICATE),
    ) {
        (Some(u), Some(c)) => STANDARD.decode(c).ok().map(|c| (u.clone(), c)),
        _ => None,
    };
    let view = PrimaryView {
        holder: obs.spec.holder.clone(),
        desired: desired.unwrap_or_else(|| initial_seats(shared)),
        seat_nodes: a
            .get(annotations::SEAT_NODES)
            .and_then(|v| serde_json::from_str(v).ok())
            .unwrap_or_default(),
        claim_window: a
            .get(annotations::CLAIM_WINDOW)
            .and_then(|v| v.parse().ok())
            .unwrap_or(30),
        endpoint,
    };
    *shared.primary.borrow_mut() = view;
    let list = parse_endpoints(a.get(annotations::ENDPOINTS));
    let mut current = endpoints.write().unwrap_or_else(|e| e.into_inner());
    if *current != list {
        *current = list;
    }
    shared.changed.notify();
}

/// Seat count used before any primary has written one.
pub fn initial_seats(shared: &Shared) -> usize {
    match shared.config.seats {
        crate::SeatPolicy::Fixed(n) => n,
        crate::SeatPolicy::Auto => limits::MIN_AUTO_SEATS,
    }
}

fn jitter(shared: &Shared, salt: u32, max: Duration) -> Duration {
    let r = shared.rng.borrow_mut().u32_or_zero();
    election::claim_delay(&shared.config.pod_uid, salt ^ r, max)
}

/// Watch the primary Lease. One watch per Pod; no polling while it holds.
pub async fn discovery(
    shared: Rc<Shared>,
    endpoints: DiscoveredEndpoints,
    changed: Rc<RefCell<(String, Option<Instant>)>>,
) {
    let ns = shared.config.namespace.clone();
    let query = format!(
        "fieldSelector={}",
        escape(&format!("metadata.name={}", annotations::PRIMARY_LEASE))
    );
    let mut rv = String::new();
    let mut failures = 0u32;
    while !shared.stopping.get() {
        if rv.is_empty() {
            match lease::get(&shared.api, &ns, annotations::PRIMARY_LEASE).await {
                Ok(v) => {
                    let o = lease::observe(&v);
                    rv = o.resource_version.clone();
                    apply_primary(&shared, &o, &endpoints, &changed);
                    failures = 0;
                }
                Err(_) => {
                    failures = (failures + 1).min(6);
                    exec::sleep(
                        Duration::from_secs(1 << failures)
                            + jitter(&shared, failures, Duration::from_secs(5)),
                    )
                    .await;
                    continue;
                }
            }
        }
        let Ok(mut watch) = shared.api.watch(&paths::leases(&ns), &query, &rv).await else {
            rv.clear();
            exec::sleep(Duration::from_secs(2) + jitter(&shared, 7, Duration::from_secs(5))).await;
            continue;
        };
        loop {
            match watch.next().await {
                Ok(Some(e)) if e.kind == "BOOKMARK" => {
                    rv = str_at(&e.object, &["metadata", "resourceVersion"]).into()
                }
                Ok(Some(e)) if e.kind == "DELETED" => {
                    rv.clear();
                    break;
                }
                Ok(Some(e)) => {
                    let o = lease::observe(&e.object);
                    rv = o.resource_version.clone();
                    apply_primary(&shared, &o, &endpoints, &changed);
                }
                Ok(None) => break,
                Err(KubeError::Gone) | Err(_) => {
                    rv.clear();
                    break;
                }
            }
            if shared.stopping.get() {
                return;
            }
        }
    }
}

fn my_seat_annotations(shared: &Shared, seat: &Seat) -> BTreeMap<&'static str, Option<String>> {
    let issuer = seat.issuer.borrow();
    let control = seat.control.borrow();
    let webhook = seat.webhook.borrow();
    let ready = shared
        .ready()
        .then(|| {
            shared
                .serving
                .borrow()
                .record
                .as_ref()
                .map(|r| r.sequence.to_string())
        })
        .flatten();
    let mut a = BTreeMap::new();
    a.insert(annotations::ENDPOINT, Some(endpoint_url(shared)));
    a.insert(
        annotations::WEBHOOK_ADDRESSES,
        Some(webhook_addresses(shared)),
    );
    a.insert(
        annotations::SERVING_CERTIFICATE,
        Some(STANDARD.encode(&control.ca_der)),
    );
    a.insert(
        annotations::WEBHOOK_CERTIFICATE,
        Some(STANDARD.encode(&webhook.ca_der)),
    );
    let pending = seat.pending.borrow();
    let offered = pending.as_ref().map_or(issuer.der(), |p| p.der());
    if let Some(c) = shared.issuer.borrow_mut().as_mut()
        && Rc::ptr_eq(&c.issuer, &issuer)
    {
        c.offered = SystemTime::now();
    }
    a.insert(
        annotations::ISSUER_CERTIFICATE,
        Some(STANDARD.encode(offered)),
    );
    a.insert(annotations::READY, ready);
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
    a
}

/// `https://<pod ip>:<port>`.
pub fn endpoint_url(shared: &Shared) -> String {
    format!(
        "https://{}",
        SocketAddr::new(shared.config.pod_ip, shared.config.control_listen.port())
    )
}

/// IPs serving the webhook: the Pod IP and, on a dual-stack Pod, the Pod
/// IP of the other family. The webhook certificate is verified by Service
/// DNS name, so neither needs an IP SAN.
pub fn webhook_addresses(shared: &Shared) -> String {
    let c = &shared.config;
    let mut ips = vec![c.pod_ip.to_string()];
    ips.extend(c.webhook_alternate.map(|ip| ip.to_string()));
    ips.join(",")
}

fn webhook_names(ns: &str) -> Vec<String> {
    let s = annotations::WEBHOOK_ENDPOINT_SLICE;
    vec![
        format!("{s}.{ns}.svc"),
        format!("{s}.{ns}.svc.cluster.local"),
    ]
}

/// Whether a cached issuer may be offered by a new claim at `now`.
///
/// The keyring keeps a root at most one Lease duration plus one certificate
/// lifetime after its last offer and then retires it for good, so a root not
/// offered for a certificate lifetime is replaced. A root with less than half
/// its lifetime left is replaced too, well before the keyring refuses it.
pub fn issuer_reusable(cached: &CachedIssuer, cluster: &str, now: SystemTime) -> bool {
    cached.cluster == cluster
        && cached.issuer.not_after() >= now + pki::ROOT_LIFETIME / 2
        && now
            .duration_since(cached.offered)
            .is_ok_and(|idle| idle < wire::CERTIFICATE_LIFETIME)
}

/// The issuer for a new claim: the cached one when reusable, else a new one.
/// One issuer per process keeps the number of keyring roots bounded by the
/// number of Pods instead of the number of claims.
fn seat_issuer(shared: &Shared, now: SystemTime) -> Result<Rc<SeatIssuer>, Error> {
    let cluster = shared.cluster.borrow().clone();
    let mut cache = shared.issuer.borrow_mut();
    if let Some(c) = cache.as_ref()
        && issuer_reusable(c, &cluster, now)
    {
        return Ok(c.issuer.clone());
    }
    let issuer = Rc::new(
        SeatIssuer::generate(&cluster, now, pki::ROOT_LIFETIME).map_err(|_| Error::Unavailable)?,
    );
    *cache = Some(CachedIssuer {
        cluster,
        issuer: issuer.clone(),
        offered: now,
    });
    Ok(issuer)
}

/// Build seat material. The issuer is shared with earlier seats of this
/// process, see [`seat_issuer`]; serving identities are always fresh.
pub fn new_seat(shared: &Shared, index: usize) -> Result<Seat, Error> {
    let now = SystemTime::now();
    let issuer = seat_issuer(shared, now)?;
    let control = Rc::new(
        ServingIdentity::pinned(
            &[shared.config.pod_ip],
            &webhook_names(&shared.config.namespace),
            now,
            pki::SERVING_LIFETIME,
        )
        .map_err(|_| Error::Unavailable)?,
    );
    let webhook = control.clone();
    let control_tls = control
        .server_config(Some(shared.verifier.clone()))
        .map_err(|_| Error::Unavailable)?;
    let webhook_tls = webhook
        .server_config(None)
        .map_err(|_| Error::Unavailable)?;
    Ok(Seat {
        index,
        issuer: RefCell::new(issuer),
        pending: RefCell::new(None),
        control: RefCell::new(control),
        webhook: RefCell::new(webhook),
        control_tls: RefCell::new(control_tls),
        webhook_tls: RefCell::new(webhook_tls),
        lease: std::cell::Cell::new(state::Freshness::none()),
        lease_uid: RefCell::new(String::new()),
    })
}

fn claim_views(
    seen: &BTreeMap<usize, LeaseObservation>,
    observer: &Observer,
    candidate: usize,
    now: Instant,
) -> Vec<SeatView> {
    seen.iter()
        .filter_map(|(index, observation)| {
            let expired = observer.expired(observation, now);
            // Only the selected seat was just read. A stale cached vacancy
            // must not shadow it, but live observations still guard Node uniqueness.
            if *index != candidate && expired {
                return None;
            }
            Some(SeatView {
                index: *index,
                holder: lease::holder_key(observation),
                holder_node: observation
                    .annotations
                    .get(annotations::HOLDER_NODE)
                    .cloned(),
                expired,
            })
        })
        .collect()
}

/// Compete for a seat when one is vacant. Paced to keep API load low.
pub async fn lottery(shared: Rc<Shared>, primary_changed: Rc<RefCell<(String, Option<Instant>)>>) {
    let ns = shared.config.namespace.clone();
    let mut attempt = 0u32;
    let mut observer = Observer::default();
    let mut seen: BTreeMap<usize, LeaseObservation> = BTreeMap::new();
    while !shared.stopping.get() {
        if shared.seat.borrow().is_some() || shared.cluster.borrow().is_empty() {
            exec::sleep(Duration::from_secs(2)).await;
            continue;
        }
        let now = Instant::now();
        let view = shared.primary.borrow().clone();
        let live = view.live(primary_changed.borrow().1, now);
        let (candidates, window) = if live {
            if view.node_seated(&shared.config.node_name) {
                (Vec::new(), 0)
            } else {
                let v: Vec<usize> = (0..view.desired)
                    .filter(|i| view.seat_nodes.get(*i).is_none_or(String::is_empty))
                    .collect();
                (v, view.claim_window.clamp(5, 3600))
            }
        } else {
            (
                (0..initial_seats(&shared)).collect(),
                (5u64 << attempt.min(7)).min(MAX_COLD_WINDOW),
            )
        };
        if candidates.is_empty() {
            attempt = 0;
            exec::sleep(Duration::from_secs(10) + jitter(&shared, 1, Duration::from_secs(10)))
                .await;
            continue;
        }
        exec::sleep(jitter(&shared, attempt, Duration::from_secs(window))).await;
        if shared.seat.borrow().is_some() || shared.stopping.get() {
            continue;
        }
        let index = candidates[(jitter(&shared, 3, Duration::from_millis(1 << 20)).as_millis()
            as usize)
            % candidates.len()];
        attempt = attempt.saturating_add(1);
        let name = annotations::seat_lease(index);
        let Ok(object) = lease::get(&shared.lease_api, &ns, &name).await else {
            continue;
        };
        let obs = lease::observe(&object);
        observer.observe(&obs, Instant::now());
        seen.insert(index, obs.clone());
        let views = claim_views(&seen, &observer, index, Instant::now());
        let desired = if live {
            view.desired
        } else {
            initial_seats(&shared)
        };
        if election::seat_to_claim(&views, desired, &shared.config.node_name, &shared.identity)
            != Some(index)
        {
            continue;
        }
        let Ok(seat) = new_seat(&shared, index) else {
            continue;
        };
        let spec = election::claim(
            &obs.spec,
            &shared.config.pod_name,
            &election::now_micro(SystemTime::now()),
        );
        let started = Instant::now();
        let body = lease::with(&object, &spec, &my_seat_annotations(&shared, &seat));
        match lease::put(&shared.lease_api, &ns, &body).await {
            Ok(v) => {
                seat.lease.set(lease_authority(started));
                *seat.lease_uid.borrow_mut() = obs.uid.clone();
                let seat = Rc::new(seat);
                *shared.seat.borrow_mut() = Some(seat.clone());
                shared.changed.notify();
                attempt = 0;
                let s = shared.clone();
                let _ = exec::spawn(seat_task(s.clone(), seat.clone(), v));
                let _ = exec::spawn(follower(s.clone(), seat.clone()));
                let _ = exec::spawn(crate::primary::contend(s, seat));
            }
            Err(_) => continue,
        }
    }
}

/// Rotate the issuer root when due. The root is staged: offered, committed,
/// and only then used.
///
/// Serving identities are not rotated in place. Clients pin the control
/// certificate byte for byte and the webhook trusts it as its CA bundle, so
/// swapping it while the seat is advertised breaks every pinned client until
/// discovery catches up. Instead a seat whose serving identity is due drains
/// (see [`serving_rotation_due`]): it releases its Lease, the primary removes
/// its endpoint before its trust, and a fresh claim publishes new material
/// before it is ready.
fn rotate(shared: &Shared, seat: &Seat) {
    let now = SystemTime::now();
    if seat.issuer.borrow().should_rotate(now) && seat.pending.borrow().is_none() {
        let cluster = shared.cluster.borrow().clone();
        if let Ok(p) = SeatIssuer::generate(&cluster, now, pki::ROOT_LIFETIME) {
            *seat.pending.borrow_mut() = Some(Rc::new(p));
        }
    }
    // Switch issuers only after the new root is committed.
    let committed = {
        let serving = shared.serving.borrow();
        let pending = seat.pending.borrow();
        match (&serving.record, pending.as_ref()) {
            (Some(r), Some(p)) => r.keyring.root_committed(p.der()),
            _ => false,
        }
    };
    if committed && let Some(p) = seat.pending.borrow_mut().take() {
        // Later claims of this process reuse the rotated issuer.
        *shared.issuer.borrow_mut() = Some(CachedIssuer {
            cluster: shared.cluster.borrow().clone(),
            issuer: p.clone(),
            offered: now,
        });
        *seat.issuer.borrow_mut() = p;
    }
}

fn seat_still_mine(shared: &Shared, seat: &Rc<Seat>) -> bool {
    shared
        .seat
        .borrow()
        .as_ref()
        .is_some_and(|s| Rc::ptr_eq(s, seat))
}

/// Whether the seat must drain to replace its serving identity. Seats are
/// staggered by index so they do not all drain together; the last one still
/// drains well before expiry.
pub fn serving_rotation_due(identity: &ServingIdentity, index: usize, now: SystemTime) -> bool {
    let half = pki::SERVING_LIFETIME / 2;
    let step = half / (annotations::MAX_SEATS as u32 + 1);
    let remaining = half - step * (index.min(annotations::MAX_SEATS) as u32);
    identity.not_after < now + remaining
}

/// Whether the ready annotation in `object` differs from what this seat would
/// write now.
fn ready_changed(shared: &Shared, seat: &Seat, object: &serde_json::Value) -> bool {
    let written = lease::observe(object)
        .annotations
        .get(annotations::READY)
        .cloned();
    my_seat_annotations(shared, seat)
        .remove(annotations::READY)
        .flatten()
        != written
}

/// Renew the seat Lease until it is lost, shrunk away, or the runtime stops.
pub async fn seat_task(shared: Rc<Shared>, seat: Rc<Seat>, mut object: serde_json::Value) {
    let ns = shared.config.namespace.clone();
    let mut next = Instant::now() + limits::LEASE_RENEW;
    let mut last_put = Instant::now();
    loop {
        let generation = shared.changed.generation();
        let _ = exec::deadline(next, shared.changed.changed(generation)).await;
        if !seat_still_mine(&shared, &seat) {
            return;
        }
        let now = Instant::now();
        let view = shared.primary.borrow().clone();
        let stopping = shared.stopping.get();
        // Another live seat on this Node with a lower index wins.
        let duplicate = view
            .seat_nodes
            .iter()
            .enumerate()
            .any(|(i, n)| i < seat.index && *n == shared.config.node_name);
        let rotation = serving_rotation_due(&seat.control.borrow(), seat.index, SystemTime::now());
        let shrink = view.holder.is_some()
            && election::must_release(seat.index, view.desired)
            && !shared.is_primary.get();
        if stopping || duplicate || shrink || rotation {
            let spec = election::release(&lease::observe(&object).spec);
            let a: BTreeMap<_, _> = my_seat_annotations(&shared, &seat)
                .into_keys()
                .map(|k| (k, None))
                .collect();
            // Held across the clear so shutdown waits for the PUT.
            let _releasing = shared.releasing();
            shared.lose_seat();
            let _ = exec::timeout(
                limits::API_TIMEOUT,
                lease::put(&shared.lease_api, &ns, &lease::with(&object, &spec, &a)),
            )
            .await;
            return;
        }
        if now < next {
            // Woken by a change; refresh annotations only at renew time,
            // except readiness, which the primary needs to publish us. That
            // is written early, at most once a second so failures cannot spin.
            if ready_changed(&shared, &seat, &object) {
                next = next.min(last_put + Duration::from_secs(1));
            }
            if now < next {
                continue;
            }
        }
        rotate(&shared, &seat);
        let obs = lease::observe(&object);
        let spec = election::renew(&obs.spec, &election::now_micro(SystemTime::now()));
        let body = lease::with(&object, &spec, &my_seat_annotations(&shared, &seat));
        let started = Instant::now();
        last_put = started;
        match exec::timeout(
            limits::LEASE_RENEW,
            lease::put(&shared.lease_api, &ns, &body),
        )
        .await
        {
            Ok(Ok(v)) => {
                object = v;
                seat.lease.set(lease_authority(started));
                shared.changed.notify();
                next = started + limits::LEASE_RENEW;
            }
            Ok(Err(KubeError::Conflict)) => {
                match lease::get(&shared.lease_api, &ns, &obs.name).await {
                    Ok(v)
                        if crate::primary::same_term(
                            &lease::observe(&v),
                            &shared.identity,
                            &state::Term {
                                lease_uid: seat.lease_uid.borrow().clone(),
                                transitions: obs.spec.transitions,
                                holder: String::new(),
                            },
                        ) =>
                    {
                        object = v;
                        next = Instant::now();
                    }
                    Ok(_) => {
                        shared.lose_seat();
                        return;
                    }
                    Err(_) => next = Instant::now() + Duration::from_secs(2),
                }
            }
            _ => next = Instant::now() + Duration::from_secs(2),
        }
        if !seat.lease.get().fresh(Instant::now()) {
            shared.lose_seat();
            return;
        }
    }
}

/// Read the state Secret, keep serving state, and copy images from the primary.
pub async fn follower(shared: Rc<Shared>, seat: Rc<Seat>) {
    while seat_still_mine(&shared, &seat) && !shared.stopping.get() {
        let started = Instant::now();
        let _ = follow_once(&shared, started).await;
        let generation = shared.changed.generation();
        let _ = exec::timeout(FOLLOW_INTERVAL, shared.changed.changed(generation)).await;
        // Wake on every change, but read at most every second.
        exec::sleep_until(started + Duration::from_secs(1)).await;
    }
}

/// Read and install committed state once.
pub async fn follow_once(shared: &Shared, started: Instant) -> Result<(), Error> {
    let secret = shared
        .lease_api
        .get(&paths::state(&shared.config.namespace))
        .await?;
    let uid = str_at(&secret, &["metadata", "uid"]);
    if uid != shared.cluster.borrow().as_str() {
        fail_closed(shared);
        return Err(Error::InvalidState);
    }
    let initialized = !str_at(
        &secret,
        &["metadata", "annotations", annotations::INITIALIZED],
    )
    .is_empty();
    if initialized && !state_markers_match(&secret, uid) {
        fail_closed(shared);
        return Err(Error::InvalidState);
    }
    let data = secret["data"][annotations::STATE_KEY]
        .as_str()
        .map(|s| STANDARD.decode(s));
    let data = match data {
        Some(Ok(d)) => Some(d),
        Some(Err(_)) => {
            fail_closed(shared);
            return Err(Error::InvalidState);
        }
        None => None,
    };
    let record = match state::decode(uid, initialized, data.as_deref()) {
        Ok(Some(r)) => r,
        // Uninitialized after this process accepted state: wiped.
        Ok(None) if shared.serving.borrow().high_water.is_some() => {
            fail_closed(shared);
            return Err(Error::InvalidState);
        }
        Ok(None) => return Ok(()),
        Err(_) => {
            fail_closed(shared);
            return Err(Error::InvalidState);
        }
    };
    install_record(shared, record, started).map_err(|_| Error::InvalidState)?;
    let need_image = {
        let s = shared.serving.borrow();
        let Some(r) = s.record.as_ref() else {
            return Err(Error::InvalidState);
        };
        r.sequence != 0
            && s.image.as_ref().is_none_or(|i| {
                i.record.sequence != r.sequence || i.record.content_hash != r.content_hash
            })
    };
    if need_image && !shared.is_primary.get() {
        fetch_image(shared).await?;
    }
    Ok(())
}

/// Initialized markers and `data.cluster` must all equal the Secret UID.
pub fn state_markers_match(secret: &serde_json::Value, uid: &str) -> bool {
    let a = |k: &str| str_at(secret, &["metadata", "annotations", k]).to_owned();
    let cluster = secret["data"][annotations::CLUSTER_KEY]
        .as_str()
        .and_then(|v| STANDARD.decode(v).ok())
        .and_then(|v| String::from_utf8(v).ok());
    a(annotations::INITIALIZED) == uid
        && a(annotations::CLUSTER_ID) == uid
        && cluster.as_deref() == Some(uid)
}

/// Stop serving: state is missing or invalid.
pub fn fail_closed(shared: &Shared) {
    let mut s = shared.serving.borrow_mut();
    s.record = None;
    s.confirmed = None;
    s.image = None;
    s.bundle = None;
    shared.last_error.set(Some(Error::InvalidState));
    drop(s);
    shared.changed.notify();
}

/// Install a committed record confirmed at `confirmed`.
#[must_use = "a refused record means the runtime failed closed"]
pub fn install_record(
    shared: &Shared,
    record: state::StateRecord,
    confirmed: Instant,
) -> Result<(), state::StateError> {
    let cluster = shared.cluster.borrow().clone();
    let mut s = shared.serving.borrow_mut();
    if s.high_water
        .as_ref()
        .is_some_and(|old| state::check_monotonic(old, &record).is_err())
    {
        // A rollback is never accepted.
        drop(s);
        fail_closed(shared);
        return Err(state::StateError::Rollback);
    }
    let generation = record.keyring.generation;
    if s.bundle.as_ref().is_none_or(|(g, _)| *g != generation)
        || s.record
            .as_ref()
            .is_none_or(|r| r.keyring != record.keyring)
    {
        s.bundle = record
            .keyring
            .bundle(&cluster)
            .ok()
            .filter(|b| !b.peer_trust_roots.is_empty() && generation != 0)
            .and_then(|b| racer_control_wire::encode_bundle(&b).ok())
            .map(|bytes| (generation, Arc::new(bytes)));
        s.roots = record.keyring.root_ders();
        shared.verifier.set_roots(&s.roots);
    }
    if s.image
        .as_ref()
        .is_some_and(|i| i.record.sequence != record.sequence)
        && shared.is_primary.get()
    {
        // The primary installs images itself on commit.
    }
    s.high_water = Some(record.clone());
    s.record = Some(record);
    s.confirmed = Some(confirmed);
    drop(s);
    shared.changed.notify();
    Ok(())
}

/// Copy the committed image from the primary and verify it.
async fn fetch_image(shared: &Shared) -> Result<(), Error> {
    let (url, cert) = shared
        .primary
        .borrow()
        .endpoint
        .clone()
        .ok_or(Error::Unavailable)?;
    let address: SocketAddr = url
        .strip_prefix("https://")
        .ok_or(Error::Unavailable)?
        .parse()
        .map_err(|_| Error::Unavailable)?;
    let token =
        std::fs::read_to_string(&shared.config.control_token).map_err(|_| Error::Unavailable)?;
    let token = zeroize::Zeroizing::new(token.trim().to_owned());
    let tls = server::pinned_client(&cert)?;
    let body = exec::timeout(Duration::from_secs(60), async {
        let tcp = Tcp::connect(address)
            .await
            .map_err(|_| Error::Unavailable)?;
        let name = rustls::pki_types::ServerName::IpAddress(address.ip().into());
        let mut conn = Conn::new(
            TlsStream::client(tcp, tls, name)
                .await
                .map_err(|_| Error::Unavailable)?,
        );
        let bearer = zeroize::Zeroizing::new(format!("Bearer {}", token.as_str()));
        let request = zeroize::Zeroizing::new(http::request(
            "GET",
            &address.to_string(),
            server::REPLICA_PATH,
            &[("Authorization", &bearer)],
            &[],
        ));
        conn.tls
            .write_all(&request)
            .await
            .map_err(|_| Error::Unavailable)?;
        let head = conn
            .read_head(false, limits::MAX_HEAD_BYTES)
            .await
            .map_err(|_| Error::Unavailable)?
            .ok_or(Error::Unavailable)?;
        if head.status != 200 {
            return Err(Error::Unavailable);
        }
        conn.read_response_body(&head, racer_control_wire::MAX_PUBLICATION_BYTES)
            .await
            .map_err(|_| Error::Unavailable)
    })
    .await??;
    let cluster = shared.cluster.borrow().clone();
    let record = shared
        .serving
        .borrow()
        .record
        .clone()
        .ok_or(Error::Unavailable)?;
    let committed = publication::Record {
        sequence: record.sequence,
        membership_version: record.membership_version,
        content_hash: record.content_hash.clone(),
        membership_hash: record.membership_hash.clone(),
    };
    let p =
        publication::verify_image(&body, &cluster, &committed).map_err(|_| Error::Unavailable)?;
    let mut s = shared.serving.borrow_mut();
    if s.record
        .as_ref()
        .is_none_or(|r| r.sequence != committed.sequence)
    {
        return Err(Error::Unavailable);
    }
    let image = Image::new(committed, p, body, s.image.as_ref());
    s.image = Some(image);
    drop(s);
    shared.changed.notify();
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        authority::keyring::{self, Durable, MAX_ROOTS, Offer},
        kube::fake,
    };

    fn unix(t: SystemTime) -> u64 {
        t.duration_since(std::time::UNIX_EPOCH).unwrap().as_secs()
    }

    /// Plan the keyring with one offer of `der` at `now`.
    fn offer(d: &Durable, der: &[u8], seat: usize, now: SystemTime) -> Durable {
        let offers = [Offer {
            der: der.to_vec(),
            seat: seat as u8,
            holder: "p1".into(),
        }];
        let p = keyring::plan(&keyring::tests::policy(), d, &[], &offers, unix(now)).unwrap();
        assert_eq!(p.refused_roots, 0);
        p.durable
    }

    #[test]
    fn reclaims_reuse_the_issuer_root() {
        fake::run(|_, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let first = new_seat(&shared, 0).unwrap();
            let der = first.issuer.borrow().der().to_vec();
            let mut d = offer(&Durable::default(), &der, 0, SystemTime::now());
            drop(first);
            // Reclaim of the same and of another index offers the same root.
            for index in [0, 3] {
                let seat = new_seat(&shared, index).unwrap();
                assert_eq!(seat.issuer.borrow().der(), der.as_slice());
                // The cached issuer is the one in use.
                assert!(Rc::ptr_eq(
                    &shared.issuer.borrow().as_ref().unwrap().issuer,
                    &seat.issuer.borrow()
                ));
                d = offer(&d, seat.issuer.borrow().der(), index, SystemTime::now());
            }
            assert_eq!(d.roots.len(), 1);
            assert!(d.root_committed(&der));
        });
    }

    #[test]
    fn issuer_is_replaced_near_expiry_idle_or_on_cluster_change() {
        let now = SystemTime::now();
        let cluster = "c1c1c1c1-0000-4000-8000-000000000001";
        let cached = |at: SystemTime, offered: SystemTime| CachedIssuer {
            cluster: cluster.into(),
            issuer: Rc::new(SeatIssuer::generate(cluster, at, pki::ROOT_LIFETIME).unwrap()),
            offered,
        };
        let fresh = cached(now, now);
        assert!(issuer_reusable(&fresh, cluster, now));
        assert!(!issuer_reusable(
            &fresh,
            "c2c2c2c2-0000-4000-8000-000000000002",
            now
        ));
        // Less than half the root lifetime left.
        let old = cached(now - pki::ROOT_LIFETIME / 2 - Duration::from_secs(60), now);
        assert!(!issuer_reusable(&old, cluster, now));
        // Not offered for a certificate lifetime: the keyring may retire it.
        let idle = cached(now, now - wire::CERTIFICATE_LIFETIME);
        assert!(!issuer_reusable(&idle, cluster, now));

        fake::run(|_, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let der = new_seat(&shared, 0).unwrap().issuer.borrow().der().to_vec();
            shared.issuer.borrow_mut().as_mut().unwrap().issuer = Rc::new(
                SeatIssuer::generate(
                    &shared.cluster.borrow(),
                    SystemTime::now() - pki::ROOT_LIFETIME,
                    pki::ROOT_LIFETIME + Duration::from_secs(3600),
                )
                .unwrap(),
            );
            let seat = new_seat(&shared, 0).unwrap();
            let renewed = seat.issuer.borrow().der().to_vec();
            assert_ne!(renewed, der);
            assert!(issuer_reusable(
                shared.issuer.borrow().as_ref().unwrap(),
                &shared.cluster.borrow(),
                SystemTime::now()
            ));
        });
    }

    #[test]
    fn claim_churn_does_not_exhaust_roots() {
        fake::run(|_, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let mut d = Durable::default();
            let start = SystemTime::now();
            for i in 0..(MAX_ROOTS * 3) {
                let seat = new_seat(&shared, i % annotations::MAX_SEATS).unwrap();
                // Offered while held, as the Lease annotation would.
                let _ = my_seat_annotations(&shared, &seat);
                let at = start + Duration::from_secs(60 * i as u64);
                d = offer(&d, seat.issuer.borrow().der(), seat.index, at);
                assert!(d.root_committed(seat.issuer.borrow().der()));
            }
            assert_eq!(d.roots.len(), 1);
        });
    }

    fn observed_seat(index: usize, node: &str) -> LeaseObservation {
        LeaseObservation {
            name: annotations::seat_lease(index),
            resource_version: format!("{index}-old"),
            spec: election::LeaseSpec {
                holder: Some(format!("old-pod-{index}")),
                duration_secs: limits::LEASE_DURATION.as_secs() as u32,
                ..Default::default()
            },
            annotations: BTreeMap::from([(annotations::HOLDER_NODE.into(), node.into())]),
            ..Default::default()
        }
    }

    #[test]
    fn stale_unselected_observations_do_not_block_a_vacant_seat() {
        let seen = BTreeMap::from([
            (0, observed_seat(0, "other-node")),
            (1, observed_seat(1, "my-node")),
        ]);
        let mut observer = Observer::default();
        let start = Instant::now();
        for observation in seen.values() {
            observer.observe(observation, start);
        }
        let now = start + limits::LEASE_DURATION;
        let views = claim_views(&seen, &observer, 1, now);
        assert_eq!(
            election::seat_to_claim(&views, 3, "my-node", "new-pod/nonce"),
            Some(1)
        );
    }

    #[test]
    fn selected_seat_checks_keep_live_same_node_protection() {
        let mut seen = BTreeMap::from([
            (0, observed_seat(0, "my-node")),
            (1, observed_seat(1, "my-node")),
        ]);
        let mut observer = Observer::default();
        let start = Instant::now();
        for observation in seen.values() {
            observer.observe(observation, start);
        }
        let now = start + limits::LEASE_DURATION;
        seen.get_mut(&0).unwrap().resource_version = "0-fresh".into();
        observer.observe(&seen[&0], now);
        let views = claim_views(&seen, &observer, 1, now);
        assert_eq!(
            election::seat_to_claim(&views, 3, "my-node", "new-pod/nonce"),
            None
        );
        let views = claim_views(&seen, &observer, 0, now);
        assert_eq!(
            election::seat_to_claim(&views, 3, "other-node", "new-pod/nonce"),
            None
        );
    }

    #[test]
    fn node_seated_ignores_seats_being_shrunk_away() {
        let view = PrimaryView {
            desired: 2,
            seat_nodes: vec!["a".into(), String::new(), "b".into()],
            ..Default::default()
        };
        assert!(view.node_seated("a"));
        // Seat 2 is beyond desired: its Node may claim a kept seat.
        assert!(!view.node_seated("b"));
        assert!(!view.node_seated("c"));
        let none = PrimaryView { desired: 0, ..view };
        assert!(!none.node_seated("a"));
    }

    #[test]
    fn endpoints_annotation_roundtrip() {
        let e = vec![DiscoveredEndpoint {
            url: "https://10.0.0.1:8443".into(),
            certificate: vec![1, 2, 3],
            generation: "0-1-ab".into(),
        }];
        let s = encode_endpoints(&e);
        assert_eq!(parse_endpoints(Some(&s)), e);
        assert!(parse_endpoints(Some(&"x".to_owned())).is_empty());
        assert!(parse_endpoints(None).is_empty());
    }

    #[test]
    fn primary_liveness() {
        let v = PrimaryView {
            holder: Some("p/0000000000000000".into()),
            ..Default::default()
        };
        let t = Instant::now();
        assert!(v.live(Some(t), t));
        assert!(!v.live(Some(t), t + limits::LEASE_DURATION));
        assert!(!v.live(None, t));
        assert!(!PrimaryView::default().live(Some(t), t));
    }

    fn record(sequence: u64) -> state::StateRecord {
        let hash = |c: char| {
            if sequence == 0 {
                String::new()
            } else {
                c.to_string().repeat(64)
            }
        };
        state::StateRecord {
            version: state::VERSION,
            cluster: "c1c1c1c1-0000-4000-8000-000000000001".into(),
            term: state::Term {
                lease_uid: "l".into(),
                transitions: 1,
                holder: "p/00".into(),
            },
            writer_pod_uid: "u".into(),
            sequence,
            membership_version: sequence,
            content_hash: hash(char::from(b'a' + sequence as u8)),
            membership_hash: hash('b'),
            ..Default::default()
        }
    }

    #[test]
    fn rollback_refused_after_fail_closed() {
        crate::kube::fake::run(|_api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            install_record(&shared, record(5), Instant::now()).unwrap();
            assert_eq!(shared.serving.borrow().record.as_ref().unwrap().sequence, 5);
            fail_closed(&shared);
            // An older record must not be accepted just because the current
            // one was cleared.
            assert_eq!(
                install_record(&shared, record(4), Instant::now()),
                Err(state::StateError::Rollback)
            );
            assert!(shared.serving.borrow().record.is_none());
            install_record(&shared, record(6), Instant::now()).unwrap();
            assert_eq!(shared.serving.borrow().record.as_ref().unwrap().sequence, 6);
        });
    }

    #[test]
    fn wiped_markers_after_accepting_state_fail_closed() {
        crate::kube::fake::run(|api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let uid = shared.cluster.borrow().clone();
            let path = paths::state(&shared.config.namespace);
            let r = record(3);
            api.put_object(
                &path,
                serde_json::json!({"metadata": {"name": "racer-state", "uid": uid,
                    "annotations": {annotations::INITIALIZED: uid, annotations::CLUSTER_ID: uid}},
                    "data": {annotations::CLUSTER_KEY: STANDARD.encode(&uid),
                        annotations::STATE_KEY: STANDARD.encode(state::encode(&r).unwrap())}}),
            );
            shared.is_primary.set(true);
            follow_once(&shared, Instant::now()).await.unwrap();
            assert_eq!(shared.serving.borrow().record.as_ref().unwrap().sequence, 3);
            // Markers and data removed on the same Secret: not a fresh install.
            api.put_object(
                &path,
                serde_json::json!({"metadata": {"name": "racer-state", "uid": uid}}),
            );
            assert!(follow_once(&shared, Instant::now()).await.is_err());
            assert!(shared.serving.borrow().record.is_none());
        });
    }

    #[test]
    fn serving_rotation_drains_staggered_before_expiry() {
        let t = SystemTime::now();
        let id = ServingIdentity::pinned(
            &["127.0.0.1".parse().unwrap()],
            &[],
            t,
            pki::SERVING_LIFETIME,
        )
        .unwrap();
        assert!(!serving_rotation_due(&id, 0, t));
        let mid = t + pki::SERVING_LIFETIME / 2 + Duration::from_secs(60);
        assert!(serving_rotation_due(&id, 0, mid));
        assert!(
            !serving_rotation_due(&id, 3, mid),
            "later seats drain later"
        );
        let late = t + pki::SERVING_LIFETIME - Duration::from_secs(3600);
        assert!((0..annotations::MAX_SEATS).all(|i| serving_rotation_due(&id, i, late)));
    }

    #[test]
    fn follower_rollback_fails_closed_without_panic() {
        crate::kube::fake::run(|api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let uid = shared.cluster.borrow().clone();
            let path = paths::state(&shared.config.namespace);
            let put = |r: &state::StateRecord| {
                api.put_object(
                    &path,
                    serde_json::json!({"metadata": {"name": "racer-state", "uid": uid,
                        "annotations": {annotations::INITIALIZED: uid, annotations::CLUSTER_ID: uid}},
                        "data": {annotations::CLUSTER_KEY: STANDARD.encode(&uid),
                            annotations::STATE_KEY: STANDARD.encode(state::encode(r).unwrap())}}),
                );
            };
            shared.is_primary.set(true);
            put(&record(5));
            follow_once(&shared, Instant::now()).await.unwrap();
            put(&record(4));
            assert!(follow_once(&shared, Instant::now()).await.is_err());
            assert!(shared.serving.borrow().record.is_none());
            assert!(
                follow_once(&shared, Instant::now()).await.is_err(),
                "stays closed"
            );
        });
    }
}
