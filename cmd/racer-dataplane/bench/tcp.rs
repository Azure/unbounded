// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::{
    allocator::{Allocator, Slab},
    buffers::{Fill, Key, WorkerPool},
    cache::CachedValue,
    http_client as client, http_server as server, tls,
};
use std::num::NonZeroU32;

pub(super) fn slab(o: &Options, allowed: usize) -> io::Result<Option<Mutex<Slab>>> {
    if !o.server || o.body != "file" {
        return Ok(None);
    }
    let path = o
        .slab_dir
        .as_ref()
        .unwrap()
        .join(format!("racer-transport-bench-{}.slab", std::process::id()));
    let slab = Slab::create(&path, allowed as u64 * 32 * 1024 * 1024, allowed)?;
    std::fs::remove_file(path)?;
    Ok(Some(Mutex::new(slab)))
}
pub(super) fn payload(
    o: &Options,
    placement: &workers::WorkerContext,
    pool: &WorkerPool,
    ring: &mut uring::Ring,
    slab: Option<&Mutex<Slab>>,
) -> io::Result<CachedValue> {
    if o.kind == Kind::Metadata {
        return Ok(CachedValue::Metadata(fixture::record()));
    }
    let f = fixture::Fixture::new(o.kind);
    let mut fill = pool
        .stage(Key::new(f.key))
        .map_err(|_| io::Error::other("pool exhausted"))?;
    for (i, b) in fill.as_mut_slice().iter_mut().enumerate() {
        *b = fixture::pattern(i);
    }
    let crc = crate::allocator::crc64(fill.as_mut_slice());
    let buffer = fill.publish_checked(BUFFER_SIZE, crc)?;
    // Benchmark fidelity: prepared registered Buffer or allocator FileValue goes
    // through BodyChunk::value, exactly as handlers/response.rs does after lookup.
    if let Some(slab) = slab {
        let shard = slab.lock().unwrap().take_shard(placement.shard_ids()[0])?;
        let mut allocator = Allocator::open(placement, shard, Default::default())?;
        allocator
            .insert_payload(f.key, buffer, None)
            .map_err(|e| e.error)?;
        let lease = allocator
            .lookup(&f.key, 0)
            .ok_or_else(|| invalid("fixture insert missing"))?;
        let deadline = Instant::now() + o.timeout;
        while !allocator.is_idle() {
            if Instant::now() >= deadline {
                return Err(timed_out("fixture file preparation timed out"));
            }
            ring.progress()?;
            let work = allocator.poll(ring, BUDGET)?;
            if !work.runnable && !allocator.is_idle() {
                ring.wait(Some(deadline))?;
            }
        }
        Ok(CachedValue::File(
            lease
                .ready()
                .ok_or_else(|| invalid("fixture file unavailable"))?,
        ))
    } else {
        Ok(CachedValue::Buffer(buffer))
    }
}

#[allow(clippy::large_enum_variant)]
pub(super) enum Task {
    Request(Option<server::Request>),
    Headers(server::SendingGetHeaders),
    Body(server::SendingBody),
    Done,
}
pub(super) struct Handler {
    fixture: fixture::Fixture,
    payload: CachedValue,
    body: String,
    failed: bool,
}
impl Handler {
    fn progress(
        &mut self,
        task: &mut Task,
        ring: &mut uring::Ring,
        budget: usize,
    ) -> io::Result<server::Progress<server::Completed>> {
        let result = match task {
            Task::Request(request) => {
                let request = request.take().unwrap();
                self.fixture.validate_request(request.headers())?;
                let server::Request::Get(request) = request else {
                    return Err(invalid("fixture requires GET"));
                };
                let etag = fixture::record().checksum.etag();
                let crc = format!("{:016x}", self.payload.checksum().unwrap());
                let headers = [
                    ("ETag", etag.as_str().as_bytes()),
                    ("X-Racer-CRC64", crc.as_bytes()),
                    ("X-Racer-Bench-Body", self.body.as_bytes()),
                ];
                let head = server::ResponseHead::new(
                    200,
                    Some(self.fixture.kind.bytes() as u64),
                    &headers,
                )?;
                *task = Task::Headers(request.respond(head)?);
                return Ok(server::Progress::Pending(uring::Work {
                    runnable: true,
                    deadline: None,
                }));
            }
            Task::Headers(send) => send.poll(ring, budget)?,
            Task::Body(send) => send.poll(ring, budget)?,
            Task::Done => return Err(invalid("completed fixture task polled")),
        };
        match result {
            server::Progress::Pending(w) => Ok(server::Progress::Pending(w)),
            server::Progress::Ready(server::BodyProgress::Done(done)) => {
                *task = Task::Done;
                Ok(server::Progress::Ready(done))
            }
            server::Progress::Ready(server::BodyProgress::More(writer)) => {
                // Metadata stays inline in CachedValue::Metadata, with no page buffer.
                let chunk =
                    server::BodyChunk::value(self.payload.clone(), 0..self.fixture.kind.bytes())
                        .map_err(|e| e.error)?;
                *task = Task::Body(writer.send(chunk).map_err(|e| e.error)?);
                Ok(server::Progress::Pending(uring::Work {
                    runnable: true,
                    deadline: None,
                }))
            }
        }
    }
}
impl server::Handler for Handler {
    type Task = Task;
    fn start(&mut self, request: server::Request) -> Task {
        Task::Request(Some(request))
    }
    fn poll(
        &mut self,
        task: &mut Task,
        ring: &mut uring::Ring,
        budget: usize,
    ) -> io::Result<server::Progress<server::Completed>> {
        let result = self.progress(task, ring, budget);
        self.failed |= result.is_err();
        result
    }
}
#[allow(clippy::large_enum_variant)]
enum Exchange {
    Metadata(client::SmallExchange),
    Page(client::GetExchange),
}
struct Slot {
    connection: Option<client::Connection>,
    fill: Option<Fill>,
    exchange: Option<Exchange>,
    started: Instant,
    verified: bool,
}
pub(super) struct Client {
    slots: Vec<Slot>,
    fixture: fixture::Fixture,
    measure: Measure,
    first: usize,
    body: String,
}
impl Client {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        let mut work = uring::Work::default();
        let per = (budget / self.slots.len()).max(1);
        for offset in 0..self.slots.len() {
            let i = (self.first + offset) % self.slots.len();
            let slot = &mut self.slots[i];
            let mut completed = false;
            if let Some(exchange) = &mut slot.exchange {
                match exchange {
                    Exchange::Metadata(e) => match e.poll(ring, per)? {
                        client::Progress::Pending(w) => work.merge(w),
                        client::Progress::Ready(response) => {
                            if response.status() != 200
                                || response.content_length()
                                    != Some(self.fixture.kind.bytes() as u64)
                            {
                                return Err(invalid("unexpected metadata response"));
                            }
                            // Benchmark fidelity: metadata decode and CRC stay timed, as in upstream.rs.
                            self.fixture
                                .validate_response(response.headers(), Some(response.body()))?;
                            if !slot.verified {
                                self.fixture.validate_body(response.body())?;
                            }
                            self.measure.completion(slot.started, Instant::now());
                            slot.connection = response.recycle();
                            completed = true;
                        }
                    },
                    Exchange::Page(e) => match e.poll(ring, per)? {
                        client::Progress::Pending(w) => work.merge(w),
                        client::Progress::Ready(mut response) => {
                            if response.status() != 200
                                || response.content_length() != Some(BUFFER_SIZE as u64)
                            {
                                return Err(invalid("unexpected page response"));
                            }
                            self.fixture.validate_response(response.headers(), None)?;
                            if crate::cache::http_metadata::text(
                                response.headers(),
                                "x-racer-bench-body",
                            )? != Some(self.body.as_str())
                            {
                                return Err(invalid("client/server --body mismatch"));
                            }
                            self.measure.completion(slot.started, Instant::now());
                            if !slot.verified {
                                self.fixture.validate_body(response.body())?;
                            }
                            let (connection, fill, len) = response.recycle();
                            if len != BUFFER_SIZE {
                                return Err(invalid("short page"));
                            }
                            slot.connection = connection;
                            slot.fill = Some(fill);
                            completed = true;
                        }
                    },
                }
            }
            if completed {
                if slot.connection.is_none() {
                    return Err(io::Error::new(
                        io::ErrorKind::UnexpectedEof,
                        "persistent connection closed",
                    ));
                }
                slot.exchange = None;
                slot.verified = true;
            }
            let now = Instant::now();
            if slot.exchange.is_none() && (self.measure.admitting(now) || !slot.verified) {
                slot.started = now;
                let deadline = now + self.measure.timeout;
                let fields = self.fixture.fields(deadline)?;
                let fields: Vec<_> = fields.iter().map(|(n, v)| (*n, v.as_str())).collect();
                let request = client::Request::new("/", &fields)?;
                let connection = slot.connection.take().unwrap();
                slot.exchange = Some(if self.fixture.kind == Kind::Metadata {
                    Exchange::Metadata(connection.get_small(
                        request,
                        crate::metadata::Metadata::SIZE,
                        deadline,
                    )?)
                } else {
                    Exchange::Page(connection.get(request, slot.fill.take().unwrap(), deadline)?)
                });
                work.runnable = true;
            }
        }
        self.first = (self.first + 1) % self.slots.len();
        work.merge(self.measure.progress(
            self.slots.iter().all(|s| s.exchange.is_none()),
            self.slots.iter().all(|s| s.verified),
        )?);
        Ok(work)
    }
}
pub(super) enum App {
    Server(server::Server<Handler>),
    Client(Client),
}
impl uring::Application for App {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        match self {
            Self::Server(s) => {
                let w = s.poll(ring, budget)?;
                if s.handler().failed {
                    return Err(invalid("server fixture failed"));
                }
                Ok(w)
            }
            Self::Client(c) => c.poll(ring, budget),
        }
    }
    fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        match self {
            Self::Server(s) => s.shutdown(ring),
            Self::Client(c) => {
                c.slots.clear();
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
    slab: Option<&Mutex<Slab>>,
    shared: Arc<Shared>,
) -> io::Result<uring::Driver<super::App>> {
    let fixture = fixture::Fixture::new(o.kind);
    let app = if o.server {
        let payload = payload(o, placement, &pool, &mut ring, slab)?;
        let mut listener = server::Listener::bind(o.address, NonZeroU32::new(1024).unwrap())?;
        listener.set_tls(
            tls.clone(),
            tls::ExpectedPeer::Identity(fixture::identity(false)),
        );
        App::Server(server::Server::new(
            listener,
            Handler {
                fixture,
                payload,
                body: o.body.clone(),
                failed: false,
            },
            server::Config {
                request_timeout: o.timeout,
                streaming_timeout: o.timeout,
                ..Default::default()
            },
        ))
    } else {
        let mut slots = Vec::new();
        for _ in 0..o.connections {
            let fill = if o.kind == Kind::Metadata {
                None
            } else {
                Some(
                    pool.stage(Key::new(fixture.key))
                        .map_err(|_| invalid("pool exhausted"))?,
                )
            };
            slots.push(Slot {
                connection: Some(client::Connection::new_tls(
                    o.address,
                    &o.address.to_string(),
                    tls,
                    tls::ExpectedPeer::Identity(fixture::identity(true)),
                )?),
                fill,
                exchange: None,
                started: Instant::now(),
                verified: false,
            });
        }
        App::Client(Client {
            slots,
            fixture,
            measure: Measure::new(o, shared, placement.worker_id().0),
            first: 0,
            body: o.body.clone(),
        })
    };
    uring::Driver::new(ring, super::App::Tcp(app), BUDGET)
}
