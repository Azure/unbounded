//! Certificate-authenticated capabilities and signed RDMA setup over HTTP.
use crate::{
    error::{Error, Operation, Result},
    model::identity::NodeId,
    rdma::session::Sessions,
    runtime::deadline::RequestScope,
    security::signing::Signatures,
};
use std::{cell::RefCell, collections::HashMap, rc::Rc, time::Instant};

/// The security implementation owns challenge establishment, certificate checks,
/// and signing bytes. Negotiation never trusts unsigned capability advertisements.
pub trait HandshakeExchange {
    fn negotiate<'a>(
        &'a self,
        peer: &'a NodeId,
        scope: &'a RequestScope,
    ) -> Operation<'a, AuthenticatedCapabilities>;
}

/// Constructed only after verifying the peer's signed capability answer. Session
/// setup retains the verified identity and complete signed QP parameters.
pub struct AuthenticatedCapabilities {
    pub peer: crate::security::certificates::VerifiedPeer,
    pub capabilities: Capabilities,
    pub setup: Option<crate::rdma::session::SetupParameters>,
    pub expires: Instant,
}

pub struct Handshake {
    signatures: Rc<Signatures>,
    rdma: Option<Rc<Sessions>>,
    exchange: Option<Rc<dyn HandshakeExchange>>,
    cache: RefCell<HashMap<NodeId, (Capabilities, Instant)>>,
    capacity: usize,
    http: Option<(Rc<super::PeerNetwork>, Rc<super::transfer::Transfers>)>,
    discovery: Option<(
        Rc<crate::security::keyring::Keyring>,
        Rc<crate::security::certificates::Certificates>,
        Rc<crate::security::replay::ReplayWindow>,
    )>,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Capabilities {
    pub rdma: bool,
    pub scoped_grants: bool,
}
impl Handshake {
    pub fn new(signatures: Rc<Signatures>, rdma: Option<Rc<Sessions>>) -> Self {
        Self {
            signatures,
            rdma,
            exchange: None,
            cache: RefCell::new(HashMap::new()),
            capacity: 36,
            http: None,
            discovery: None,
        }
    }
    pub fn with_discovery(
        mut self,
        keys: Rc<crate::security::keyring::Keyring>,
        certificates: Rc<crate::security::certificates::Certificates>,
        replay: Rc<crate::security::replay::ReplayWindow>,
    ) -> Self {
        self.discovery = Some((keys, certificates, replay));
        self
    }
    pub fn respond_probe(&self, bytes: &[u8]) -> Result<Vec<u8>> {
        let (keys, _, replay) = self.discovery.as_ref().ok_or(Error::InvalidConfiguration)?;
        crate::security::session::respond(keys, replay, bytes)?.encode()
    }
    pub fn with_http(
        mut self,
        network: Rc<super::PeerNetwork>,
        transfers: Rc<super::transfer::Transfers>,
    ) -> Self {
        self.http = Some((network, transfers));
        self
    }
    /// Prepare a QP only for a certificate-authenticated neighbor and a rail chosen
    /// by the full-path rail planner. The returned setup must be included in a
    /// signed control request; finish_session validates the signed peer answer.
    pub fn prepare_session<'a>(
        &'a self,
        peer: &'a crate::security::certificates::VerifiedPeer,
        rail: crate::topology::rails::RailId,
        scope: &'a RequestScope,
    ) -> Operation<'a, crate::rdma::session::PreparedSession> {
        Box::pin(async move {
            self.rdma
                .as_ref()
                .ok_or(Error::Unavailable)?
                .prepare(peer, rail, scope)
                .await
        })
    }

    pub fn finish_session<'a>(
        &'a self,
        prepared: crate::rdma::session::PreparedSession,
        signed: crate::security::signing::SignedHead,
        expected_request: &'a crate::security::signing::SignedHead,
        scope: &'a RequestScope,
    ) -> Operation<'a, crate::rdma::session::SessionLease> {
        Box::pin(async move {
            scope.check()?;
            let verified = self.signatures.verify(signed)?;
            let binding = crate::security::signing::signed_digest(expected_request)?;
            let actual = crate::security::protocol::decode_binary(
                crate::security::protocol::field(&verified.signed.head, "racer-request-binding")?
                    .as_bytes(),
            )?;
            if actual != binding {
                return Err(Error::Unauthorized);
            }
            prepared.finish(&verified, scope).await
        })
    }

    /// Discover the receiver challenge using the security-owned fresh-probe
    /// protocol, then prove capabilities and exact request correlation.
    pub fn negotiate_at<'a>(
        &'a self,
        peer: &'a NodeId,
        membership: crate::model::identity::MembershipVersion,
        scope: &'a RequestScope,
    ) -> Operation<'a, Capabilities> {
        Box::pin(async move {
            use crate::http::codec::{MessageHead, StartLine};
            use crate::security::{protocol as p, signing::signed_digest};
            scope.check()?;
            let (network, transfers) = self.http.as_ref().ok_or(Error::InvalidConfiguration)?;
            if let Some((_, certificates, _)) = &self.discovery {
                let probe = crate::security::session::ChallengeProbe::new(
                    network.local.clone(),
                    peer.clone(),
                )?;
                let reply = transfers
                    .exchange_probe(
                        network.endpoint(membership, peer)?,
                        probe.request_bytes(),
                        scope,
                    )
                    .await?;
                let challenge = probe.verify(
                    certificates,
                    crate::security::session::ChallengeReply::decode(&reply)?,
                )?;
                self.signatures.install_peer_challenge(challenge)?;
            }
            let mut head = MessageHead {
                start: StartLine::Request {
                    method: "POST".into(),
                    target: "/racer/peer/v1/handshake".into(),
                },
                headers: Vec::new(),
            };
            p::push(&mut head, "content-length", 0);
            p::push(&mut head, "racer-kind", "handshake");
            p::push(&mut head, "racer-wire-version", super::wire::VERSION);
            p::push(&mut head, "racer-membership", membership.0);
            p::push(&mut head, "racer-receiver", &peer.0);
            p::push_binary(
                &mut head,
                "racer-session-challenge",
                &self.signatures.challenge()?,
            );
            let signed = self.signatures.sign(head)?;
            let binding = signed_digest(&signed)?;
            let response = transfers
                .exchange_head(network.endpoint(membership, peer)?, signed, scope)
                .await?;
            let verified = self.signatures.verify(response)?;
            let head = &verified.signed.head;
            if verified.peer.node() != peer
                || !matches!(head.start, StartLine::Response { status: 200 })
                || p::field(head, "racer-kind")? != "handshake-response"
                || p::field(head, "racer-wire-version")? != super::wire::VERSION
                || p::number(head, "racer-membership")? != membership.0
                || p::decode_binary(p::field(head, "racer-request-binding")?.as_bytes())? != binding
            {
                return Err(Error::Unauthorized);
            }
            let challenge =
                p::decode_binary(p::field(head, "racer-session-challenge")?.as_bytes())?
                    .try_into()
                    .map_err(|_| Error::InvalidRequest)?;
            self.signatures
                .configure_authenticated_peer_challenge(peer.clone(), challenge)?;
            let capabilities = Capabilities {
                rdma: bit(head, "racer-rdma")?,
                scoped_grants: bit(head, "racer-scoped-grants")?,
            };
            if capabilities.rdma && !capabilities.scoped_grants {
                return Err(Error::InvalidRequest);
            }
            scope.check()?;
            self.cache
                .borrow_mut()
                .retain(|_, (_, expiry)| *expiry > Instant::now());
            if self.cache.borrow().len() >= self.capacity && !self.cache.borrow().contains_key(peer)
            {
                return Err(Error::Overloaded);
            }
            self.cache
                .borrow_mut()
                .insert(peer.clone(), (capabilities, scope.deadline.0));
            Ok(capabilities)
        })
    }

    pub fn respond(
        &self,
        signed: crate::security::signing::SignedHead,
    ) -> Result<crate::security::signing::SignedHead> {
        use crate::http::codec::{MessageHead, StartLine};
        use crate::security::{protocol as p, signing::signed_digest};
        let binding = signed_digest(&signed)?;
        let verified = self.signatures.verify(signed)?;
        let head = &verified.signed.head;
        if !matches!(&head.start, StartLine::Request { method, target } if method == "POST" && target == "/racer/peer/v1/handshake")
            || p::field(head, "racer-kind")? != "handshake"
            || p::field(head, "racer-wire-version")? != super::wire::VERSION
        {
            return Err(Error::InvalidRequest);
        }
        let membership =
            crate::model::identity::MembershipVersion(p::number(head, "racer-membership")?);
        let (network, _) = self.http.as_ref().ok_or(Error::InvalidConfiguration)?;
        network.endpoint(membership, verified.peer.node())?;
        let challenge = p::decode_binary(p::field(head, "racer-session-challenge")?.as_bytes())?
            .try_into()
            .map_err(|_| Error::InvalidRequest)?;
        self.signatures
            .configure_authenticated_peer_challenge(verified.peer.node().clone(), challenge)?;
        let mut response = MessageHead {
            start: StartLine::Response { status: 200 },
            headers: Vec::new(),
        };
        p::push(&mut response, "content-length", 0);
        p::push(&mut response, "racer-kind", "handshake-response");
        p::push(&mut response, "racer-wire-version", super::wire::VERSION);
        p::push(&mut response, "racer-membership", membership.0);
        p::push(&mut response, "racer-receiver", &verified.peer.node().0);
        p::push_binary(&mut response, "racer-request-binding", &binding);
        p::push_binary(
            &mut response,
            "racer-session-challenge",
            &self.signatures.challenge()?,
        );
        let native_ready = self.rdma.as_ref().is_some_and(|sessions| {
            network
                .membership(membership)
                .ok()
                .and_then(|members| {
                    members
                        .members()
                        .iter()
                        .find(|m| m.node == network.local)
                        .cloned()
                })
                .is_some_and(|member| {
                    member.alignment_enabled
                        && member.rails.iter().any(|rail| sessions.ready(rail.rail))
                })
        });
        p::push(&mut response, "racer-rdma", u8::from(native_ready));
        p::push(&mut response, "racer-scoped-grants", u8::from(native_ready));
        self.signatures.sign(response)
    }
    pub fn with_exchange(
        mut self,
        exchange: Rc<dyn HandshakeExchange>,
        capacity: usize,
    ) -> Result<Self> {
        if capacity == 0 {
            return Err(Error::InvalidConfiguration);
        }
        self.exchange = Some(exchange);
        self.capacity = capacity;
        Ok(self)
    }
    pub fn invalidate(&self, peer: &NodeId) {
        self.cache.borrow_mut().remove(peer);
        self.signatures.remove_peer_challenge(peer);
    }
    /// Legacy callers must use negotiate_scoped to preserve their request deadline.
    pub fn negotiate<'a>(&'a self, peer: &'a NodeId) -> Operation<'a, Capabilities> {
        Box::pin(async move {
            self.cache
                .borrow()
                .get(peer)
                .filter(|(_, expiry)| *expiry > Instant::now())
                .map(|(capabilities, _)| *capabilities)
                .ok_or(Error::Unavailable)
        })
    }
    pub fn negotiate_scoped<'a>(
        &'a self,
        peer: &'a NodeId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Capabilities> {
        Box::pin(async move {
            scope.check()?;
            if let Some((capabilities, _)) = self
                .cache
                .borrow()
                .get(peer)
                .filter(|(_, expiry)| *expiry > Instant::now())
            {
                return Ok(*capabilities);
            }
            self.cache
                .borrow_mut()
                .retain(|_, (_, expiry)| *expiry > Instant::now());
            if self.cache.borrow().len() >= self.capacity {
                return Err(Error::Overloaded);
            }
            let result = self
                .exchange
                .as_ref()
                .ok_or(Error::InvalidConfiguration)?
                .negotiate(peer, scope)
                .await?;
            scope.check()?;
            if result.peer.node() != peer || result.expires <= Instant::now() {
                return Err(Error::Unauthorized);
            }
            let capabilities = result.capabilities;
            if capabilities.rdma && (!capabilities.scoped_grants || result.setup.is_none()) {
                return Err(Error::InvalidRequest);
            }
            self.cache
                .borrow_mut()
                .insert(peer.clone(), (capabilities, result.expires));
            Ok(capabilities)
        })
    }
}
fn bit(head: &crate::http::codec::MessageHead, field: &str) -> Result<bool> {
    match crate::security::protocol::number(head, field)? {
        0 => Ok(false),
        1 => Ok(true),
        _ => Err(Error::InvalidRequest),
    }
}
#[cfg(test)]
mod tests { /* Capability tampering, QP/rail binding, expired certs, downgrade policy. */
}
