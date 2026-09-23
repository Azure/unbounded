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
    pub(super) fn poll(
        &mut self,
        ring: &mut Ring,
        budget: usize,
    ) -> io::Result<Progress<HttpResponse>> {
        Ok(match self {
            Self::Payload(e) => match e.poll(ring, budget)? {
                Progress::Pending(w) => Progress::Pending(w),
                Progress::Ready(r) => Progress::Ready(HttpResponse::Payload(r)),
            },
            Self::Metadata(e) => match e.poll(ring, budget)? {
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
        let result = (|| match exchange.poll(ring, 1)? {
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
        let result = (|| match exchange.poll(ring, 1).map_err(|error| {
            if let Some(attempt) = &attempt {
                let evidence = error
                    .get_ref()
                    .and_then(|e| e.downcast_ref::<client::attempt::Failure>())
                    .cloned();
                cache::Error::Io(io::Error::other(AttemptFailure {
                    route: attempt.route.clone(),
                    evidence,
                    reported: false,
                }))
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
                        return Err(io::Error::other(AttemptFailure {
                            route: a.route.clone(),
                            evidence: None,
                            reported: true,
                        })
                        .into());
                    }
                    return Err(status(response.status()));
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
            if let Some(a) = attempt.as_mut()
                && let Some(failure) = error_detail::<AttemptFailure>(error)
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
        let kind = metric_kind(&request);
        let service_end = self.service_end(deadline);
        let bytes = self.budget_wire(&request, service_end)?;
        let wire = hex(&bytes);
        let mut headers = vec![("X-Racer-Fault".to_owned(), wire.as_bytes().to_vec())];
        if let Some(volume) = &self.volume {
            headers.push(("X-Racer-Volume".into(), volume.as_bytes().to_vec()));
        }
        let mut nonce = [0; 16];
        crate::environment::random(&mut nonce).map_err(|e| io::Error::other(e.to_string()))?;
        let context = format!("{}{}", hex(blake3::hash(&bytes).as_bytes()), hex(&nonce));
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
            return Err(io::Error::other(AttemptFailure {
                route: a.route.clone(),
                evidence: None,
                reported: true,
            })
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
        let (connection, permit) = connection.map_err(|error| {
            if let Some(a) = &attempt {
                let evidence = error
                    .get_ref()
                    .and_then(|e| e.downcast_ref::<client::attempt::Failure>())
                    .cloned()
                    .unwrap_or_else(|| client::attempt::Failure {
                        endpoint: a.route.endpoint.into(),
                        transport: client::attempt::Transport::Http,
                        phase: client::attempt::Phase::LocalAdmission,
                        cause: if error.kind() == io::ErrorKind::WouldBlock {
                            client::attempt::Cause::BreakerRejected
                        } else {
                            client::attempt::Cause::Other
                        },
                        initiated: false,
                        kind: error.kind(),
                        message: error.to_string(),
                    });
                cache::Error::Io(io::Error::other(AttemptFailure {
                    route: a.route.clone(),
                    evidence: Some(evidence),
                    reported: false,
                }))
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
                        .retry_idle_peer(&peer.http, &self.metrics)
                        .service_deadline(service_end < deadline)
                        .connect_cap(COOLDOWN),
                ),
                None => HttpGet::Metadata(
                    connection
                        .get_small(
                            request_wire,
                            cache::METADATA_SIZE,
                            service_end.min(deadline),
                        )?
                        .retry_idle_peer(&peer.http, &self.metrics)
                        .service_deadline(service_end < deadline)
                        .connect_cap(COOLDOWN),
                ),
            };
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
