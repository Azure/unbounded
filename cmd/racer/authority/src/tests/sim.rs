//! End-to-end tests on the simulated driver: several [`Authority`] roles and
//! the fake Kubernetes API share one simulated network and disk and are
//! stepped on this thread. Each Pod has its own virtual clock; the world
//! moves reference time only when nothing is runnable. No real sockets,
//! threads, or sleeps.

use super::{NS, SECRET_UID, call, node_uid, seed, token};
use crate::{
    Authority, Config, Next, Role, SeatPolicy, annotations,
    config::Rotation,
    kube::{KubeConfig, fake, str_at},
    rt::{self, Executor},
    rules::state::Term,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use racer_io::{ctx::VirtualClock, ring::SocketAddress, sim::SimOs};
use racer_wire::control as wire;
use serde_json::{Value, json};
use std::{
    cell::RefCell,
    future::Future,
    net::SocketAddr,
    path::Path,
    rc::Rc,
    sync::{Arc, atomic::AtomicU64},
    time::{Duration, Instant, SystemTime},
};

/// Fake API server address on the simulated network.
const API: &str = "10.0.0.1:443";

/// Control API port of every Pod.
const CONTROL_PORT: u16 = 8443;

/// Webhook port of every Pod.
const WEBHOOK_PORT: u16 = 9443;

fn pod_ip(i: usize) -> std::net::IpAddr {
    format!("10.0.1.{i}").parse().unwrap()
}

/// The endpoint label of Pod `i`, for partitions.
fn pod_endpoint(i: usize) -> SocketAddress {
    SocketAddress::Inet(SocketAddr::new(pod_ip(i), CONTROL_PORT))
}

fn api_endpoint() -> SocketAddress {
    SocketAddress::Inet(API.parse().unwrap())
}

fn state_path() -> String {
    format!("/api/v1/namespaces/{NS}/secrets/racer-state")
}

fn lease_path(name: &str) -> String {
    format!("/apis/coordination.k8s.io/v1/namespaces/{NS}/leases/{name}")
}

struct Node {
    clock: VirtualClock,
    authority: Option<Authority>,
}

/// A simulated cluster: the fake API on its own executor and one
/// [`Authority`] per started Pod, all on this thread.
pub(super) struct World {
    os: SimOs,
    origin: Instant,
    wall: SystemTime,
    api_clock: VirtualClock,
    api_ex: Executor,
    pub api: fake::FakeApi,
    kube: KubeConfig,
    /// Pod `i` is `nodes[i - 1]`.
    nodes: Vec<Node>,
    seed: u64,
    /// Primary terms seen, with the reference time each was first seen.
    pub terms: Vec<(Duration, usize, Term)>,
    /// Steps since reference time last moved, to catch a livelock.
    spins: usize,
}

/// Steps without time moving before the world reports a livelock.
const MAX_SPINS: usize = 200_000;

impl World {
    pub fn new(pods: usize) -> Self {
        Self::with(pods, 1, |_| {})
    }

    /// `pods` Pods seeded in the fake API, none started; `setup` adjusts the
    /// seeded API.
    pub fn with(pods: usize, seed_value: u64, setup: impl FnOnce(&fake::FakeApi)) -> Self {
        let os = SimOs::seeded(seed_value);
        let origin = Instant::now();
        // Near real wall time, so test tokens (`exp` one hour out) hold.
        let wall = SystemTime::now();
        let api_clock = VirtualClock::new_at(origin, wall);
        let api_ex = Executor::simulated(os.clone(), &api_clock, seed_value, 4096).unwrap();
        let (api, kube) = api_ex.enter(|| {
            let api = fake::FakeApi::new();
            seed(&api, pods);
            setup(&api);
            let kube = fake::serve_at(api.clone(), Path::new("/api"), API.parse().unwrap());
            (api, kube)
        });
        let nodes = (0..pods)
            .map(|_| Node {
                clock: VirtualClock::new_at(origin, wall),
                authority: None,
            })
            .collect();
        Self {
            os,
            origin,
            wall,
            api_clock,
            api_ex,
            api,
            kube,
            nodes,
            seed: seed_value,
            terms: Vec::new(),
            spins: 0,
        }
    }

    /// Configuration of Pod `i`, with its control token on the simulated disk.
    pub fn config(&self, i: usize) -> Config {
        let token_path = format!("/pods/{i}/control-token");
        self.os
            .write_file(Path::new(&token_path), token(i).as_bytes())
            .unwrap();
        Config {
            namespace: NS.into(),
            pod_name: format!("p{i}"),
            pod_uid: super::pod_uid(i),
            node_name: format!("n{i}"),
            pod_ip: pod_ip(i),
            seats: SeatPolicy::Auto,
            control_listen: SocketAddr::new(pod_ip(i), CONTROL_PORT),
            webhook_listen: SocketAddr::new(pod_ip(i), WEBHOOK_PORT),
            webhook_alternate: None,
            object_image: Some("example.com/racer-object:1".into()),
            api_server: self.kube.server.clone(),
            api_ca: self.kube.ca.clone(),
            api_token: self.kube.token.clone(),
            control_token: token_path.into(),
            daemonset: "racer".into(),
            service_account: "racer".into(),
            peer_port: 8082,
            page_size: racer_wire::object::PageSize::DEFAULT,
            rotation: Rotation::default(),
        }
    }

    /// Pod `i`'s clock. Its rate may be changed to skew it.
    pub fn clock(&self, i: usize) -> &VirtualClock {
        &self.nodes[i - 1].clock
    }

    /// Start Pod `i` with `config`.
    pub fn start_with(&mut self, i: usize, config: Config) {
        let node = &mut self.nodes[i - 1];
        let _label = self.os.enter_endpoint(pod_endpoint(i));
        let seed = self.seed.wrapping_mul(1000) + i as u64;
        node.authority =
            Some(Authority::simulated(config, self.os.clone(), &node.clock, seed).unwrap());
    }

    pub fn start(&mut self, i: usize) {
        let config = self.config(i);
        self.start_with(i, config);
    }

    pub fn authority(&self, i: usize) -> &Authority {
        self.nodes[i - 1].authority.as_ref().expect("started")
    }

    pub fn authority_mut(&mut self, i: usize) -> &mut Authority {
        self.nodes[i - 1].authority.as_mut().expect("started")
    }

    pub fn running(&self, i: usize) -> bool {
        self.nodes[i - 1].authority.is_some()
    }

    pub fn role(&self, i: usize) -> Role {
        self.authority(i).diagnostics().role
    }

    pub fn ready(&self, i: usize) -> bool {
        self.authority(i).diagnostics().ready
    }

    pub fn endpoints(&self, i: usize) -> usize {
        self.authority(i).endpoints().read().unwrap().len()
    }

    /// Reference (true) time since the world began.
    pub fn now(&self) -> Duration {
        self.api_clock.reference_elapsed()
    }

    pub fn partition(&self, i: usize) {
        self.os.partition(pod_endpoint(i), api_endpoint());
    }

    pub fn heal(&self, i: usize) {
        self.os.heal(pod_endpoint(i), api_endpoint());
    }

    /// Stop Pod `i` and step the world until it finished. Returns the time
    /// it took on the Pod's clock.
    pub fn stop(&mut self, i: usize) -> Duration {
        let started = self.clock(i).now();
        self.authority_mut(i).stop();
        assert!(
            self.run_until(Duration::from_secs(60), |w| !w.running(i)),
            "p{i} did not stop"
        );
        self.clock(i).now() - started
    }

    /// The Pods acting as primary now, with their terms. Checked after every
    /// step: more than one is a split brain.
    fn primaries(&self) -> Vec<(usize, Term)> {
        self.nodes
            .iter()
            .enumerate()
            .filter_map(|(n, node)| Some((n + 1, node.authority.as_ref()?.primary_term()?)))
            .collect()
    }

    fn check(&mut self) {
        let primaries = self.primaries();
        assert!(
            primaries.len() <= 1,
            "two primaries at {:?}: {primaries:?}",
            self.now()
        );
        for (i, term) in primaries {
            if !self.terms.iter().any(|(_, j, t)| *j == i && *t == term) {
                assert!(
                    !self.terms.iter().any(|(_, _, t)| *t == term),
                    "term {term:?} led twice"
                );
                let now = self.now();
                self.terms.push((now, i, term));
            }
        }
    }

    /// Whether a role reporting `next` may be stepped at `now`.
    fn runnable(next: &Next, now: Instant) -> bool {
        match next {
            Next::Ready => true,
            Next::WaitUntil(at) => *at <= now,
            Next::Idle | Next::Done => false,
        }
    }

    /// One scheduling round: step every runnable role once. With nothing
    /// runnable, move all clocks to the earliest deadline. Returns false
    /// when nothing could ever run again.
    pub fn step(&mut self) -> bool {
        let mut stepped = false;
        if Self::runnable(&self.api_ex.next(), self.api_clock.now()) {
            let _ = self.api_ex.turn();
            stepped = true;
        }
        for n in 0..self.nodes.len() {
            let node = &mut self.nodes[n];
            let Some(authority) = node.authority.as_mut() else {
                continue;
            };
            if !Self::runnable(&authority.next(), node.clock.now()) {
                continue;
            }
            let next = {
                let _label = self.os.enter_endpoint(pod_endpoint(n + 1));
                authority.turn()
            };
            if matches!(next, Next::Done) {
                node.authority = None;
            }
            stepped = true;
            self.check();
        }
        if stepped {
            self.spins += 1;
            if self.spins > MAX_SPINS {
                let nexts: Vec<_> = self
                    .nodes
                    .iter()
                    .map(|n| n.authority.as_ref().map(|a| a.next()))
                    .collect();
                panic!(
                    "livelock at {:?}: api {:?}, pods {nexts:?}",
                    self.now(),
                    self.api_ex.next()
                );
            }
            return true;
        }
        self.spins = 0;
        let mut wake: Option<Duration> = None;
        let mut consider = |next: Next, clock: &VirtualClock| {
            if let Next::WaitUntil(at) = next
                && let Some(r) = clock.reference_at(at)
            {
                wake = Some(wake.map_or(r, |w: Duration| w.min(r)));
            }
        };
        consider(self.api_ex.next(), &self.api_clock);
        for node in &self.nodes {
            if let Some(a) = &node.authority {
                consider(a.next(), &node.clock);
            }
        }
        let Some(at) = wake else {
            return false;
        };
        let at = at.max(self.now());
        self.api_clock.advance_reference_to(at).unwrap();
        for node in &self.nodes {
            node.clock.advance_reference_to(at).unwrap();
        }
        true
    }

    /// Step until `done` holds or `limit` of reference time passes.
    pub fn run_until(&mut self, limit: Duration, mut done: impl FnMut(&mut Self) -> bool) -> bool {
        let end = self.now() + limit;
        loop {
            if done(self) {
                return true;
            }
            if self.now() > end || !self.step() {
                return done(self);
            }
        }
    }

    /// Step for `duration` of reference time.
    pub fn run_for(&mut self, duration: Duration) {
        let end = self.now() + duration;
        self.run_until(duration + Duration::from_secs(1), |w| w.now() >= end);
    }

    /// Run `future` as a client on the API executor's network and clock.
    pub fn client<T: 'static>(&mut self, future: impl Future<Output = T> + 'static) -> T {
        let out = Rc::new(RefCell::new(None));
        let o = out.clone();
        self.api_ex
            .enter(|| {
                rt::spawn(async move {
                    *o.borrow_mut() = Some(future.await);
                })
            })
            .unwrap();
        assert!(
            self.run_until(Duration::from_secs(120), |_| out.borrow().is_some()),
            "client did not finish"
        );
        out.borrow_mut().take().unwrap()
    }

    pub fn record(&self) -> Value {
        let secret = self.api.object(&state_path()).unwrap();
        serde_json::from_slice(
            &STANDARD
                .decode(secret["data"]["state"].as_str().unwrap())
                .unwrap(),
        )
        .unwrap()
    }

    pub fn has_record(&self) -> bool {
        self.api
            .object(&state_path())
            .is_some_and(|s| s["data"].get("state").is_some())
    }

    pub fn holder(&self, name: &str) -> String {
        self.api
            .object(&lease_path(name))
            .map(|l| str_at(&l, &["spec", "holderIdentity"]).to_owned())
            .unwrap_or_default()
    }

    pub fn primary_pods(&self) -> Vec<usize> {
        (1..=self.nodes.len())
            .filter(|&i| self.running(i) && matches!(self.role(i), Role::Primary(_)))
            .collect()
    }

    #[allow(dead_code)]
    pub fn anchors(&self) -> (Instant, SystemTime) {
        (self.origin, self.wall)
    }
}

fn fixed(world: &World, i: usize, seats: usize) -> Config {
    let mut c = world.config(i);
    c.seats = SeatPolicy::Fixed(seats);
    c
}

/// Start Pod 1 alone holding seat 0 and the primary, and wait until ready.
fn ready_single(world: &mut World) {
    let c = fixed(world, 1, 1);
    world.start_with(1, c);
    assert!(world.run_until(Duration::from_secs(60), |w| w.ready(1)
        && matches!(w.role(1), Role::Primary(0))));
}

#[test]
fn cold_start_elects_commits_serves_and_fails_closed() {
    let mut w = World::new(2);
    w.start(1);
    w.start(2);
    assert!(w.run_until(Duration::from_secs(30), |w| {
        w.authority(1).bootstrap().is_some()
    }));
    let b = w.authority(1).bootstrap().unwrap().unwrap();
    assert_eq!(b.cluster.0, SECRET_UID);
    assert_eq!(b.block_devices.as_deref(), Some("nvme-.*"));

    // Both Pods take seats on distinct Nodes; one becomes primary; both get
    // ready and are published as endpoints.
    assert!(w.run_until(Duration::from_secs(60), |w| w.endpoints(1) == 2
        && w.endpoints(2) == 2));
    assert_eq!(w.primary_pods().len(), 1);

    // Durable state is initialized and bound to the Secret UID.
    let secret = w.api.object(&state_path()).unwrap();
    for key in [annotations::INITIALIZED, annotations::CLUSTER_ID] {
        assert_eq!(
            str_at(&secret, &["metadata", "annotations", key]),
            SECRET_UID
        );
    }
    let record = w.record();
    assert_eq!(record["cluster"], SECRET_UID);
    assert_eq!(record["sequence"], 1);
    assert!(record["keyring"]["roots"].as_array().unwrap().len() >= 2);
    let primary = w
        .api
        .object(&lease_path(annotations::PRIMARY_LEASE))
        .unwrap();
    assert!(matches!(
        str_at(&primary, &["spec", "holderIdentity"]),
        "p1" | "p2"
    ));
    assert!(primary["spec"]["leaseTransitions"].as_u64().unwrap() >= 1);

    // A node enrolls through any seat, then reads a snapshot and keyring.
    let endpoint = w.authority(1).endpoints().read().unwrap()[0].clone();
    w.client(async move {
        let node_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let csr = rcgen::CertificateParams::new(Vec::<String>::new())
            .unwrap()
            .serialize_request(&node_key)
            .unwrap();
        let request = wire::EnrollmentRequest {
            shares: 4,
            rdma_nics: vec![],
            schema_version: wire::SCHEMA_VERSION,
            cluster: wire::ClusterId(SECRET_UID.into()),
            enrollment: wire::EnrollmentId("12345678-1234-4234-8234-123456789012".into()),
            csr_der: csr.der().to_vec(),
        };
        let body = wire::encode_enrollment_request(&request).unwrap();
        let bearer = format!("Bearer {}", token(1));
        let (url, pin) = (&endpoint.url, &endpoint.certificate);
        let auth = [("Authorization", bearer.as_str())];
        let (status, out) = call(url, pin, None, "POST", wire::BOOTSTRAP_PATH, &auth, &body).await;
        assert_eq!(status, 200, "{}", String::from_utf8_lossy(&out));
        let response = wire::decode_enrollment_response(&out).unwrap();
        assert_eq!(response.node.0, node_uid(1));
        assert_eq!(response.block_devices.as_deref(), Some("nvme-.*"));

        // Bad token: unauthenticated.
        let bad = [("Authorization", "Bearer x")];
        let (status, _) = call(url, pin, None, "POST", wire::BOOTSTRAP_PATH, &bad, &body).await;
        assert_eq!(status, 401);

        let cert = Some((response.certificate_chain.clone(), node_key.serialize_der()));
        let (status, out) =
            call(url, pin, cert.clone(), "GET", wire::SNAPSHOT_PATH, &[], &[]).await;
        assert_eq!(status, 200);
        let p = wire::decode_publication(&out).unwrap();
        assert_eq!(p.sequence.0, 1);
        assert_eq!(p.members.len(), 2);
        let (status, _) = call(url, pin, None, "GET", wire::SNAPSHOT_PATH, &[], &[]).await;
        assert_eq!(status, 401);
        let (status, out) = call(url, pin, cert.clone(), "GET", wire::KEYRING_PATH, &[], &[]).await;
        assert_eq!(status, 200);
        let bundle = wire::decode_bundle(&out).unwrap();
        assert!(bundle.peer_trust_roots.len() >= 2);
        let (status, _) = call(url, pin, None, "GET", wire::KEYRING_PATH, &auth, &[]).await;
        assert_eq!(status, 200);
        let (status, _) = call(
            url,
            pin,
            cert.clone(),
            "GET",
            "/v1/snapshot?after=x",
            &[],
            &[],
        )
        .await;
        assert_eq!(status, 400);
        // A cursor from a seat that is ahead of this one: retry or fail
        // over, as the Go controller answered.
        for path in [wire::SNAPSHOT_PATH, wire::KEYRING_PATH] {
            let target = format!("{path}?after=999999");
            let (status, out) = call(url, pin, cert.clone(), "GET", &target, &[], &[]).await;
            assert_eq!(status, 503, "{path}");
            assert_eq!(
                wire::decode_error(&out).unwrap().code,
                wire::ProtocolFailure::Unavailable
            );
        }
        let (status, _) = call(url, pin, cert, "GET", "/v1/snapshot?after=0", &[], &[]).await;
        assert_eq!(status, 409);
    });

    // A report reaches this Pod's annotation and the next publication.
    w.authority_mut(2).report(9, vec![]).unwrap();
    let pod = format!("/api/v1/namespaces/{NS}/pods/p2");
    assert!(w.run_until(Duration::from_secs(30), |w| {
        let pod = w.api.object(&pod).unwrap();
        str_at(&pod, &["metadata", "annotations", annotations::REPORT]).contains("\"shares\":9")
    }));
    assert!(w.run_until(Duration::from_secs(60), |w| {
        w.authority(1).diagnostics().sequence >= 2 && w.authority(2).diagnostics().sequence >= 2
    }));

    // Steady state API load is small and bounded.
    let before = w.api.requests().len();
    w.run_for(Duration::from_secs(10));
    let steady = w.api.requests().len() - before;
    assert!(steady < 150, "{steady} requests in 10s");

    // Graceful primary shutdown hands over to the other seat with a new term.
    let primary = w.primary_pods()[0];
    let other = 3 - primary;
    let before = w.record();
    w.stop(primary);
    assert!(w.run_until(Duration::from_secs(60), |w| matches!(
        w.role(other),
        Role::Primary(_)
    ) && w.ready(other)));
    assert!(w.run_until(Duration::from_secs(30), |w| {
        w.record()["term"]["transitions"].as_u64() > before["term"]["transitions"].as_u64()
    }));
    let after = w.record();
    assert_ne!(after["term"]["holder"], before["term"]["holder"]);
    assert!(after["sequence"].as_u64() >= before["sequence"].as_u64());
    assert_eq!(after["term"]["lease_uid"], before["term"]["lease_uid"]);

    // Losing the Secret data fails closed: endpoints are withdrawn, nothing resets.
    let mut s = w.api.object(&state_path()).unwrap();
    s["data"] = json!({});
    w.api.put_object(&state_path(), s);
    assert!(w.run_until(Duration::from_secs(30), |w| !w.ready(other)));
    w.run_for(Duration::from_secs(30));
    assert!(!w.has_record(), "runtime must never recreate state");
}

/// A seat that becomes ready writes that to its Lease right away, and the
/// primary publishes it within one reconcile period instead of waiting for
/// the next seat renewal and then the next primary renewal.
#[test]
fn readiness_is_published_without_waiting_for_renewals() {
    let mut w = World::new(1);
    let c = fixed(&w, 1, 1);
    w.start_with(1, c);
    assert!(w.run_until(Duration::from_secs(30), |w| w.ready(1)));
    let ready = w.now();
    let seat = lease_path(&annotations::seat_lease(0));
    let written = |w: &mut World| {
        w.api
            .object(&seat)
            .is_some_and(|l| str_at(&l, &["metadata", "annotations", annotations::READY]) == "1")
    };
    assert!(w.run_until(Duration::from_secs(3), written));
    // One reconcile period plus debounce and slack, short of the
    // renewal-paced path (seat renewal, then reconcile, then primary renewal).
    let budget = crate::limits::LEASE_RENEW + Duration::from_secs(5);
    assert!(w.run_until(budget, |w| w.endpoints(1) == 1));
    assert!(w.now() - ready <= budget, "{:?}", w.now() - ready);
}

/// A fresh Node with no local files must not adopt a racer-state Secret the
/// installation did not record: bootstrap fails closed before any seat is
/// taken or state is initialized.
#[test]
fn bootstrap_requires_the_installation_state_binding() {
    const REPLACED: &str = "c1c1c1c1-0000-4000-8000-0000000000ff";
    let ds = format!("/apis/apps/v1/namespaces/{NS}/daemonsets/racer");
    let sa = format!("/api/v1/namespaces/{NS}/serviceaccounts/racer");
    let state = state_path();
    type Mutate = fn(&mut Value, &mut Value, &mut Value);
    let cases: [(&str, Mutate); 6] = [
        ("replaced secret", |s, _, _| {
            s["metadata"]["uid"] = REPLACED.into()
        }),
        ("unannotated daemonset", |_, d, _| {
            d["metadata"]["annotations"] = json!({})
        }),
        ("mismatched service account", |_, _, a| {
            a["metadata"]["annotations"][crate::control::STATE_UID] = REPLACED.into()
        }),
        ("terminating daemonset", |_, d, _| {
            d["metadata"]["deletionTimestamp"] = "2026-01-01T00:00:00Z".into()
        }),
        ("missing daemonset", |_, d, _| *d = Value::Null),
        ("missing service account", |_, _, a| *a = Value::Null),
    ];
    for (name, mutate) in cases {
        let mut w = World::with(1, 1, |api| {
            let mut s = api.object(&state).unwrap();
            let mut d = api.object(&ds).unwrap();
            let mut a = api.object(&sa).unwrap();
            mutate(&mut s, &mut d, &mut a);
            for (path, obj) in [(&state, s), (&ds, d), (&sa, a)] {
                if obj.is_null() {
                    api.delete_object(path);
                } else {
                    api.put_object(path, obj);
                }
            }
        });
        w.start(1);
        assert!(
            w.run_until(Duration::from_secs(30), |w| w
                .authority(1)
                .bootstrap()
                .is_some()),
            "{name}"
        );
        assert_eq!(
            w.authority(1).bootstrap().unwrap().unwrap_err(),
            crate::Error::InvalidState,
            "{name}"
        );
        w.run_for(Duration::from_secs(60));
        assert_eq!(w.role(1), Role::Member, "{name}");
        let secret = w.api.object(&state).unwrap();
        assert!(secret["data"].get("state").is_none(), "{name}");
        assert!(
            str_at(
                &secret,
                &["metadata", "annotations", annotations::INITIALIZED]
            )
            .is_empty(),
            "{name}"
        );
    }
}

/// An operator deletes and recreates the primary Lease (new UID, transitions
/// reset). The term on the new Lease takes over the state record instead of
/// failing closed on a record stamped by the dead Lease.
#[test]
fn recreated_primary_lease_takes_over_state() {
    let mut w = World::new(1);
    ready_single(&mut w);
    assert!(w.run_until(Duration::from_secs(30), |w| w.has_record()));
    let before = w.record();
    let path = lease_path(annotations::PRIMARY_LEASE);
    w.api.delete_object(&path);
    let live = w.api.put_object(
        &path,
        json!({"metadata":{"name":annotations::PRIMARY_LEASE,"namespace":NS},"spec":{}}),
    );
    let live = str_at(&live, &["metadata", "uid"]).to_owned();
    assert_ne!(before["term"]["lease_uid"].as_str(), Some(live.as_str()));
    assert!(w.run_until(Duration::from_secs(90), |w| {
        w.record()["term"]["lease_uid"].as_str() == Some(live.as_str())
            && w.ready(1)
            && matches!(w.role(1), Role::Primary(_))
    }));
    let after = w.record();
    assert!(after["sequence"].as_u64() >= before["sequence"].as_u64());
    assert!(after["keyring"]["generation"].as_u64() >= before["keyring"]["generation"].as_u64());
}

#[test]
fn shutdown_releases_seat_and_primary_leases() {
    let delay = Arc::new(AtomicU64::new(0));
    let d = delay.clone();
    let mut w = World::with(1, 1, move |api| api.delay_puts(d));
    ready_single(&mut w);
    let seat = annotations::seat_lease(0);
    assert_eq!(w.holder(&seat), "p1");
    assert_eq!(w.holder(annotations::PRIMARY_LEASE), "p1");
    // Slow releases: a shutdown that does not wait for them returns first.
    delay.store(500, std::sync::atomic::Ordering::SeqCst);
    let took = w.stop(1);
    assert!(took < Duration::from_secs(5), "{took:?}");
    // Released before the authority finished, not left to expire.
    let r = w.api.requests();
    assert_eq!(
        w.holder(&seat),
        "",
        "{took:?} {:?}",
        &r[r.len().saturating_sub(8)..]
    );
    assert_eq!(w.holder(annotations::PRIMARY_LEASE), "", "{took:?}");
}

#[test]
fn shutdown_is_bounded_when_releases_hang() {
    let delay = Arc::new(AtomicU64::new(0));
    let d = delay.clone();
    let mut w = World::with(1, 1, move |api| api.delay_puts(d));
    ready_single(&mut w);
    delay.store(u64::MAX, std::sync::atomic::Ordering::SeqCst);
    let took = w.stop(1);
    assert!(took <= Duration::from_secs(6), "{took:?}");
    assert!(
        took >= Duration::from_secs(5),
        "did not wait for releases: {took:?}"
    );
}

/// Two Pods race for the primary Lease through the fake API at the same
/// time. Exactly one wins and commits under its term. When the winner
/// stops, the other takes over through the real reconcile path: the state
/// record moves to the new holder's term and its sequence never goes back.
#[test]
fn racing_primaries_are_fenced_by_term_and_resource_version() {
    for seed_value in 1..=4 {
        let mut w = World::with(2, seed_value, |_| {});
        let (c1, c2) = (fixed(&w, 1, 2), fixed(&w, 2, 2));
        w.start_with(1, c1);
        w.start_with(2, c2);
        assert!(
            w.run_until(Duration::from_secs(60), |w| w.primary_pods().len() == 1
                && w.ready(1)
                && w.ready(2)
                && w.has_record()),
            "seed {seed_value}"
        );
        let winner = w.primary_pods()[0];
        let loser = 3 - winner;
        assert!(
            w.holder(annotations::PRIMARY_LEASE)
                .starts_with(&format!("p{winner}"))
        );
        let before = w.record();
        w.stop(winner);
        assert!(
            w.run_until(Duration::from_secs(90), |w| {
                w.holder(annotations::PRIMARY_LEASE)
                    .starts_with(&format!("p{loser}"))
                    && matches!(w.role(loser), Role::Primary(_))
                    && w.ready(loser)
                    && w.record()["term"] != before["term"]
            }),
            "seed {seed_value}"
        );
        let after = w.record();
        assert!(after["sequence"].as_u64() >= before["sequence"].as_u64());
        assert!(
            after["keyring"]["generation"].as_u64() >= before["keyring"]["generation"].as_u64()
        );
    }
}

/// Three authorities and the fake API, stepped on this thread, elect exactly
/// one primary, which commits and publishes all three seats.
#[test]
fn three_authorities_elect_one_primary_and_publish_on_one_thread() {
    let mut w = World::new(3);
    for i in 1..=3 {
        w.start(i);
    }
    assert!(w.run_until(Duration::from_secs(120), |w| {
        w.primary_pods().len() == 1 && (1..=3).all(|i| w.ready(i) && w.endpoints(i) == 3)
    }));
    let record = w.record();
    assert!(record["sequence"].as_u64() >= Some(1));
    let winner = w.primary_pods()[0];
    assert_eq!(w.terms.len(), 1, "{:?}", w.terms);
    assert_eq!(w.terms[0].1, winner);
    assert_eq!(
        record["term"]["holder"]
            .as_str()
            .map(|h| h.split('/').next().unwrap()),
        Some(format!("p{winner}").as_str())
    );
}

/// The primary renews its Lease every [`crate::limits::LEASE_RENEW`].
#[test]
fn primary_renews_every_renew_interval() {
    let mut w = World::new(1);
    ready_single(&mut w);
    let path = lease_path(annotations::PRIMARY_LEASE);
    let renew_time = |w: &World| {
        let l = w.api.object(&path).unwrap();
        str_at(&l, &["spec", "renewTime"]).to_owned()
    };
    let mut seen = vec![(w.now(), renew_time(&w))];
    let end = w.now() + Duration::from_secs(65);
    w.run_until(Duration::from_secs(70), |w| {
        let r = renew_time(w);
        if r != seen.last().unwrap().1 {
            seen.push((w.now(), r));
        }
        w.now() >= end
    });
    let gaps: Vec<Duration> = seen.windows(2).skip(1).map(|p| p[1].0 - p[0].0).collect();
    assert!(gaps.len() >= 4, "{gaps:?}");
    for gap in &gaps {
        // Early renewals publish discovery changes; never later than the interval.
        assert!(
            *gap <= crate::limits::LEASE_RENEW + Duration::from_secs(1),
            "{gaps:?}"
        );
    }
    assert!(
        gaps.iter()
            .filter(|g| **g >= crate::limits::LEASE_RENEW - Duration::from_secs(1))
            .count()
            >= 3,
        "{gaps:?}"
    );
}

/// The primary is cut off from the API. It stops acting as primary within
/// [`crate::limits::LEASE_FRESHNESS`] of its last renewal, and the other
/// seat takes over only after the Lease expired
/// ([`crate::limits::LEASE_DURATION`]), so there is a gap with no primary.
/// With `rates` the two clocks run at different speeds.
fn partitioned_primary_hands_over(seed_value: u64, rates: [i64; 2]) {
    let mut w = World::with(2, seed_value, |_| {});
    for (i, ppm) in rates.iter().enumerate() {
        w.clock(i + 1).set_rate_ppm(*ppm).unwrap();
    }
    let (c1, c2) = (fixed(&w, 1, 2), fixed(&w, 2, 2));
    w.start_with(1, c1);
    w.start_with(2, c2);
    assert!(
        w.run_until(Duration::from_secs(90), |w| w.primary_pods().len() == 1
            && w.ready(1)
            && w.ready(2))
    );
    w.run_for(Duration::from_secs(15));
    let old = w.primary_pods()[0];
    let new = 3 - old;
    w.partition(old);
    let cut = w.now();
    // The old primary stops acting within its freshness window, by its own
    // clock, of a renewal that started before the cut.
    assert!(w.run_until(Duration::from_secs(60), |w| {
        w.authority(old).primary_term().is_none()
    }));
    let lost = w.now();
    let rate = |ppm: i64| 1.0 + ppm as f64 / 1e6;
    let freshness = crate::limits::LEASE_FRESHNESS.as_secs_f64() / rate(rates[old - 1]);
    assert!(
        (lost - cut).as_secs_f64() <= freshness + 0.5,
        "{:?} after the cut",
        lost - cut
    );
    // The other seat takes over after the Lease expired.
    assert!(w.run_until(Duration::from_secs(120), |w| {
        w.authority(new).primary_term().is_some()
    }));
    let gained = w.now();
    assert!(
        gained - lost >= Duration::from_secs(5),
        "takeover {:?} after the old primary stopped",
        gained - lost
    );
    assert!(w.run_until(Duration::from_secs(60), |w| w.ready(new)));
    // Heal: the old primary rejoins as a seat and never leads the old term.
    w.heal(old);
    w.run_for(Duration::from_secs(60));
    assert_eq!(w.primary_pods(), vec![new]);
    let terms: Vec<usize> = w.terms.iter().map(|t| t.1).collect();
    assert_eq!(terms, vec![old, new], "{:?}", w.terms);
}

#[test]
fn partitioned_primary_hands_over_after_lease_expiry() {
    partitioned_primary_hands_over(1, [0, 0]);
}

/// Two authorities whose clocks run 10% apart, in both directions, never
/// act as primary at the same time.
#[test]
fn skewed_clock_rates_never_yield_two_primaries() {
    for (seed_value, rates) in [(2, [-100_000, 100_000]), (3, [100_000, -100_000])] {
        partitioned_primary_hands_over(seed_value, rates);
    }
}

/// A seat cut off from the API stops serving within
/// [`crate::limits::LEASE_FRESHNESS`], and [`crate::limits::STATE_FRESHNESS`]
/// bounds how long it serves an image it can no longer confirm.
#[test]
fn partitioned_seat_stops_serving_within_freshness() {
    let mut w = World::new(2);
    let (c1, c2) = (fixed(&w, 1, 2), fixed(&w, 2, 2));
    w.start_with(1, c1);
    w.start_with(2, c2);
    assert!(
        w.run_until(Duration::from_secs(90), |w| w.primary_pods().len() == 1
            && w.ready(1)
            && w.ready(2))
    );
    w.run_for(Duration::from_secs(7));
    let seat = 3 - w.primary_pods()[0];
    w.partition(seat);
    let cut = w.now();
    assert!(w.run_until(Duration::from_secs(60), |w| !w.ready(seat)));
    let bound = crate::limits::LEASE_FRESHNESS.min(crate::limits::STATE_FRESHNESS);
    assert!(w.now() - cut <= bound, "{:?}", w.now() - cut);
    // The primary keeps serving and withdraws the seat from discovery.
    let primary = 3 - seat;
    assert!(w.run_until(Duration::from_secs(90), |w| w.endpoints(primary) == 1));
    assert!(w.ready(primary));
}

/// The same seed replays the same election: the same winner at the same
/// virtual time after the same API requests.
#[test]
fn same_seed_replays_the_same_election() {
    let run = |seed_value| {
        let mut w = World::with(3, seed_value, |_| {});
        for i in 1..=3 {
            w.start(i);
        }
        assert!(w.run_until(Duration::from_secs(120), |w| w.has_record()));
        let (at, winner, _) = w.terms[0].clone();
        (at, winner, w.api.requests())
    };
    for seed_value in [5, 6] {
        assert_eq!(run(seed_value), run(seed_value), "seed {seed_value}");
    }
}

/// The API server ends every watch stream (a restart or an idle cut). The
/// primary watches again: a later Pod change still reaches a publication
/// well before any periodic resync.
#[test]
fn dropped_watches_are_resumed() {
    let mut w = World::new(1);
    ready_single(&mut w);
    assert!(w.run_until(
        Duration::from_secs(30),
        |w| w.authority(1).diagnostics().sequence >= 1
    ));
    w.run_for(Duration::from_secs(5));
    let before = w.authority(1).diagnostics().sequence;
    w.api.drop_watches();
    w.run_for(Duration::from_secs(1));
    w.authority_mut(1).report(9, vec![]).unwrap();
    assert!(w.run_until(Duration::from_secs(20), |w| {
        w.authority(1).diagnostics().sequence > before
    }));
    assert!(w.ready(1));
    assert!(matches!(w.role(1), Role::Primary(0)));
}

/// A Pod process dies without stopping (its authority is dropped). A new
/// process with the same identity binds the same addresses and rejoins.
#[test]
fn dropped_authority_restarts_with_the_same_identity() {
    let mut w = World::new(1);
    ready_single(&mut w);
    let before = w.os.live_handles();
    w.nodes[0].authority = None;
    // Its listeners and connections closed with it.
    assert!(w.os.live_handles() < before);
    w.run_for(Duration::from_secs(1));
    let c = fixed(&w, 1, 1);
    w.start_with(1, c);
    assert!(w.run_until(Duration::from_secs(120), |w| w.ready(1)
        && matches!(w.role(1), Role::Primary(0))));
}
