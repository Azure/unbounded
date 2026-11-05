//! Simulation support for deterministic harnesses outside this crate.
//!
//! Not a stable API and not a cargo feature (simulation is always compiled,
//! plan decision D9). It exposes what a deterministic simulation needs to run
//! real [`Authority`](crate::Authority) instances against the in-memory
//! Kubernetes API that this crate's own simulated tests use: [`FakeKube`] (the
//! API server as a steppable role on the simulated network), [`KubeView`]
//! (read-only observation and fault injection), and [`pod_config`].
//!
//! Every identity is fixed: Pod `i` (1-based) is `p{i}` on Node `n{i}` with
//! control address `10.0.1.{i}:8443`; the API server is `10.0.0.1:443`. The
//! API server's TLS key and CA file live on the simulated disk under `/api`.

#![doc(hidden)]

use crate::{
    Config, Next, SeatPolicy, annotations,
    config::Rotation,
    kube::{KubeConfig, fake::FakeApi, str_at},
    rt::Executor,
    rules::state::Term,
};
use base64::{
    Engine,
    engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD},
};
use racer_io::{ctx::VirtualClock, ring::SocketAddress, sim::SimOs};
use serde_json::{Value, json};
use std::{
    net::{IpAddr, SocketAddr},
    path::Path,
};

/// Fake API server address on the simulated network.
pub const API: &str = "10.0.0.1:443";

/// Control API port of every Pod.
pub const CONTROL_PORT: u16 = 8443;

/// Webhook port of every Pod.
pub const WEBHOOK_PORT: u16 = 9443;

/// Directory of the API server's CA and token files on the simulated disk.
pub const API_DIR: &str = "/api";

/// Tasks of the fake API executor.
const API_TASKS: usize = 4096;

/// Pod `i`'s IP address.
pub fn pod_ip(i: usize) -> IpAddr {
    IpAddr::from([10, 0, 1, i as u8])
}

/// Pod `i`'s endpoint label: sockets it opens while labeled with it are cut
/// by [`SimOs::partition`] on this label.
pub fn pod_endpoint(i: usize) -> SocketAddress {
    SocketAddress::Inet(SocketAddr::new(pod_ip(i), CONTROL_PORT))
}

/// The API server's endpoint label.
pub fn api_endpoint() -> SocketAddress {
    SocketAddress::Inet(API.parse().expect("constant address"))
}

/// Path of the racer-state Secret.
pub fn state_path() -> String {
    format!("/api/v1/namespaces/{NS}/secrets/racer-state")
}

/// Path of Lease `name`.
pub fn lease_path(name: &str) -> String {
    format!("/apis/coordination.k8s.io/v1/namespaces/{NS}/leases/{name}")
}

/// Configuration of Pod `i` against the [`FakeKube`] on `os`. Writes Pod
/// `i`'s control token (expiring at `token_exp`, Unix seconds) to the
/// simulated disk.
pub fn pod_config(os: &SimOs, i: usize, token_exp: u64) -> std::io::Result<Config> {
    let token_path = format!("/pods/{i}/control-token");
    os.write_file(Path::new(&token_path), token(i, token_exp).as_bytes())?;
    let api = Path::new(API_DIR);
    Ok(Config {
        namespace: NS.into(),
        pod_name: format!("p{i}"),
        pod_uid: pod_uid(i),
        node_name: format!("n{i}"),
        pod_ip: pod_ip(i),
        seats: SeatPolicy::Auto,
        control_listen: SocketAddr::new(pod_ip(i), CONTROL_PORT),
        webhook_listen: SocketAddr::new(pod_ip(i), WEBHOOK_PORT),
        webhook_alternate: None,
        object_image: Some("example.com/racer-object:1".into()),
        api_server: API.into(),
        api_ca: api.join("ca.crt"),
        api_token: api.join("token"),
        control_token: token_path.into(),
        daemonset: "racer".into(),
        service_account: "racer".into(),
        peer_port: 8082,
        page_size: racer_wire::object::PageSize::DEFAULT,
        rotation: Rotation::default(),
    })
}

/// The fake Kubernetes API server as one steppable role: its own executor on
/// the shared simulated network, with `pods` Pods seeded.
pub struct FakeKube {
    executor: Executor,
    api: FakeApi,
    kube: KubeConfig,
}

impl FakeKube {
    /// Serve the fake API at [`API`] on `os`, on `clock`.
    pub fn new(
        os: SimOs,
        clock: &VirtualClock,
        seed_value: u64,
        pods: usize,
    ) -> std::io::Result<Self> {
        let executor = Executor::simulated(os.clone(), clock, seed_value, API_TASKS)?;
        let _label = os.enter_endpoint(api_endpoint());
        let (api, kube) = executor.enter(|| {
            let api = FakeApi::new();
            seed(&api, pods);
            let kube = crate::kube::fake::serve_at(
                api.clone(),
                Path::new(API_DIR),
                API.parse().expect("constant address"),
            );
            (api, kube)
        });
        Ok(Self {
            executor,
            api,
            kube,
        })
    }

    /// One bounded step.
    pub fn turn(&mut self) -> std::io::Result<Next> {
        self.executor.turn()
    }

    /// What the server needs next, without doing work.
    pub fn next(&self) -> Next {
        self.executor.next()
    }

    /// The API server address clients use.
    pub fn server(&self) -> &str {
        &self.kube.server
    }

    /// Observation and fault handle; usable while the server runs.
    pub fn view(&self) -> KubeView {
        KubeView(self.api.clone())
    }
}

/// The committed state record, as stored in the racer-state Secret.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Committed {
    /// Publication sequence.
    pub sequence: u64,
    /// Keyring generation.
    pub keyring_generation: u64,
    /// The primary term that wrote it.
    pub term: Term,
}

/// Read access and fault injection on a [`FakeKube`].
#[derive(Clone)]
pub struct KubeView(FakeApi);

impl KubeView {
    /// The committed state record, if the Secret holds one.
    pub fn committed(&self) -> Option<Committed> {
        let secret = self.0.object(&state_path())?;
        let bytes = STANDARD.decode(secret["data"]["state"].as_str()?).ok()?;
        let record: Value = serde_json::from_slice(&bytes).ok()?;
        Some(Committed {
            sequence: record["sequence"].as_u64()?,
            keyring_generation: record["keyring"]["generation"].as_u64().unwrap_or(0),
            term: serde_json::from_value(record["term"].clone()).ok()?,
        })
    }

    /// Holder identity of Lease `name`, empty when vacant.
    pub fn holder(&self, name: &str) -> String {
        self.0
            .object(&lease_path(name))
            .map(|l| str_at(&l, &["spec", "holderIdentity"]).to_owned())
            .unwrap_or_default()
    }

    /// Holder of seat Lease `index`.
    pub fn seat_holder(&self, index: usize) -> String {
        self.holder(&annotations::seat_lease(index))
    }

    /// Requests served so far.
    pub fn requests(&self) -> usize {
        self.0.requests().len()
    }

    /// Answer the next write with a spurious 409 Conflict.
    pub fn conflict_next_write(&self) {
        self.0.fail_next_write(409);
    }

    /// End every open watch stream.
    pub fn drop_watches(&self) {
        self.0.drop_watches();
    }

    /// Forget watch history so resumed watches get 410 Gone.
    pub fn compact(&self) {
        self.0.compact();
    }
}

pub const NS: &str = "racer-system";
pub const SECRET_UID: &str = "c1c1c1c1-0000-4000-8000-000000000001";
pub const DS_UID: &str = "d5d5d5d5-0000-4000-8000-000000000001";
pub const SA_UID: &str = "5a5a5a5a-0000-4000-8000-000000000001";

pub fn node_uid(i: usize) -> String {
    format!("0000000{i}-0000-4000-8000-0000000000aa")
}

pub fn pod_uid(i: usize) -> String {
    format!("0000000{i}-0000-4000-8000-0000000000bb")
}

/// Unsigned test JWT naming Pod `pod` that expires at `exp` (Unix seconds).
pub fn token(pod: usize, exp: u64) -> String {
    format!(
        "e30.{}.sig",
        URL_SAFE_NO_PAD.encode(json!({"exp": exp, "pod": pod}).to_string())
    )
}

/// Seed `api` with the objects an installation has: the empty state Secret,
/// seat and primary Leases, the DaemonSet and ServiceAccount bound to the
/// Secret, and `pods` Nodes and Pods. TokenReviews authenticate [`token`]s.
pub(crate) fn seed(api: &FakeApi, pods: usize) {
    let mut secret = json!({"apiVersion":"v1","kind":"Secret","type":"Opaque","metadata":{"name":"racer-state","namespace":NS,"uid":SECRET_UID}});
    secret["data"] = json!({});
    api.put_object(
        &format!("/api/v1/namespaces/{NS}/secrets/racer-state"),
        secret,
    );
    for name in (0..16)
        .map(annotations::seat_lease)
        .chain([annotations::PRIMARY_LEASE.to_owned()])
    {
        api.put_object(
            &format!("/apis/coordination.k8s.io/v1/namespaces/{NS}/leases/{name}"),
            json!({"metadata":{"name":name,"namespace":NS},"spec":{}}),
        );
    }
    api.put_object(
        &format!("/apis/apps/v1/namespaces/{NS}/daemonsets/racer"),
        json!({"metadata":{"name":"racer","uid":DS_UID,"annotations":{crate::control::STATE_UID:SECRET_UID}}}),
    );
    api.put_object(
        &format!("/api/v1/namespaces/{NS}/serviceaccounts/racer"),
        json!({"metadata":{"name":"racer","uid":SA_UID,"annotations":{crate::control::STATE_UID:SECRET_UID}}}),
    );
    for i in 1..=pods {
        api.put_object(
            &format!("/api/v1/nodes/n{i}"),
            json!({"metadata":{"name":format!("n{i}"),"uid":node_uid(i),"annotations":{annotations::BLOCK_DEVICES:"nvme-.*"}}}),
        );
        api.put_object(
            &format!("/api/v1/namespaces/{NS}/pods/p{i}"),
            json!({
                "metadata":{"name":format!("p{i}"),"namespace":NS,"uid":pod_uid(i),"creationTimestamp":"2026-01-01T00:00:00Z",
                    // Serving dataplanes have reported.
                    "annotations":{annotations::REPORT:"{\"shares\":1,\"rdma_nics\":[]}"},
                    "ownerReferences":[{"apiVersion":"apps/v1","kind":"DaemonSet","name":"racer","uid":DS_UID,"controller":true}]},
                "spec":{"nodeName":format!("n{i}"),"serviceAccountName":"racer"},
                "status":{"podIP":format!("127.0.0.{}", i + 1),"phase":"Running"}
            }),
        );
    }
    api.on_post(|path, body| {
        if path != crate::kube::paths::TOKEN_REVIEWS {
            return None;
        }
        let token = body["spec"]["token"].as_str().unwrap_or_default();
        let payload = token.split('.').nth(1).and_then(|p| URL_SAFE_NO_PAD.decode(p).ok());
        let Some(claims) = payload.and_then(|p| serde_json::from_slice::<Value>(&p).ok()) else {
            return Some((201, json!({"status":{"authenticated":false}})));
        };
        let i = claims["pod"].as_u64().unwrap_or(0) as usize;
        Some((201, json!({"status":{
            "authenticated": true,
            "audiences": ["racer-control"],
            "user": {"username": format!("system:serviceaccount:{NS}:racer"), "uid": SA_UID, "extra": {
                "authentication.kubernetes.io/pod-name": [format!("p{i}")],
                "authentication.kubernetes.io/pod-uid": [pod_uid(i)],
                "authentication.kubernetes.io/node-name": [format!("n{i}")],
                "authentication.kubernetes.io/node-uid": [node_uid(i)],
            }}
        }})))
    });
}
