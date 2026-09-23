// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Request admission, authentication, and immutable volume-context capture.
use super::*;

impl Handler {
    pub(super) fn start_request(&mut self, mut request: http::Request) -> Task {
        let traffic = if request.headers().get("x-racer-fault").is_some() {
            crate::metrics::Traffic::PeerHttp
        } else {
            crate::metrics::Traffic::ClientHttp
        };
        self.upstream.metrics.request(traffic);
        request.set_metric_traffic(traffic);
        let shared_cache = self.cache.clone();
        let mut cache = shared_cache.borrow_mut();
        let context = cache::Context::new(self.namespace).with_crypto(self.crypto.clone());
        let deadline = request.deadline();
        let response_deadline = request.response_deadline();
        let mut task = Task {
            _cache_use: (!self.maintenance).then(|| cache.use_guard()),
            upstream: self.upstream.routed(None),
            response: Response::Request(request),
            metadata: None,
            fault: None,
            pages: VecDeque::new(),
            position: 0,
            end: 0,
            next: 0,
            peer: false,
            distributed: self
                .upstream
                .routing
                .as_ref()
                .map_or(self.upstream.peer.is_some(), |r| {
                    r.local.len() < r.geometry.slot_count() as usize
                }),
            deadline,
            response_deadline,
            failure: None,
            head: None,
            metric_peer: matches!(traffic, crate::metrics::Traffic::PeerHttp),
            error_metric: None,
            headers_sent: false,
        };
        let Response::Request(request) = &task.response else {
            unreachable!()
        };
        let parsed: cache::Result<Initial> = (|| {
            if let Some(wire) = text(request.headers(), "x-racer-fault")? {
                if !matches!(request, http::Request::Get(_)) {
                    return Err(invalid("peer faults require GET").into());
                }
                task.peer = true;
                let policy = self
                    .upstream
                    .authentication
                    .as_ref()
                    .ok_or_else(|| invalid("missing peer authentication policy"))?;
                match request.peer_identity() {
                    Some(identity) => policy.authorize(identity)?,
                    None => return Err(invalid("peer request requires mutual TLS").into()),
                }
                let bytes = unhex(wire)?;
                let (cursor, descriptor) = routed_descriptor(&bytes)?;
                task.deadline = remote_deadline(&bytes, deadline)?;
                task.upstream = self.upstream.routed(self.upstream.route_state(
                    cursor,
                    &descriptor.key(self.namespace)?,
                    true,
                )?);
                if self.maintenance {
                    return Err(cache::busy("storage maintenance"));
                }
                cache
                    .peer_fault_in(&context, descriptor, task.deadline)
                    .map(Initial::Peer)
            } else {
                if self.maintenance {
                    return Err(cache::busy("storage maintenance"));
                }
                if task.distributed && !cache::peer_wire::client_fits(request.target().len()) {
                    task.failure = Some(414);
                    return Err(invalid("target exceeds distributed page wire limit").into());
                }
                task.upstream = self.upstream.routed(self.upstream.route_state(
                    None,
                    &cache::PeerDescriptor::metadata(request.target()).key(self.namespace)?,
                    false,
                )?);
                cache
                    .metadata_in(&context, request.target(), deadline)
                    .map(Initial::Metadata)
            }
        })();
        match parsed {
            Ok(fault) => task.fault = Some(fault),
            Err(error @ cache::Error::Admission(_)) => task.fault = Some(Initial::Rejected(error)),
            Err(error) if error.io_kind() == io::ErrorKind::WouldBlock => {
                task.error_metric = Some(metric_failure(&error));
                task.failure = Some(503)
            }
            Err(_) => task.failure = Some(task.failure.unwrap_or(400)),
        }
        if task.peer
            && let Response::Request(request) = &mut task.response
        {
            request.cap_deadline(task.deadline + RETURN_SLACK);
        }
        task
    }
}
