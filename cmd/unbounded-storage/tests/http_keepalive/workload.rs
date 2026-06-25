// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Workload model and runner for HTTP keep-alive connection semantics.
//!
//! The runner intentionally models only the transport-independent part of
//! the frontend: incremental request-head parsing, the keep-alive decision,
//! header-drain behavior that preserves pipelined bytes, and response
//! `Connection` header serialization. Real sockets and io_uring progress
//! remain covered by the storage smoke/integration paths.

use std::cell::{Cell, RefCell};
use std::collections::VecDeque;
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};

use ::http::header::{CONNECTION, CONTENT_LENGTH};
use proptest::collection::vec;
use proptest::prelude::*;
use unbounded_storage::http::{
    HttpRequest, ParseError, ResponseHead, StatusCode, connection_header_value,
    request_allows_keep_alive, request_is_bodyless, request_wants_keep_alive,
    serialize_response_head,
};

use crate::framework::executor::{Executor, RunError, yield_n, yield_once};

#[derive(Clone, Debug)]
pub struct Workload {
    pub requests: Vec<RequestSpec>,
    pub chunk_sizes: Vec<u8>,
    pub max_send_delay: u32,
    pub max_requests_per_connection: usize,
}

#[derive(Clone, Debug)]
pub struct RequestSpec {
    pub method: RequestMethod,
    pub version: HttpVersion,
    pub connection: ConnectionSpec,
    pub body: BodySpec,
}

#[derive(Clone, Debug)]
pub enum RequestMethod {
    Get,
    Head,
}

#[derive(Clone, Debug)]
pub enum HttpVersion {
    Http10,
    Http11,
}

#[derive(Clone, Debug)]
pub enum ConnectionSpec {
    None,
    KeepAlive,
    Close,
    KeepAliveThenClose,
}

#[derive(Clone, Debug)]
pub enum BodySpec {
    None,
    ContentLengthZero,
    ContentLengthNonZero,
    DuplicateContentLength,
    TransferEncoding,
}

#[derive(Debug)]
pub struct RunReport {
    pub served: Vec<ServedRequest>,
    pub stopped_with: StopReason,
    pub leftover: Vec<u8>,
    pub steps: u64,
}

#[derive(Debug)]
pub struct ServedRequest {
    pub request_index: usize,
    pub wants_keep_alive: bool,
    pub bodyless: bool,
    pub keep_alive: bool,
    pub response_connection: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StopReason {
    ClientClosed,
    ServerClosed,
    ParseError,
}

pub fn workload_strategy() -> impl Strategy<Value = Workload> {
    (
        vec(request_strategy(), 1..=8),
        vec(1u8..=48, 1..=24),
        prop_oneof![1 => Just(0u32), 9 => 1u32..=4],
        1usize..=8,
    )
        .prop_map(
            |(requests, chunk_sizes, max_send_delay, max_requests_per_connection)| Workload {
                requests,
                chunk_sizes,
                max_send_delay,
                max_requests_per_connection,
            },
        )
}

fn request_strategy() -> impl Strategy<Value = RequestSpec> {
    (
        prop_oneof![Just(RequestMethod::Get), Just(RequestMethod::Head)],
        prop_oneof![1 => Just(HttpVersion::Http10), 4 => Just(HttpVersion::Http11)],
        prop_oneof![
            4 => Just(ConnectionSpec::None),
            3 => Just(ConnectionSpec::KeepAlive),
            2 => Just(ConnectionSpec::Close),
            1 => Just(ConnectionSpec::KeepAliveThenClose),
        ],
        prop_oneof![
            5 => Just(BodySpec::None),
            3 => Just(BodySpec::ContentLengthZero),
            1 => Just(BodySpec::ContentLengthNonZero),
            1 => Just(BodySpec::DuplicateContentLength),
            1 => Just(BodySpec::TransferEncoding),
        ],
    )
        .prop_map(|(method, version, connection, body)| RequestSpec {
            method,
            version,
            connection,
            body,
        })
}

pub fn run_workload(seed: u64, w: Workload) -> Result<RunReport, RunError> {
    let chunks = chunk_request_bytes(&w);
    let inbound = Rc::new(RefCell::new(VecDeque::<Vec<u8>>::new()));
    let client_done = Rc::new(Cell::new(false));
    let input_waiter = Rc::new(RefCell::new(None));
    let report = Rc::new(RefCell::new(None));

    let mut exec = Executor::new(seed);
    spawn_client(
        &mut exec,
        chunks,
        inbound.clone(),
        client_done.clone(),
        input_waiter.clone(),
        w.max_send_delay,
    );
    spawn_server(
        &mut exec,
        inbound,
        client_done,
        input_waiter,
        report.clone(),
        w.requests.len(),
        w.max_requests_per_connection,
    );

    let step_budget = 512 + w.requests.len() as u64 * 256 + w.chunk_sizes.len() as u64 * 16;
    exec.run(step_budget)?;

    let mut report = report.borrow_mut().take().expect("server produced report");
    report.steps = exec.last_steps();
    Ok(report)
}

pub fn expected_keep_alive_prefix(w: &Workload) -> Vec<bool> {
    let mut out = Vec::new();
    for req in &w.requests {
        let bytes = req.to_bytes();
        let parsed = HttpRequest::parse(&bytes).expect("generated request parses");
        let keep_alive = request_allows_keep_alive(&parsed)
            && out.len().saturating_add(1) < w.max_requests_per_connection;
        out.push(keep_alive);
        if !keep_alive {
            break;
        }
    }
    out
}

pub fn request_bytes(reqs: &[RequestSpec]) -> Vec<u8> {
    let mut out = Vec::new();
    for req in reqs {
        out.extend_from_slice(&req.to_bytes());
    }
    out
}

fn spawn_client(
    exec: &mut Executor,
    chunks: Vec<Vec<u8>>,
    inbound: Rc<RefCell<VecDeque<Vec<u8>>>>,
    client_done: Rc<Cell<bool>>,
    input_waiter: Rc<RefCell<Option<Waker>>>,
    max_send_delay: u32,
) {
    exec.spawn(async move {
        for chunk in chunks {
            let delay = if max_send_delay == 0 {
                0
            } else {
                crate::framework::executor::with_sim(|s| {
                    use rand::Rng;
                    s.rng.gen_range(0..=max_send_delay)
                })
            };
            yield_n(delay).await;
            inbound.borrow_mut().push_back(chunk);
            wake_input_waiter(&input_waiter);
            yield_once().await;
        }
        client_done.set(true);
        wake_input_waiter(&input_waiter);
    });
}

fn spawn_server(
    exec: &mut Executor,
    inbound: Rc<RefCell<VecDeque<Vec<u8>>>>,
    client_done: Rc<Cell<bool>>,
    input_waiter: Rc<RefCell<Option<Waker>>>,
    report: Rc<RefCell<Option<RunReport>>>,
    request_count: usize,
    max_requests_per_connection: usize,
) {
    exec.spawn(async move {
        let mut buf = Vec::new();
        let mut served = Vec::new();
        let mut stop = StopReason::ClientClosed;

        loop {
            if served.len() >= request_count {
                break;
            }

            match HttpRequest::parse(&buf) {
                Ok(req) => {
                    let request_index = served.len();
                    let wants_keep_alive = request_wants_keep_alive(&req);
                    let bodyless = request_is_bodyless(&req);
                    let keep_alive = request_allows_keep_alive(&req)
                        && served.len().saturating_add(1) < max_requests_per_connection;
                    let header_end = req.header_end;
                    let response_connection = response_connection_value(keep_alive);

                    served.push(ServedRequest {
                        request_index,
                        wants_keep_alive,
                        bodyless,
                        keep_alive,
                        response_connection,
                    });
                    buf.drain(..header_end);

                    if !keep_alive {
                        stop = StopReason::ServerClosed;
                        break;
                    }
                    yield_once().await;
                }
                Err(ParseError::Incomplete) => {
                    if let Some(chunk) = inbound.borrow_mut().pop_front() {
                        buf.extend_from_slice(&chunk);
                    } else if client_done.get() {
                        stop = StopReason::ClientClosed;
                        break;
                    } else {
                        wait_for_input(inbound.clone(), client_done.clone(), input_waiter.clone())
                            .await;
                    }
                }
                Err(_) => {
                    stop = StopReason::ParseError;
                    break;
                }
            }
        }

        *report.borrow_mut() = Some(RunReport {
            served,
            stopped_with: stop,
            leftover: buf,
            steps: 0,
        });
    });
}

fn wake_input_waiter(input_waiter: &Rc<RefCell<Option<Waker>>>) {
    if let Some(waker) = input_waiter.borrow_mut().take() {
        waker.wake();
    }
}

fn wait_for_input(
    inbound: Rc<RefCell<VecDeque<Vec<u8>>>>,
    client_done: Rc<Cell<bool>>,
    input_waiter: Rc<RefCell<Option<Waker>>>,
) -> InputReady {
    InputReady {
        inbound,
        client_done,
        input_waiter,
    }
}

struct InputReady {
    inbound: Rc<RefCell<VecDeque<Vec<u8>>>>,
    client_done: Rc<Cell<bool>>,
    input_waiter: Rc<RefCell<Option<Waker>>>,
}

impl Future for InputReady {
    type Output = ();

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        if !self.inbound.borrow().is_empty() || self.client_done.get() {
            Poll::Ready(())
        } else {
            *self.input_waiter.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        }
    }
}

fn response_connection_value(keep_alive: bool) -> String {
    let resp = ::http::Response::builder()
        .status(StatusCode::OK)
        .header(CONTENT_LENGTH, "0")
        .header(CONNECTION, connection_header_value(keep_alive))
        .body(())
        .expect("valid response");
    let bytes = serialize_response_head(&resp);
    let head = ResponseHead::parse(&bytes)
        .expect("response parses")
        .expect("response head complete");
    head.header("connection")
        .expect("connection header present")
        .to_string()
}

fn chunk_request_bytes(w: &Workload) -> Vec<Vec<u8>> {
    let bytes = request_bytes(&w.requests);
    let mut chunks = Vec::new();
    let mut pos = 0usize;
    let mut idx = 0usize;
    while pos < bytes.len() {
        let requested = w.chunk_sizes[idx % w.chunk_sizes.len()].max(1) as usize;
        let end = pos.saturating_add(requested).min(bytes.len());
        chunks.push(bytes[pos..end].to_vec());
        pos = end;
        idx += 1;
    }
    chunks
}

impl RequestSpec {
    pub fn to_bytes(&self) -> Vec<u8> {
        let mut out = format!(
            "{} /object/{} {}\r\n",
            self.method.as_str(),
            self.path_suffix(),
            self.version.as_str()
        )
        .into_bytes();

        match self.connection {
            ConnectionSpec::None => {}
            ConnectionSpec::KeepAlive => out.extend_from_slice(b"Connection: keep-alive\r\n"),
            ConnectionSpec::Close => out.extend_from_slice(b"Connection: close\r\n"),
            ConnectionSpec::KeepAliveThenClose => {
                out.extend_from_slice(b"Connection: keep-alive\r\n");
                out.extend_from_slice(b"Connection: close\r\n");
            }
        }

        match self.body {
            BodySpec::None => {}
            BodySpec::ContentLengthZero => out.extend_from_slice(b"Content-Length: 0\r\n"),
            BodySpec::ContentLengthNonZero => out.extend_from_slice(b"Content-Length: 1\r\n"),
            BodySpec::DuplicateContentLength => {
                out.extend_from_slice(b"Content-Length: 0\r\n");
                out.extend_from_slice(b"Content-Length: 1\r\n");
            }
            BodySpec::TransferEncoding => out.extend_from_slice(b"Transfer-Encoding: chunked\r\n"),
        }

        out.extend_from_slice(b"\r\n");
        if matches!(self.body, BodySpec::ContentLengthNonZero) {
            out.extend_from_slice(b"x");
        }
        out
    }

    fn path_suffix(&self) -> &'static str {
        match self.body {
            BodySpec::None => "none",
            BodySpec::ContentLengthZero => "cl0",
            BodySpec::ContentLengthNonZero => "cl1",
            BodySpec::DuplicateContentLength => "dupe-cl",
            BodySpec::TransferEncoding => "te",
        }
    }
}

impl RequestMethod {
    fn as_str(&self) -> &'static str {
        match self {
            RequestMethod::Get => "GET",
            RequestMethod::Head => "HEAD",
        }
    }
}

impl HttpVersion {
    fn as_str(&self) -> &'static str {
        match self {
            HttpVersion::Http10 => "HTTP/1.0",
            HttpVersion::Http11 => "HTTP/1.1",
        }
    }
}
