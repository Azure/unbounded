//! Closed payload-control schema using the security-owned RFC 9421 signer.
use crate::{
    error::{Error, Result},
    http::codec::{Header, MessageHead, StartLine},
    model::identity::{NodeId, TransferId},
    runtime::deadline::RequestScope,
    security::{
        forwarding::ForwardedHead,
        protocol as p,
        signing::{Signatures, SignedHead, VerifiedHead, signed_digest},
    },
    topology::rails::RailId,
};
use std::sync::Arc;
pub(crate) const HEADER: &str = "racer-payload-control";
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Binding {
    pub request: [u8; 32],
    pub response: [u8; 32],
    pub transfer: TransferId,
    pub membership: u64,
    pub deadline: u64,
    pub rail: RailId,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Phase {
    Accept,
    Offer,
    Setup,
    Ready,
    Grant,
    Complete,
    Failed,
    Fallback,
    Done,
    Finish,
}
impl Phase {
    fn name(self) -> &'static str {
        match self {
            Self::Accept => "accept",
            Self::Offer => "offer",
            Self::Setup => "setup",
            Self::Ready => "ready",
            Self::Grant => "grant",
            Self::Complete => "complete",
            Self::Failed => "failed",
            Self::Fallback => "fallback",
            Self::Done => "done",
            Self::Finish => "finish",
        }
    }
    fn response(self) -> bool {
        matches!(
            self,
            Self::Offer | Self::Ready | Self::Complete | Self::Failed | Self::Finish
        )
    }
    fn fields(self) -> &'static [&'static str] {
        match self {
            Self::Offer => &["racer-rdma-setup"],
            Self::Setup | Self::Ready => &["racer-rdma-setup", "racer-rdma-setup-binding"],
            Self::Grant => &["racer-rdma-descriptor"],
            Self::Complete => &["racer-rdma-completion"],
            _ => &[],
        }
    }
}
impl Binding {
    pub fn request(
        auth: &ForwardedHead,
        membership: u64,
        scope: &RequestScope,
        rail: RailId,
    ) -> Result<Self> {
        let mut transfer = [0; 16];
        getrandom::getrandom(&mut transfer).map_err(|_| Error::Unavailable)?;
        Ok(Self {
            request: envelope_digest(auth)?,
            response: [0; 32],
            transfer: TransferId(transfer),
            membership,
            deadline: p::encode_deadline(scope.deadline)?,
            rail,
        })
    }
    fn head(
        &self,
        phase: Phase,
        previous: &[u8; 32],
        length: usize,
        extensions: Vec<Header>,
    ) -> Result<MessageHead> {
        if self.membership == 0
            || self.transfer.0 == [0; 16]
            || extensions.len() != phase.fields().len()
            || extensions
                .iter()
                .zip(phase.fields())
                .any(|(h, n)| h.name != *n)
        {
            return Err(Error::InvalidRequest);
        }
        for h in &extensions {
            if h.value.len() > 256 {
                return Err(Error::InvalidRequest);
            }
            p::decode_binary(&h.value)?;
        }
        if length > crate::model::range::PAGE_BYTES as usize + 16
            || (length != 0 && phase != Phase::Finish)
        {
            return Err(Error::InvalidRequest);
        }
        let mut h = MessageHead {
            start: if phase.response() {
                StartLine::Response { status: 200 }
            } else {
                StartLine::Request {
                    method: "POST".into(),
                    target: "/racer/peer/v1/payload".into(),
                }
            },
            headers: vec![],
        };
        p::push(&mut h, "content-length", length);
        p::push(&mut h, "racer-kind", format!("payload-v1-{}", phase.name()));
        for (n, b) in [
            ("racer-payload-request", self.request.as_slice()),
            ("racer-payload-response", self.response.as_slice()),
            ("racer-payload-transfer", self.transfer.0.as_slice()),
            ("racer-payload-previous", previous.as_slice()),
        ] {
            p::push_binary(&mut h, n, b);
        }
        p::push(&mut h, "racer-payload-membership", self.membership);
        p::push(&mut h, "racer-payload-deadline", self.deadline);
        p::push(&mut h, "racer-payload-rail", self.rail.0);
        h.headers.extend(extensions);
        Ok(h)
    }
    pub fn sign(
        &self,
        signatures: &Signatures,
        to: &NodeId,
        phase: Phase,
        previous: &[u8; 32],
        length: usize,
        extensions: Vec<Header>,
    ) -> Result<SignedHead> {
        let mut h = self.head(phase, previous, length, extensions)?;
        p::push(&mut h, "racer-receiver", &to.0);
        signatures.sign(h)
    }
    pub fn verify(
        &self,
        signatures: &Signatures,
        from: &NodeId,
        signed: SignedHead,
        allowed: &[Phase],
        previous: &[u8; 32],
        length: usize,
        scope: &RequestScope,
    ) -> Result<(VerifiedHead, Phase)> {
        scope.check()?;
        if p::decode_deadline(self.deadline)?.0 <= std::time::Instant::now() {
            return Err(Error::DeadlineExceeded);
        }
        let kind = p::field(&signed.head, "racer-kind")?;
        let phase = *allowed
            .iter()
            .find(|phase| kind == format!("payload-v1-{}", phase.name()))
            .ok_or(Error::Unauthorized)?;
        let extensions = phase
            .fields()
            .iter()
            .map(|name| {
                Ok(Header {
                    name: (*name).into(),
                    value: signed
                        .head
                        .unique(name)?
                        .ok_or(Error::Unauthorized)?
                        .to_vec(),
                })
            })
            .collect::<Result<Vec<_>>>()?;
        p::agrees(
            &signed.head,
            &self.head(phase, previous, length, extensions)?,
            false,
        )?;
        if crate::security::signing::node_field(&signed.head, "racer-signer")? != *from {
            return Err(Error::Unauthorized);
        }
        Ok((signatures.verify(signed)?, phase))
    }
    pub fn parse_accept(signed: &SignedHead) -> Result<Self> {
        fn a<const N: usize>(h: &MessageHead, n: &str) -> Result<[u8; N]> {
            p::decode_binary(p::field(h, n)?.as_bytes())?
                .try_into()
                .map_err(|_| Error::InvalidRequest)
        }
        let h = &signed.head;
        Ok(Self {
            request: a(h, "racer-payload-request")?,
            response: a(h, "racer-payload-response")?,
            transfer: TransferId(a(h, "racer-payload-transfer")?),
            membership: p::number(h, "racer-payload-membership")?,
            deadline: p::number(h, "racer-payload-deadline")?,
            rail: RailId(
                p::number(h, "racer-payload-rail")?
                    .try_into()
                    .map_err(|_| Error::InvalidRequest)?,
            ),
        })
    }
}
/// Bind all original/hop signatures using security's canonical digest, never bodies.
pub(crate) fn envelope_digest(auth: &ForwardedHead) -> Result<[u8; 32]> {
    use sha2::{Digest, Sha256};
    let mut hash = Sha256::new();
    hash.update(b"racer-peer-v1/payload-envelope\0");
    hash.update((auth.hops.len() as u64).to_be_bytes());
    hash.update(signed_digest(&auth.original)?);
    for h in &auth.hops {
        hash.update(signed_digest(h)?);
    }
    Ok(hash.finalize().into())
}
pub(crate) fn attach(head: &mut MessageHead, signed: &SignedHead) -> Result<()> {
    head.headers.push(Header {
        name: HEADER.into(),
        value: super::wire::encode_signed(signed)?,
    });
    Ok(())
}
pub(crate) fn detach(head: &mut MessageHead) -> Result<Option<SignedHead>> {
    let value = head.unique(HEADER)?.map(|v| v.to_vec());
    head.headers
        .retain(|h| !h.name.eq_ignore_ascii_case(HEADER));
    value.map(|v| super::wire::decode_signed(&v)).transpose()
}
pub(crate) fn frame(signed: SignedHead) -> Result<MessageHead> {
    let response = matches!(signed.head.start, StartLine::Response { .. });
    super::wire::WireCodec::encode(
        &ForwardedHead {
            original: Arc::new(signed),
            hops: vec![],
        },
        response,
        0,
    )
}
pub(crate) fn unframe(head: MessageHead, response: bool) -> Result<SignedHead> {
    let (auth, len) = super::wire::WireCodec::decode(head, response)?;
    if len != 0 || !auth.hops.is_empty() {
        return Err(Error::InvalidRequest);
    }
    Arc::try_unwrap(auth.original).map_err(|_| Error::InvalidRequest)
}
pub(crate) fn extension(name: &str, value: Vec<u8>) -> Header {
    Header {
        name: name.into(),
        value,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn binding(scope: &RequestScope) -> Binding {
        Binding {
            request: [1; 32],
            response: [2; 32],
            transfer: TransferId([3; 16]),
            membership: 1,
            deadline: p::encode_deadline(scope.deadline).unwrap(),
            rail: RailId(7),
        }
    }
    #[test]
    fn exact_signed_controls_reject_every_binding_substitution_and_unknown_field() {
        let nodes = crate::peer::tests::signers();
        let scope = RequestScope::new(
            crate::model::identity::RequestId([4; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(30),
        )
        .unwrap();
        let original = binding(&scope);
        for mutation in 0..9 {
            let signed = original
                .sign(
                    &nodes[0],
                    nodes[1].node(),
                    Phase::Fallback,
                    &[9; 32],
                    0,
                    vec![],
                )
                .unwrap();
            let mut expected = original.clone();
            match mutation {
                0 => expected.request[0] ^= 1,
                1 => expected.response[0] ^= 1,
                2 => expected.transfer.0[0] ^= 1,
                3 => expected.membership += 1,
                4 => expected.deadline += 1,
                5 => expected.rail.0 += 1,
                _ => {}
            }
            let previous = if mutation == 6 { [8; 32] } else { [9; 32] };
            let phase = if mutation == 7 {
                Phase::Done
            } else {
                Phase::Fallback
            };
            let peer = if mutation == 8 {
                nodes[2].node()
            } else {
                nodes[0].node()
            };
            assert!(
                expected
                    .verify(&nodes[1], peer, signed, &[phase], &previous, 0, &scope)
                    .is_err()
            );
        }
        let mut head = original.head(Phase::Fallback, &[9; 32], 0, vec![]).unwrap();
        p::push(&mut head, "racer-receiver", &nodes[1].node().0);
        p::push(&mut head, "racer-extra", 1);
        assert!(
            original
                .verify(
                    &nodes[1],
                    nodes[0].node(),
                    nodes[0].sign(head).unwrap(),
                    &[Phase::Fallback],
                    &[9; 32],
                    0,
                    &scope
                )
                .is_err()
        );
        let signed = original
            .sign(
                &nodes[0],
                nodes[1].node(),
                Phase::Fallback,
                &[9; 32],
                0,
                vec![],
            )
            .unwrap();
        let copy =
            super::super::wire::decode_signed(&super::super::wire::encode_signed(&signed).unwrap())
                .unwrap();
        original
            .verify(
                &nodes[1],
                nodes[0].node(),
                signed,
                &[Phase::Fallback],
                &[9; 32],
                0,
                &scope,
            )
            .unwrap();
        assert!(matches!(
            original.verify(
                &nodes[1],
                nodes[0].node(),
                copy,
                &[Phase::Fallback],
                &[9; 32],
                0,
                &scope
            ),
            Err(Error::Replay)
        ));
    }
    #[test]
    fn control_extension_and_fallback_length_schema_is_closed() {
        let scope = RequestScope::new(
            crate::model::identity::RequestId([4; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(30),
        )
        .unwrap();
        let binding = binding(&scope);
        assert!(binding.head(Phase::Offer, &[0; 32], 0, vec![]).is_err());
        assert!(
            binding
                .head(
                    Phase::Complete,
                    &[0; 32],
                    0,
                    vec![extension("racer-rdma-completion", b"bad".to_vec())]
                )
                .is_err()
        );
        assert!(
            binding
                .head(
                    Phase::Grant,
                    &[0; 32],
                    17,
                    vec![extension(
                        "racer-rdma-descriptor",
                        p::binary(&[0; 32]).into_bytes()
                    )]
                )
                .is_err()
        );
        assert!(binding.head(Phase::Finish, &[0; 32], 17, vec![]).is_ok());
        assert!(
            binding
                .head(Phase::Finish, &[0; 32], usize::MAX, vec![])
                .is_err()
        );
    }
}
