//! Connection authentication configuration. Native support is attempted on the
//! selected rail and authenticated by transfer-scoped controls on that connection.
use crate::{rdma::session::Sessions, security::signing::Signatures};
use std::rc::Rc;

pub struct Handshake {
    pub(crate) signatures: Rc<Signatures>,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Capabilities {
    pub rdma: bool,
    pub scoped_grants: bool,
}
impl Handshake {
    pub fn new(signatures: Rc<Signatures>, _rdma: Option<Rc<Sessions>>) -> Self {
        Self { signatures }
    }
}
