//! Preserve original signatures and bind each signed hop to consumed route state.
use super::signing::{Signatures, SignedHead, VerifiedHead};
use crate::{
    error::{Result, pending},
    topology::paths::RouteBudget,
};
use std::rc::Rc;
pub struct Forwarding {
    signatures: Rc<Signatures>,
}
pub struct ForwardedHead {
    pub original: SignedHead,
    pub hops: Vec<SignedHead>,
}
impl Forwarding {
    pub fn new(signatures: Rc<Signatures>) -> Self {
        Self { signatures }
    }
    pub fn verify(&self, _head: ForwardedHead) -> Result<VerifiedHead> {
        pending("forwarding.verify")
    }
    pub fn append(&self, _head: ForwardedHead, _budget: &RouteBudget) -> Result<ForwardedHead> {
        pending("forwarding.append")
    }
}
#[cfg(test)]
mod tests { /* Budget/deadline extension, original signature substitution, hop binding. */
}
