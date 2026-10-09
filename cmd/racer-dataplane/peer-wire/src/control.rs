//! Signed control-phase chains; native transport state and policy remain outside.

use crate::*;
use racer_control_wire::RailId;

/// Exact signed correlation fields shared by every phase of a payload exchange.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Binding {
    /// Digest of the complete request proof envelope.
    pub request: [u8; 32],

    /// Digest of the complete response proof envelope.
    pub response: [u8; 32],

    /// Nonzero caller-selected transfer identifier.
    pub transfer: [u8; 16],

    /// Nonzero authenticated membership epoch.
    pub membership: u64,

    /// Absolute signed deadline in milliseconds.
    pub deadline: u64,

    /// Selected transport rail.
    pub rail: RailId,
}

/// Wire phase only; the caller chooses allowed transitions and executes transport.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Phase {
    /// Request transport acceptance.
    Accept,
    /// Offer transport setup.
    Offer,
    /// Bind remote setup.
    Setup,
    /// Confirm setup readiness.
    Ready,
    /// Grant a destination descriptor.
    Grant,
    /// Confirm native completion.
    Complete,
    /// Report native failure.
    Failed,
    /// Request HTTP fallback.
    Fallback,
    /// Acknowledge completion.
    Done,
    /// Finish with optional fallback body.
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

    /// Whether the wire phase uses an HTTP response start line.
    pub fn response(self) -> bool {
        matches!(
            self,
            Self::Offer | Self::Ready | Self::Complete | Self::Failed | Self::Finish
        )
    }

    /// Ordered extension fields required by this phase.
    pub fn fields(self) -> &'static [&'static str] {
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
    /// Construct the canonical unsigned phase head.
    pub fn head(
        &self,
        phase: Phase,
        previous: &[u8; 32],
        length: usize,
        extensions: Vec<Header>,
    ) -> Result<MessageHead> {
        if self.membership == 0
            || self.transfer == [0; 16]
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
            decode_binary(&h.value)?;
        }
        if length > MAX_BODY || (length != 0 && phase != Phase::Finish) {
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
        push(&mut h, "content-length", length);
        push(&mut h, "racer-kind", format!("payload-v1-{}", phase.name()));
        for (n, b) in [
            ("racer-payload-request", self.request.as_slice()),
            ("racer-payload-response", self.response.as_slice()),
            ("racer-payload-transfer", self.transfer.as_slice()),
            ("racer-payload-previous", previous.as_slice()),
        ] {
            push_binary(&mut h, n, b);
        }
        push(&mut h, "racer-payload-membership", self.membership);
        push(&mut h, "racer-payload-deadline", self.deadline);
        push(&mut h, "racer-payload-rail", self.rail.0);
        h.headers.extend(extensions);
        Ok(h)
    }

    /// Sign the next phase against the previous exact signed-head digest.
    #[allow(clippy::too_many_arguments)]
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
        push(&mut h, "racer-receiver", &to.0);
        signatures.sign(h)
    }

    /// Check deadline, phase, exact correlation fields, signer, and proof in order.
    /// The caller must check its cancellation scope before invoking this method.
    #[allow(clippy::too_many_arguments)]
    pub fn verify(
        &self,
        signatures: &Signatures,
        from: &NodeId,
        signed: SignedHead,
        allowed: &[Phase],
        previous: &[u8; 32],
        length: usize,
    ) -> Result<(VerifiedHead, Phase)> {
        if decode_deadline(self.deadline)?.0 <= uring_runtime::environment::now() {
            return Err(Error::DeadlineExceeded);
        }
        let kind = field(&signed.head, "racer-kind")?;
        let phase = *allowed
            .iter()
            .find(|phase| kind == format!("payload-v1-{}", phase.name()))
            .ok_or(Error::Unauthorized)?;
        let extensions = phase
            .fields()
            .iter()
            .map(|name| {
                Ok(Header::new(
                    *name,
                    signed.head.unique(name)?.ok_or(Error::Unauthorized)?,
                ))
            })
            .collect::<Result<Vec<_>>>()?;
        agrees(
            &signed.head,
            &self.head(phase, previous, length, extensions)?,
            false,
        )?;
        if node_field(&signed.head, "racer-signer")? != *from {
            return Err(Error::Unauthorized);
        }
        Ok((signatures.verify_proof(signed)?, phase))
    }

    /// Parse unverified acceptance fields; parsing alone grants no authority.
    pub fn parse_accept(signed: &SignedHead) -> Result<Self> {
        fn a<const N: usize>(h: &MessageHead, n: &str) -> Result<[u8; N]> {
            decode_binary(field(h, n)?.as_bytes())?
                .try_into()
                .map_err(|_| Error::InvalidRequest)
        }
        let h = &signed.head;
        Ok(Self {
            request: a(h, "racer-payload-request")?,
            response: a(h, "racer-payload-response")?,
            transfer: a(h, "racer-payload-transfer")?,
            membership: number(h, "racer-payload-membership")?,
            deadline: number(h, "racer-payload-deadline")?,
            rail: RailId(
                number(h, "racer-payload-rail")?
                    .try_into()
                    .map_err(|_| Error::InvalidRequest)?,
            ),
        })
    }
}

/// Bind the entire proof chain without hashing any payload bytes.
pub fn envelope_digest(auth: &ForwardedHead) -> Result<[u8; 32]> {
    let mut hash = Sha256::new();
    hash.update(b"racer-peer-v1/payload-envelope\0");
    hash.update((auth.hops.len() as u64).to_be_bytes());
    hash.update(signed_digest(&auth.original)?);
    for h in &auth.hops {
        hash.update(signed_digest(h)?);
    }
    Ok(hash.finalize().into())
}
