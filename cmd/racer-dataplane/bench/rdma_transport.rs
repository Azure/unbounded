// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::{
    buffers::{Key, WorkerPool},
    control::{self, proto},
    http_server as http, negotiation, rdma as transport, tls,
};
use std::{num::NonZeroU32, rc::Rc};

pub(super) fn context(o: &Options, tls: &tls::TlsContext) -> io::Result<Rc<negotiation::Context>> {
    let local = if o.server { 2 } else { 3 };
    let remote = if o.server { 3 } else { 2 };
    let peer = format!("{remote:02x}").repeat(32);
    let config = proto::Snapshot {
        universe: vec![1; 32],
        node: vec![local; 32],
        revision: 1,
        fabric: "benchmark".into(),
        member_catalogs: vec![proto::MemberCatalog {
            members: [local, remote]
                .into_iter()
                .map(|node| proto::Member {
                    node: vec![node; 32],
                    pod_uid: "benchmark".into(),
                    fabric: "benchmark".into(),
                })
                .collect(),
        }],
        peers: vec![proto::Peer {
            id: peer.clone(),
            pod_uid: "benchmark".into(),
            http_address: o.address.to_string(),
            fabric: "benchmark".into(),
            ..Default::default()
        }],
        volumes: vec![proto::Volume {
            max_candidate_attempts: Some(2),
            id: fixture::VOLUME.into(),
            member_catalog: Some(0),
            client_socket: "/run/racer/v1/client/socket".into(),
            origin_socket: "/run/racer/v1/origin/socket".into(),
            peers: vec![peer.clone()],
            peer_endpoints: Some(proto::VolumePeerEndpoints {
                peers: vec![proto::VolumePeerEndpoint {
                    peer: peer.clone(),
                    http_address: o.address.to_string(),
                }],
            }),
            topology: Some(proto::Topology {
                product: Some(proto::ProductTopology {
                    left_factor: 1,
                    right_factor: 2,
                    members: [local.min(remote), local.max(remote)]
                        .into_iter()
                        .map(|n| format!("{n:02x}").repeat(32))
                        .collect(),
                    roles: vec![0, 1],
                    local_member: u32::from(local > remote),
                    candidate_width: 2,
                    candidates: vec![0, 1, 1, 0],
                }),
                routing_algorithm: Some(1),
                epoch: 1,
                slot_count: 2,
                local_slots: vec![u32::from(local > remote)],
            }),
            ..Default::default()
        }],
        ..Default::default()
    };
    let prepared = control::Trust {
        universe: [1; 32],
        node: [local; 32],
    }
    .prepare(proto::Configuration {
        contents: Some(proto::configuration::Contents::Snapshot(config)),
    })?;
    // Benchmark fidelity: only enrollment and discovery are fixtures. Authorization,
    // HEAD offer exchange, TLS channel binding, and confirmation stay production code.
    let provider =
        control::credentials::Provider::for_test(fixture::identity(o.server), tls.clone());
    Ok(Rc::new(
        negotiation::Context::new(Arc::new(prepared), fixture::VOLUME, 0, b"benchmark")?
            .with_credentials(Some(provider)),
    ))
}
struct Handler {
    negotiation: negotiation::Server,
    failed: bool,
}
impl http::Handler for Handler {
    type Task = io::Result<negotiation::Task>;
    fn start(&mut self, request: http::Request) -> Self::Task {
        self.negotiation.start(request)
    }
    fn poll(
        &mut self,
        task: &mut Self::Task,
        ring: &mut uring::Ring,
        budget: usize,
    ) -> io::Result<http::Progress<http::Completed>> {
        let result = match task {
            Ok(t) => self.negotiation.poll_task(t, ring, budget),
            Err(e) => Err(io::Error::new(e.kind(), e.to_string())),
        };
        // Capacity/renewal rejects are normal production negotiation backpressure.
        if let Err(e) = &result {
            if !matches!(
                e.kind(),
                io::ErrorKind::WouldBlock
                    | io::ErrorKind::NotConnected
                    | io::ErrorKind::ConnectionAborted
                    | io::ErrorKind::UnexpectedEof
            ) {
                self.failed = true;
            }
        }
        result
    }
}
pub(super) struct Server {
    http: http::Server<Handler>,
    connections: Vec<negotiation::Established>,
    payload: crate::buffers::Buffer,
    fixture: fixture::Fixture,
}
impl Server {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        let mut work = self.http.poll(ring, budget)?;
        if self.http.handler().failed {
            return Err(invalid("RDMA negotiation failed"));
        }
        let now = Instant::now();
        while let Some(c) = self.http.handler_mut().negotiation.take_completed(now) {
            self.connections.push(c);
        }
        for c in &self.connections {
            if !c.connection.is_confirmed() {
                continue;
            }
            for _ in 0..budget {
                let request = match c.connection.next_request() {
                    Ok(Some(r)) => r,
                    Ok(None) => break,
                    Err(_) if !c.connection.is_healthy() => break,
                    Err(e) => return Err(e),
                };
                if request.value != self.fixture.key || request.len != BUFFER_SIZE {
                    return Err(invalid("incorrect RDMA fixture request"));
                }
                let (descriptor, _) = crate::authorization::rdma_decode(&request.metadata)?;
                self.fixture.validate_descriptor(descriptor)?;
                c.connection
                    .respond(request, self.payload.clone())
                    .map_err(|e| e.error)?;
                work.runnable = true;
            }
        }
        self.connections.retain(|c| {
            c.connection.is_healthy() || c.confirmation_deadline.is_some_and(|d| now < d)
        });
        Ok(work)
    }
}
enum Operation {
    Grant(transport::Ticket<transport::Grant>),
    Read(transport::Ticket<transport::Read>),
}
struct Lane {
    operation: Option<Operation>,
    started: Option<Instant>,
    verified: bool,
}
struct Session {
    negotiation: Option<negotiation::Client>,
    connection: Option<transport::Connection>,
    lanes: Vec<Lane>,
    retry: Instant,
    unavailable: Instant,
}
pub(super) struct Client {
    sessions: Vec<Session>,
    pool: WorkerPool,
    context: Rc<negotiation::Context>,
    rails: negotiation::Rails,
    options: Options,
    fixture: fixture::Fixture,
    measure: Measure,
    retries: u64,
}
impl Client {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        let mut work = uring::Work::default();
        let now = Instant::now();
        for session in &mut self.sessions {
            // Retained logical requests keep their deadline even while reconnecting.
            for lane in &session.lanes {
                if let Some(started) = lane.started {
                    let deadline = started + self.options.timeout;
                    if now >= deadline {
                        return Err(timed_out("RDMA operation deadline"));
                    }
                    work.merge(uring::Work {
                        runnable: false,
                        deadline: Some(deadline),
                    });
                }
            }
            let active = session.lanes.iter().any(|l| l.started.is_some());
            if session.connection.is_none()
                && session.negotiation.is_none()
                && (self.measure.admitting(now)
                    || active
                    || session.lanes.iter().any(|l| !l.verified))
            {
                if now >= session.unavailable + self.options.timeout {
                    return Err(timed_out("RDMA connection unavailable"));
                }
                if now >= session.retry {
                    let peer = "02".repeat(32);
                    match negotiation::Client::start(
                        self.context.clone(),
                        self.rails.clone(),
                        &peer,
                        "/",
                        self.options.timeout,
                    ) {
                        Ok(n) => session.negotiation = Some(n),
                        Err(e)
                            if matches!(
                                e.kind(),
                                io::ErrorKind::WouldBlock | io::ErrorKind::NotConnected
                            ) =>
                        {
                            session.retry = now + Duration::from_millis(10)
                        }
                        Err(e) => return Err(e),
                    }
                }
                work.merge(uring::Work {
                    runnable: false,
                    deadline: Some(session.retry.max(now + Duration::from_millis(1))),
                });
            }
            if let Some(n) = &mut session.negotiation {
                match n.poll(ring, budget) {
                    Ok(http::Progress::Pending(w)) => work.merge(w),
                    Ok(http::Progress::Ready(c)) => {
                        session.connection = Some(c.connection);
                        session.negotiation = None;
                        work.runnable = true;
                    }
                    Err(_) => {
                        session.negotiation = None;
                        session.retry = now + Duration::from_millis(10);
                        self.retries += 1;
                        work.merge(uring::Work {
                            runnable: false,
                            deadline: Some(session.retry),
                        });
                    }
                }
            }
            let Some(c) = &session.connection else {
                continue;
            };
            let result = (|| -> io::Result<()> {
                for lane in &mut session.lanes {
                    if let Some(operation) = &mut lane.operation {
                        match operation {
                            Operation::Grant(ticket) => {
                                if let Some(grant) = c.take_grant(ticket)? {
                                    let fill = self
                                        .pool
                                        .stage(Key::new(self.fixture.key))
                                        .map_err(|_| io::Error::from(io::ErrorKind::WouldBlock))?;
                                    lane.operation = Some(Operation::Read(
                                        c.read(grant, fill).map_err(|e| e.error)?,
                                    ));
                                    work.runnable = true;
                                }
                            }
                            Operation::Read(ticket) => {
                                if let Some((mut fill, len)) = c.take_read_unpublished(ticket)? {
                                    // Benchmark fidelity: includes READ CQE and TLS ACK retirement,
                                    // excludes cache checksum admission just like the TCP fixture.
                                    self.measure
                                        .completion(lane.started.unwrap(), Instant::now());
                                    if len != BUFFER_SIZE {
                                        return Err(invalid("short RDMA page"));
                                    }
                                    if !lane.verified {
                                        self.fixture.validate_body(&fill.as_mut_slice()[..len])?;
                                    }
                                    lane.verified = true;
                                    lane.operation = None;
                                    lane.started = None;
                                    work.runnable = true;
                                }
                            }
                        }
                    }
                    if lane.operation.is_none()
                        && (self.measure.admitting(now) || !lane.verified || lane.started.is_some())
                    {
                        let started = *lane.started.get_or_insert_with(Instant::now);
                        let descriptor = self.fixture.descriptor(started + self.options.timeout)?;
                        let descriptor = crate::authorization::rdma_envelope(
                            &descriptor,
                            &Default::default(),
                        )
                        .ok_or_else(|| io::Error::other("RDMA fixture envelope too large"))?;
                        match c.request(self.fixture.key, BUFFER_SIZE, &descriptor) {
                            Ok(ticket) => lane.operation = Some(Operation::Grant(ticket)),
                            Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                                work.merge(uring::Work {
                                    runnable: false,
                                    deadline: Some(started + self.options.timeout),
                                });
                                continue;
                            }
                            Err(e) => return Err(e),
                        }
                        work.runnable = true;
                    }
                }
                Ok(())
            })();
            if let Err(e) = result {
                if matches!(
                    e.kind(),
                    io::ErrorKind::NotConnected
                        | io::ErrorKind::ConnectionAborted
                        | io::ErrorKind::ConnectionReset
                        | io::ErrorKind::BrokenPipe
                        | io::ErrorKind::UnexpectedEof
                ) {
                    // Production retires all QPs before recycling MW keys. Reconnect
                    // through negotiation, retaining logical operation start times.
                    c.disconnect()?;
                    session.connection = None;
                    session.unavailable = now;
                    session.retry = now + Duration::from_millis(10);
                    self.retries += 1;
                    for lane in &mut session.lanes {
                        lane.operation = None;
                    }
                    work.runnable = true;
                } else {
                    return Err(e);
                }
            }
        }
        work.merge(
            self.measure.progress(
                self.sessions
                    .iter()
                    .all(|s| s.lanes.iter().all(|l| l.started.is_none())),
                self.sessions
                    .iter()
                    .all(|s| s.lanes.iter().all(|l| l.verified)),
            )?,
        );
        Ok(work)
    }
}
pub(super) enum App {
    Server(Server),
    Client(Client),
}
impl uring::Application for App {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        match self {
            Self::Server(s) => s.poll(ring, budget),
            Self::Client(c) => c.poll(ring, budget),
        }
    }
    fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        match self {
            Self::Server(s) => {
                s.connections.clear();
                s.http.shutdown(ring)
            }
            Self::Client(c) => {
                eprintln!(
                    "worker={} rdma_reconnections={}",
                    c.measure.worker, c.retries
                );
                c.sessions.clear();
                Ok(())
            }
        }
    }
}
pub(super) fn driver(
    o: &Options,
    placement: &workers::WorkerContext,
    pool: WorkerPool,
    mut ring: uring::Ring,
    tls: &tls::TlsContext,
    rail: transport::Rail,
    shared: Arc<Shared>,
) -> io::Result<uring::Driver<super::App>> {
    let (transport, source) = transport::Transport::new(
        &pool,
        rail,
        transport::Config {
            fabric: "benchmark".into(),
            connections: o.connections,
            depth: o.depth,
            timeout: o.timeout,
        },
    )?;
    let rails = negotiation::Rails::new(vec![Some(transport)], 1)?;
    let context = context(o, tls)?;
    let fixture = fixture::Fixture::new(Kind::Rdma);
    let app = if o.server {
        let crate::cache::CachedValue::Buffer(payload) =
            tcp::payload(o, placement, &pool, &mut ring, None)?
        else {
            unreachable!()
        };
        let mut listener = http::Listener::bind(o.address, NonZeroU32::new(1024).unwrap())?;
        listener.set_tls(
            tls.clone(),
            tls::ExpectedPeer::Identity(fixture::identity(false)),
        );
        App::Server(Server {
            http: http::Server::new(
                listener,
                Handler {
                    negotiation: negotiation::Server::new(
                        context,
                        rails,
                        o.connections,
                        o.timeout,
                    )?,
                    failed: false,
                },
                http::Config {
                    request_timeout: o.timeout,
                    streaming_timeout: o.timeout,
                    ..Default::default()
                },
            ),
            connections: Vec::new(),
            payload,
            fixture,
        })
    } else {
        App::Client(Client {
            sessions: (0..o.connections)
                .map(|_| Session {
                    negotiation: None,
                    connection: None,
                    lanes: (0..o.depth)
                        .map(|_| Lane {
                            operation: None,
                            started: None,
                            verified: false,
                        })
                        .collect(),
                    retry: Instant::now(),
                    unavailable: Instant::now(),
                })
                .collect(),
            pool,
            context,
            rails,
            options: o.clone(),
            fixture,
            measure: Measure::new(o, shared, placement.worker_id().0),
            retries: 0,
        })
    };
    let mut driver = uring::Driver::new(ring, super::App::Rdma(app), BUDGET)?;
    driver.add_source(source);
    Ok(driver)
}
