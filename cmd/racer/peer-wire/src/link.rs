//! Signed `RdmaLink` setup request and response heads; link policy stays outside.
//!
//! The request carries the initiator's setup bytes. The response carries the
//! responder's setup bytes and a binding to the exact initiator setup. Both
//! heads have no body. Setup bytes are opaque here; the racer link-setup codec
//! decodes them.

use crate::*;

/// Request target of the link setup request.
pub const TARGET: &str = "/racer/v1/rdma/link";

/// Value of `racer-kind` on both request and response.
pub const KIND: &str = "rdma-link-v1";

/// Signed field holding the sender's setup bytes.
pub const SETUP_FIELD: &str = "racer-rdma-setup";

/// Signed response field holding SHA-256 of the initiator's setup bytes.
pub const BINDING_FIELD: &str = "racer-rdma-setup-binding";

/// Upper bound on decoded setup bytes.
pub const MAX_SETUP: usize = 128;

/// One `RdmaLink` message: a request when `binding` is `None`, else a response.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RdmaLink {
    /// Sender's opaque setup bytes.
    pub setup: Vec<u8>,

    /// Response only: digest of the initiator setup being answered.
    pub binding: Option<[u8; 32]>,
}

impl RdmaLink {
    /// Whether this is a response.
    pub fn response(&self) -> bool {
        self.binding.is_some()
    }

    /// Construct the canonical unsigned head.
    pub fn head(&self) -> Result<MessageHead> {
        if self.setup.is_empty() || self.setup.len() > MAX_SETUP {
            return Err(Error::InvalidRequest);
        }
        let mut h = MessageHead {
            start: if self.response() {
                StartLine::Response { status: 200 }
            } else {
                StartLine::Request {
                    method: "POST".into(),
                    target: TARGET.into(),
                }
            },
            headers: vec![],
        };
        if !self.response() {
            push(&mut h, "host", "");
        }
        push(&mut h, "content-length", 0);
        push(&mut h, "racer-kind", KIND);
        push_binary(&mut h, SETUP_FIELD, &self.setup);
        if let Some(binding) = &self.binding {
            push_binary(&mut h, BINDING_FIELD, binding);
        }
        Ok(h)
    }

    /// Sign this message for `to`.
    pub fn sign(&self, signatures: &Signatures, to: &NodeId) -> Result<SignedHead> {
        let mut h = self.head()?;
        push(&mut h, "racer-receiver", &to.0);
        signatures.sign(h)
    }

    /// Parse unverified fields; parsing alone grants no authority.
    pub fn parse(head: &MessageHead) -> Result<Self> {
        if field(head, "racer-kind")? != KIND {
            return Err(Error::Unauthorized);
        }
        let setup = decode_binary(field(head, SETUP_FIELD)?.as_bytes())?;
        let binding = match head.start {
            StartLine::Request { .. } => None,
            StartLine::Response { .. } => Some(
                decode_binary(field(head, BINDING_FIELD)?.as_bytes())?
                    .try_into()
                    .map_err(|_| Error::InvalidRequest)?,
            ),
        };
        let link = Self { setup, binding };
        link.head()?;
        Ok(link)
    }

    /// Check kind, direction, exact fields, signer (if known), and proof.
    /// A responder does not know the initiator before verification, so it passes
    /// `from = None` and reads the peer from the returned head.
    pub fn verify(
        signatures: &Signatures,
        from: Option<&NodeId>,
        signed: SignedHead,
        response: bool,
    ) -> Result<(VerifiedHead, Self)> {
        let link = Self::parse(&signed.head)?;
        if link.response() != response {
            return Err(Error::Unauthorized);
        }
        agrees(&signed.head, &link.head()?, false)?;
        if let Some(from) = from
            && node_field(&signed.head, "racer-signer")? != *from
        {
            return Err(Error::Unauthorized);
        }
        Ok((signatures.verify_proof(signed)?, link))
    }
}
