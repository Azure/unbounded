//! Bounded TLS 1.3 JSON HTTP on caller-owned readiness, deadlines, and leases.
pub mod dns;
mod transport;

use std::{
    net::SocketAddr,
    path::{Path, PathBuf},
    rc::Rc,
    time::{Instant, SystemTime},
};
pub use transport::{Connection, Response, Transport};
pub use uring_runtime::Operation;
use uring_runtime::reactor::Descriptor;

/// Transport failures contain no credentials, headers, or body bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    InvalidConfiguration,
    InvalidRequest,
    Unauthorized,
    Unavailable,
    Overloaded,
    Io,
    Internal,
}
pub type Result<T, E = Error> = std::result::Result<T, E>;

/// Narrowing must preserve cancellation and all caller policy, never extend time.
pub trait Scope: uring_runtime::Scope {
    fn deadline(&self) -> Instant;
    fn narrowed(&self, until: Instant) -> Self;
}

/// Readiness retains the descriptor and optional lease until deregistration,
/// including cancellation cleanup. Futures run on the caller's sole I/O owner.
pub trait Io: 'static {
    type Error: Copy + Send + PartialEq + From<Error> + From<uring_runtime::Error> + 'static;
    type Scope: Scope<Error = Self::Error>;
    type Lease: 'static;

    fn lease(&self) -> Result<Option<Rc<Self::Lease>>, Self::Error>;
    fn ready<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        read: bool,
        write: bool,
        lease: Option<Rc<Self::Lease>>,
        scope: &'a Self::Scope,
    ) -> Operation<'a, (), Self::Error>;
    fn resolve<'a>(
        &'a self,
        host: &'a str,
        port: u16,
        scope: &'a Self::Scope,
    ) -> Operation<'a, Vec<SocketAddr>, Self::Error>;
    fn read_file<'a>(
        &'a self,
        path: &'a Path,
        limit: usize,
        scope: &'a Self::Scope,
    ) -> Operation<'a, zeroize::Zeroizing<Vec<u8>>, Self::Error>;
}

pub struct Config {
    pub url: String,
    pub trust_bundle: PathBuf,
    pub max_trust_bundle: usize,
    pub max_error_body: usize,
}

/// Borrowed DER certificates and PKCS#8 key. Caller validates identity policy.
#[derive(Clone, Copy)]
pub struct Identity<'a> {
    pub certificate_chain: &'a [Vec<u8>],
    pub private_key: &'a [u8],
    pub expires: SystemTime,
}

#[derive(Clone, Copy)]
pub enum Method {
    Get,
    Post,
}

pub struct Request<'a> {
    pub method: Method,
    pub path: &'a str,
    pub bearer: Option<&'a str>,
    pub header: Option<(&'a str, &'a str)>,
    pub body: &'a [u8],
    pub limit: usize,
}
