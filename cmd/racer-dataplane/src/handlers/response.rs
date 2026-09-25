// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Client representation preconditions, ranges, and response streaming.
use super::*;
use crate::http::trim;

// RFC 9110 entity-tag lists are byte strings, not quoted-string values: commas
// and backslashes inside a tag are literal, and obs-text need not be UTF-8.
// Repeated field lines combine as a list. Empty list members are tolerated;
// wildcard is only valid as the entire field value, never a list member.
pub(super) fn matches(
    headers: Headers<'_>,
    name: &str,
    current: &[u8],
    strong: bool,
) -> io::Result<Option<bool>> {
    let mut present = false;
    let mut wildcard = false;
    let mut matched = false;
    let mut members = 0;
    for (_, value) in headers.iter().filter(|(n, _)| n.eq_ignore_ascii_case(name)) {
        if wildcard {
            return Err(invalid("wildcard in entity-tag list"));
        }
        let mut rest = trim(value);
        if rest == b"*" {
            if present {
                return Err(invalid("wildcard in entity-tag list"));
            }
            wildcard = true;
        } else {
            while !rest.is_empty() {
                if rest[0] == b',' {
                    rest = trim(&rest[1..]);
                    continue;
                }
                let weak = rest.starts_with(b"W/");
                if weak {
                    rest = &rest[2..];
                }
                if rest.first() != Some(&b'"') {
                    return Err(invalid("invalid entity-tag"));
                }
                let end = rest[1..]
                    .iter()
                    .position(|b| *b == b'"')
                    .map(|n| n + 2)
                    .ok_or_else(|| invalid("unterminated entity-tag"))?;
                let tag = &rest[..end];
                if !tag[1..end - 1].iter().all(|b| *b >= 0x21 && *b != 0x7f) {
                    return Err(invalid("invalid entity-tag bytes"));
                }
                members += 1;
                matched |= if strong {
                    !weak && tag == current
                } else {
                    tag == current.strip_prefix(b"W/").unwrap_or(current)
                };
                rest = trim(&rest[end..]);
                if !rest.is_empty() {
                    if rest[0] != b',' {
                        return Err(invalid("invalid entity-tag separator"));
                    }
                    rest = trim(&rest[1..]);
                }
            }
        }
        present = true;
    }
    if present && !wildcard && members == 0 {
        return Err(invalid("empty entity-tag list"));
    }
    Ok(present.then_some(wildcard || matched))
}

impl Task {
    pub(super) fn poll_response(
        &mut self,
        ring: &mut Ring,
        mut page_work: Work,
    ) -> io::Result<Progress<http::Completed>> {
        if self.head.is_some() {
            if self.end != 0
                && !matches!(self.response, Response::Request(http::Request::Head(_)))
                && !matches!(self.pages.front(), Some((_, PageLoad::Ready(_))))
            {
                return Ok(Progress::Pending(page_work));
            }
            let PendingHead {
                status,
                len,
                headers,
            } = self.head.take().unwrap();
            let headers: Vec<_> = headers
                .iter()
                .map(|(n, v)| (n.as_str(), v.as_slice()))
                .collect();
            self.respond(status, len, &headers)?;
        }
        let state = std::mem::replace(&mut self.response, Response::Done);
        let progress = match state {
            Response::Head(mut headers) => {
                let result = headers.poll(ring, 1)?;
                if matches!(result, Progress::Pending(_)) {
                    self.response = Response::Head(headers);
                } else {
                    self.sent_headers(&self.upstream.metrics.clone());
                }
                return Ok(result);
            }
            Response::Headers(mut headers) => match headers.poll(ring, 1)? {
                Progress::Pending(work) => {
                    self.response = Response::Headers(headers);
                    page_work.merge(work);
                    return Ok(Progress::Pending(page_work));
                }
                Progress::Ready(progress) => {
                    self.sent_headers(&self.upstream.metrics.clone());
                    progress
                }
            },
            Response::Body(mut body) => match body.poll(ring, 1)? {
                Progress::Pending(work) => {
                    self.response = Response::Body(body);
                    page_work.merge(work);
                    return Ok(Progress::Pending(page_work));
                }
                Progress::Ready(progress) => progress,
            },
            Response::Writer(writer) => {
                if matches!(self.pages.front(), Some((_, PageLoad::Ready(_)))) {
                    let (offset, PageLoad::Ready(buffer)) = self.pages.pop_front().unwrap() else {
                        unreachable!()
                    };
                    let start = (self.position - offset) as usize;
                    let len = (buffer.len() - start)
                        .min((self.end - self.position).min(usize::MAX as u64) as usize);
                    self.position += len as u64;
                    // Benchmark fidelity: bench/tcp.rs uses this same dispatch
                    // for inline metadata, registered buffers, and file bodies.
                    let chunk =
                        http::BodyChunk::value(buffer, start..start + len).map_err(|e| e.error)?;
                    let mut body = writer.send(chunk).map_err(|e| e.error)?;
                    let context = if self.peer {
                        self.body_context.take()
                    } else {
                        self.metadata.as_ref().and_then(|meta| {
                            meta.page_key(offset).ok().and_then(|key| {
                                self.upstream
                                    .body_context(key, Some(meta.checksum()), Some(offset))
                            })
                        })
                    };
                    if let Some(mut context) = context {
                        // Client Task's provider belongs to metadata; page
                        // providers have already completed. Do not misattribute
                        // the sent page to the metadata route/flight.
                        if !self.peer {
                            context["route"] = serde_json::Value::Null;
                            context["flight"] = serde_json::Value::Null;
                        }
                        body.set_diagnostic(context);
                    }
                    self.response = Response::Body(body);
                    return Ok(Progress::Pending(runnable()));
                }
                self.response = Response::Writer(writer);
                return Ok(Progress::Pending(page_work));
            }
            _ => return Err(invalid("invalid HTTP task state")),
        };
        match progress {
            http::BodyProgress::More(writer) => {
                self.response = Response::Writer(writer);
                Ok(Progress::Pending(runnable()))
            }
            http::BodyProgress::Done(done) => Ok(Progress::Ready(done)),
        }
    }

    pub(super) fn prepare(&mut self, meta: Metadata) -> io::Result<()> {
        let Response::Request(request) = &self.response else {
            return Err(invalid("missing request"));
        };
        // Admit the representation before HEAD, preconditions, ranges, cache
        // hits or page faults can produce ownership-dependent client results.
        if self.distributed && !cache::peer_wire::client_fits(meta.target().len()) {
            return self.respond(422, 0, &[]);
        }
        // Validate both fields before evaluating them in protocol order. Syntax
        // errors are client 400s, not upstream failures. Metadata proves existence
        // for wildcard comparisons.
        let etag = meta.etag();
        let etag = etag.as_str();
        let conditions =
            matches(request.headers(), "if-match", etag.as_bytes(), true).and_then(|m| {
                matches(request.headers(), "if-none-match", etag.as_bytes(), false).map(|n| (m, n))
            });
        let mut headers = vec![("Accept-Ranges", b"bytes".as_slice())];
        headers.push(("ETag", etag.as_bytes()));
        if let Some(content_type) = meta.content_type() {
            headers.push(("Content-Type", content_type));
        }
        match conditions {
            Err(_) => return self.respond(400, 0, &[]),
            Ok((Some(false), _)) => return self.respond(412, 0, &headers),
            // 304's Content-Length describes the full selected representation;
            // transport suppresses its body, and end stays zero (no prefetch).
            Ok((_, Some(true))) => return self.respond(304, meta.len(), &headers),
            _ => {}
        }
        let mut status = 200;
        let mut content_range = String::new();
        self.end = meta.len();
        if matches!(request, http::Request::Get(_)) {
            let if_range = text(request.headers(), "if-range")?;
            if if_range.is_none() || if_range == Some(etag) {
                match http::resolve_range(request.headers(), meta.len()) {
                    http::RangeSelection::Full => {}
                    http::RangeSelection::Partial(range) => {
                        status = 206;
                        self.position = range.start();
                        self.end = range.end();
                        content_range = range.content_range().to_string();
                    }
                    http::RangeSelection::Unsatisfiable => {
                        status = 416;
                        self.end = 0;
                        content_range = format!("bytes */{}", meta.len());
                    }
                }
            }
        }
        self.next = self.position / BUFFER_SIZE as u64 * BUFFER_SIZE as u64;
        if !content_range.is_empty() {
            headers.push(("Content-Range", content_range.as_bytes()));
        }
        self.head = Some(PendingHead {
            status,
            len: self.end - self.position,
            headers: headers
                .into_iter()
                .map(|(n, v)| (n.to_owned(), v.to_vec()))
                .collect(),
        });
        self.metadata = Some(meta);
        Ok(())
    }
}
