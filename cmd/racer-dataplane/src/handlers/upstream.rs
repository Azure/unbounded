// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! HTTP transport admission and exchange completion. Cache retains publication
//! authority and reports validation before pooled connections can be reused.
use super::*;

#[allow(clippy::large_enum_variant)]
pub(super) enum HttpGet {
    Payload(client::GetExchange<Destination>),
    Metadata(client::SmallExchange),
}
pub(super) enum HttpResponse {
    Payload(client::GetResponse<Destination>),
    Metadata(client::SmallResponse),
}
impl HttpGet {
    fn take_stale(&mut self) -> Option<(Option<Destination>, Instant, Option<Instant>, bool)> {
        match self {
            Self::Payload(e) => e
                .take_stale()
                .map(|(d, end, connect, service)| (Some(d), end, connect, service)),
            Self::Metadata(e) => e
                .take_stale()
                .map(|(end, connect, service)| (None, end, connect, service)),
        }
    }
    fn retain_connect_end(&mut self, end: Option<Instant>, service: bool) {
        match self {
            Self::Payload(e) => e.retain_connect_end(end, service),
            Self::Metadata(e) => e.retain_connect_end(end, service),
        }
    }
    pub(super) fn poll(
        &mut self,
        ring: &mut Ring,
        budget: usize,
    ) -> cache::Result<Progress<HttpResponse>> {
        Ok(match self {
            Self::Payload(e) => match e.poll_typed(ring, budget)? {
                Progress::Pending(w) => Progress::Pending(w),
                Progress::Ready(r) => Progress::Ready(HttpResponse::Payload(r)),
            },
            Self::Metadata(e) => match e.poll_typed(ring, budget)? {
                Progress::Pending(w) => Progress::Pending(w),
                Progress::Ready(r) => Progress::Ready(HttpResponse::Metadata(r)),
            },
        })
    }
}
impl HttpResponse {
    pub(super) fn status(&self) -> u16 {
        match self {
            Self::Payload(r) => r.status(),
            Self::Metadata(r) => r.status(),
        }
    }
    pub(super) fn content_length(&self) -> Option<u64> {
        match self {
            Self::Payload(r) => r.content_length(),
            Self::Metadata(r) => r.content_length(),
        }
    }
    pub(super) fn headers(&self) -> client::Headers<'_> {
        match self {
            Self::Payload(r) => r.headers(),
            Self::Metadata(r) => r.headers(),
        }
    }
    pub(super) fn body(&mut self) -> &[u8] {
        match self {
            Self::Payload(r) => r.body(),
            Self::Metadata(r) => r.body(),
        }
    }
    pub(super) fn recycle(self) -> (Option<client::Connection>, Option<Destination>, usize) {
        match self {
            Self::Payload(r) => {
                let (c, d, n) = r.recycle();
                (c, Some(d), n)
            }
            Self::Metadata(r) => {
                let n = r.body().len();
                (r.recycle(), None, n)
            }
        }
    }
}

impl Provider {
    pub(super) fn poll_head(
        &mut self,
        phase: HeadPhase,
        ring: &mut Ring,
    ) -> cache::Result<ExchangeProgress<Exchange>> {
        let HeadPhase {
            mut exchange,
            permit,
        } = phase;
        let mut permit = Some(permit);
        let result = (|| match exchange.poll_typed(ring, 1)? {
            Progress::Pending(work) => Ok(ExchangeProgress::Pending {
                exchange: Exchange::Head(HeadPhase {
                    exchange,
                    permit: permit.take().unwrap(),
                }),
                work,
            }),
            Progress::Ready(response) => {
                let facts = metadata_facts(&response)?;
                self.backend.borrow_mut().recycle(response.recycle());
                Ok(ExchangeProgress::Ready(UpstreamResult::Metadata(
                    crate::metadata::Metadata::from_backend(facts),
                )))
            }
        })();
        if let Err(error) = &result {
            HttpOrigin::error(permit.take().unwrap(), error, false);
        }
        if let Some(permit) = permit {
            permit.success();
        }
        result
    }

    pub(super) fn poll_get(
        &mut self,
        phase: GetPhase,
        ring: &mut Ring,
    ) -> cache::Result<ExchangeProgress<Exchange>> {
        let GetPhase {
            mut exchange,
            request,
            permit,
            mut attempt,
        } = phase;
        let mut permit = Some(permit);
        let peer = matches!(
            request,
            UpstreamRequest::PeerMetadata(_) | UpstreamRequest::PeerPage(_)
        );
        let progress = exchange.poll(ring, 1);
        if peer
            && progress.is_err()
            && let Some((destination, end, connect_end, service)) = exchange.take_stale()
        {
            // Remote rotation/idle retirement is not owner failure. Retry once
            // on fresh TLS, spending this resolution's remaining chain authority.
            // No response byte or body IO was observed on the retired socket.
            drop(permit.take());
            self.peer.as_ref().unwrap().borrow_mut().http.retire_idle();
            let mut retry =
                self.http_peer_attempt_until(request, destination, end, attempt, end)?;
            if let Exchange::Get(phase) = &mut retry {
                phase.exchange.retain_connect_end(connect_end, service);
            }
            return Ok(ExchangeProgress::Pending {
                exchange: retry,
                work: runnable(),
            });
        }
        let result = (|| match progress.map_err(|error| {
            if let Some(attempt) = &attempt {
                let mut evidence = error.evidence().attempt.cloned();
                // A late poll cannot turn actual caller expiry into owner or
                // intermediate evidence, even if the exchange had a private cap.
                if self
                    .caller_deadline
                    .is_some_and(|end| crate::environment::now() >= end)
                    && let Some(e) = &mut evidence
                    && e.cause == crate::outcome::Cause::ServiceTimeout
                {
                    e.cause = crate::outcome::Cause::CallerDeadline;
                }
                cache::Error::from(AttemptFailure {
                    route: attempt.route.clone(),
                    evidence,
                    reported: false,
                })
            } else {
                error.into()
            }
        })? {
            Progress::Pending(work) => Ok(ExchangeProgress::Pending {
                exchange: Exchange::Get(GetPhase {
                    exchange,
                    request,
                    permit: permit.take().unwrap(),
                    attempt: attempt.take(),
                }),
                work,
            }),
            Progress::Ready(mut response) => {
                if peer && response.status() != 200 {
                    if text(response.headers(), "x-racer-failure")?.is_some() {
                        let a = attempt
                            .as_ref()
                            .ok_or_else(|| invalid("unrouted peer failure"))?;
                        let failure = validate_peer_report(
                            response.headers(),
                            response.content_length(),
                            response.status(),
                            &a.route,
                        )?;
                        let (connection, _, len) = response.recycle();
                        if len != 0 {
                            return Err(invalid("peer failure body").into());
                        }
                        self.peer
                            .as_ref()
                            .unwrap()
                            .borrow_mut()
                            .http
                            .recycle(connection);
                        permit.take().unwrap().success();
                        self.reported(failure, &mut attempt)?;
                        unreachable!();
                    }
                    if response.status() == 503
                        && text(response.headers(), "x-racer-owner-unavailable")?.is_some()
                    {
                        let a = attempt
                            .as_ref()
                            .ok_or_else(|| invalid("unrouted owner report"))?;
                        validate_owner_report(
                            response.headers(),
                            response.content_length(),
                            &a.route,
                        )?;
                        let (connection, _, len) = response.recycle();
                        if len != 0 {
                            return Err(invalid("owner report body").into());
                        }
                        self.peer
                            .as_ref()
                            .unwrap()
                            .borrow_mut()
                            .http
                            .recycle(connection);
                        permit.take().unwrap().success();
                        return Err(AttemptFailure {
                            route: a.route.clone(),
                            evidence: None,
                            reported: true,
                        }
                        .into());
                    }
                    return Err(cache::http_metadata::response_status(
                        response.status(),
                        response.headers(),
                    )?);
                }
                let facts = if peer {
                    let expected = match &request {
                        UpstreamRequest::PeerPage(page) => page.checksum(),
                        UpstreamRequest::PeerMetadata(_) => {
                            crate::metadata::Metadata::from_bytes(response.body())?.checksum
                        }
                        _ => unreachable!(),
                    };
                    cache::http_metadata::peer_checksum(response.headers(), expected)?;
                    let content_type =
                        crate::metadata::ContentType::parse(response.headers(), "content-type")?;
                    let expected_type = match &request {
                        UpstreamRequest::PeerPage(page) => page.content_type(),
                        UpstreamRequest::PeerMetadata(_) => {
                            crate::metadata::Metadata::from_bytes(response.body())?.content_type
                        }
                        _ => unreachable!(),
                    };
                    if content_type != expected_type {
                        return Err(invalid("peer Content-Type mismatch").into());
                    }
                    None
                } else {
                    Some(page_facts(response.status(), response.headers())?)
                };
                identity_encoding(response.headers())?;
                let checksum = if peer {
                    checksum(response.headers())?
                } else {
                    None
                };
                let metadata = if matches!(request, UpstreamRequest::PeerMetadata(_)) {
                    let bytes = response.body();
                    if checksum != Some(crate::allocator::crc64(bytes)) {
                        return Err(invalid("peer checksum mismatch").into());
                    }
                    Some(crate::metadata::Metadata::from_bytes(bytes)?)
                } else {
                    None
                };
                let (connection, destination, len) = response.recycle();
                let validation = if peer {
                    Some(Exchange::ValidateHttp(HttpValidation {
                        connection,
                        permit: permit.take(),
                        attempt: attempt.take(),
                    }))
                } else {
                    self.backend.borrow_mut().recycle(connection);
                    None
                };
                let result = match request {
                    UpstreamRequest::PeerMetadata(_) => UpstreamResult::Metadata(metadata.unwrap()),
                    UpstreamRequest::PeerPage(_) => UpstreamResult::PeerPage(Received {
                        destination: destination.unwrap(),
                        len,
                        checksum,
                    }),
                    UpstreamRequest::BackendPage(_) => UpstreamResult::BackendPage {
                        received: Received {
                            destination: destination.unwrap(),
                            len,
                            checksum,
                        },
                        facts: facts.unwrap(),
                    },
                    _ => return Err(invalid("invalid GET result kind").into()),
                };
                Ok(match validation {
                    Some(retry) => ExchangeProgress::ReadyPeer { result, retry },
                    None => ExchangeProgress::Ready(result),
                })
            }
        })();
        if let Err(error) = &result {
            if peer
                && let Some(permit) = permit.as_ref()
                && let Some(failure) = error.attempt_failure()
                && !failure.reported
                && failure.evidence.as_ref().is_some_and(|e| {
                    e.endpoint.tcp() == Some(failure.route.endpoint) && e.owner_evidence()
                })
            {
                self.peer
                    .as_ref()
                    .unwrap()
                    .borrow_mut()
                    .record_repair_failure(permit);
            }
            if let Some(a) = attempt.as_mut()
                && let Some(failure) = error.attempt_failure()
                && failure.owner_evidence()
                && let Some(owner) = a.owner.take()
            {
                if failure.reported {
                    owner.failure(crate::environment::now());
                } else {
                    owner.transport_failure(crate::environment::now());
                }
            }
            if let Some(permit) = permit.take() {
                HttpOrigin::error(permit, error, peer);
            }
        }
        if let Some(permit) = permit {
            if peer {
                self.peer
                    .as_ref()
                    .unwrap()
                    .borrow_mut()
                    .clear_repair_failure(&permit);
            }
            permit.success();
        }
        result
    }

    pub(super) fn http_peer_attempt(
        &mut self,
        request: UpstreamRequest,
        destination: Option<Destination>,
        deadline: Instant,
        inherited: Option<Attempt>,
    ) -> cache::Result<Exchange> {
        let service_end = self.service_end(deadline);
        self.http_peer_attempt_until(request, destination, deadline, inherited, service_end)
    }

    fn http_peer_attempt_until(
        &mut self,
        request: UpstreamRequest,
        destination: Option<Destination>,
        deadline: Instant,
        inherited: Option<Attempt>,
        service_end: Instant,
    ) -> cache::Result<Exchange> {
        self.forward_available()?;
        let kind = metric_kind(&request);
        let (bytes, spent) = self.prepare_budget_wire(&request, service_end)?;
        let wire = hex(&bytes);
        let mut headers = vec![("X-Racer-Fault".to_owned(), wire.as_bytes().to_vec())];
        if let Some(auth) = request.authorization().as_str() {
            headers.push(("Authorization".into(), auth.as_bytes().to_vec()));
        }
        if let Some(volume) = &self.volume {
            headers.push(("X-Racer-Volume".into(), volume.as_bytes().to_vec()));
        }
        let mut nonce = [0; 16];
        crate::environment::random(&mut nonce).map_err(|e| io::Error::other(e.to_string()))?;
        let context = format!(
            "{}{}",
            hex(crate::authorization::binding(&bytes, request.authorization()).as_bytes()),
            hex(&nonce)
        );
        headers.push(("X-Racer-Attempt".to_owned(), context.as_bytes().to_vec()));
        let mut attempt = if let Some(mut inherited) = inherited {
            inherited.route.context = context.clone();
            Some(inherited)
        } else if let (Some(state), Some(routing), Some(peer)) =
            (&self.active, &self.routing, &self.peer)
        {
            let cursor = state.borrow().cursor.clone();
            let peer = peer.borrow();
            let candidate = routing.destination(&cursor);
            Some(Attempt {
                route: AttemptRoute {
                    cursor: cursor.clone(),
                    candidate,
                    endpoint: peer.http.endpoint.address.tcp().expect("TCP peer"),
                    final_hop: routing.last_hop(&cursor),
                    context: context.clone(),
                },
                owner: None,
            })
        } else {
            None
        };
        // An RDMA producer can finish before its HTTP recovery waiter becomes
        // producer. Retain actual generation/owner evidence briefly, rather than
        // interpreting a transport breaker rejection as new owner evidence.
        if let Some(a) = &attempt
            && self.owner_evidence(&a.route.cursor)
        {
            return Err(AttemptFailure {
                route: a.route.clone(),
                evidence: None,
                reported: true,
            }
            .into());
        }
        if let Some(a) = &mut attempt
            && a.owner.is_none()
        {
            a.owner = Some(
                self.owners.borrow_mut().acquire_final(
                    a.route.cursor.identity,
                    a.route.candidate,
                    self.routing
                        .as_ref()
                        .and_then(|r| r.final_peer(&a.route.cursor)),
                )?,
            );
        }
        let peer = self.peer.as_ref().ok_or(cache::Error::Unavailable)?.clone();
        let mut peer = peer.borrow_mut();
        if peer.http.breaker.active() + peer.breaker.active() >= peer.http.limit {
            return Err(cache::busy("direct peer exchange limit"));
        }
        let connection = peer.http.connection();
        let (connection, permit) =
            connection.map_err(|error| {
                if let Some(a) = &attempt {
                    let evidence = error.evidence().attempt.cloned().unwrap_or_else(|| {
                        crate::outcome::Failure {
                            endpoint: a.route.endpoint.into(),
                            transport: crate::outcome::Transport::Http,
                            phase: crate::outcome::Phase::LocalAdmission,
                            cause: if error.evidence().would_block {
                                crate::outcome::Cause::BreakerRejected
                            } else {
                                crate::outcome::Cause::Other
                            },
                            initiated: false,
                            kind: error.io_kind(),
                            message: error.to_string(),
                        }
                    });
                    cache::Error::from(AttemptFailure {
                        route: a.route.clone(),
                        evidence: Some(evidence),
                        reported: false,
                    })
                } else {
                    error.into()
                }
            })?;
        let mut permit = Some(permit);
        let result = (|| {
            let fields = headers
                .iter()
                .map(|(n, v)| {
                    Ok((
                        n.as_str(),
                        std::str::from_utf8(v).map_err(io::Error::other)?,
                    ))
                })
                .collect::<io::Result<Vec<_>>>()?;
            let request_wire = client::Request::new("/", &fields)?;
            let get = match destination {
                Some(destination) => HttpGet::Payload(
                    connection
                        .get(request_wire, destination, service_end.min(deadline))?
                        .service_deadline(self.private_service_deadline(service_end, deadline))
                        .connect_cap(COOLDOWN),
                ),
                None => HttpGet::Metadata(
                    connection
                        .get_small(
                            request_wire,
                            cache::METADATA_SIZE,
                            service_end.min(deadline),
                        )?
                        .service_deadline(self.private_service_deadline(service_end, deadline))
                        .connect_cap(COOLDOWN),
                ),
            };
            // Constructors above only build an exchange; submission first occurs
            // when it is polled. Admission/construction errors spend no authority.
            *self.chain.borrow_mut() = spent;
            Ok(Exchange::Get(GetPhase {
                exchange: get,
                request,
                permit: permit.take().unwrap(),
                attempt,
            }))
        })();
        if let Err(error) = &result {
            HttpOrigin::error(permit.take().unwrap(), error, true);
        } else {
            self.metrics
                .upstream(crate::metrics::Upstream::PeerHttp, kind);
        }
        result
    }
}
