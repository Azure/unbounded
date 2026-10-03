use super::dataplane;
use dataplane::{
    error::{Error, Operation},
    model::Limits,
    runtime::{admission::AdmissionPolicy, reactor::Reactor},
};

/// Blocking readiness for loopback TLS fixtures only, never the serving graph.
pub struct ControlIo;
impl dataplane::control::transport::ControlIo for ControlIo {
    fn read_file<'a>(
        &'a self,
        path: &'a std::path::Path,
        limit: usize,
        scope: &'a dataplane::runtime::deadline::RequestScope,
    ) -> Operation<'a, zeroize::Zeroizing<Vec<u8>>> {
        Box::pin(async move {
            use std::{io::Read, os::unix::fs::OpenOptionsExt};
            scope.check()?;
            let file = std::fs::OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_NONBLOCK | libc::O_CLOEXEC)
                .open(path)
                .map_err(|_| Error::Io)?;
            if !file.metadata().map_err(|_| Error::Io)?.is_file() {
                return Err(Error::InvalidRequest);
            }
            let mut bytes = zeroize::Zeroizing::new(Vec::new());
            file.take(limit as u64 + 1)
                .read_to_end(&mut bytes)
                .map_err(|_| Error::Io)?;
            if bytes.len() > limit {
                return Err(Error::Overloaded);
            }
            Ok(bytes)
        })
    }
    fn resolve<'a>(
        &'a self,
        _: &'a str,
        port: u16,
        _: &'a dataplane::runtime::deadline::RequestScope,
    ) -> Operation<'a, Vec<std::net::SocketAddr>> {
        Box::pin(async move { Ok(vec![std::net::SocketAddr::from(([127, 0, 0, 1], port))]) })
    }
    fn ready<'a>(
        &'a self,
        fd: Rc<uring_runtime::reactor::Descriptor>,
        read: bool,
        write: bool,
        scope: &'a dataplane::runtime::deadline::RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            use std::os::fd::AsRawFd;
            loop {
                scope.check()?;
                let mut poll = libc::pollfd {
                    fd: fd.as_raw_fd(),
                    events: (if read { libc::POLLIN } else { 0 })
                        | (if write { libc::POLLOUT } else { 0 }),
                    revents: 0,
                };
                // SAFETY: poll references one initialized entry for the call.
                match unsafe { libc::poll(&mut poll, 1, 10) } {
                    n if n > 0 => return Ok(()),
                    n if n < 0 => return Err(Error::Io),
                    _ => (),
                }
            }
        })
    }
    fn sleep<'a>(
        &'a self,
        _: Instant,
        _: &'a dataplane::runtime::deadline::RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async { Ok(()) })
    }
}
use std::{
    num::NonZeroUsize,
    rc::Rc,
    time::{Duration, Instant},
};

pub fn reactor() -> Rc<Reactor> {
    let n = NonZeroUsize::new(1024 * 1024).unwrap();
    let reactor = Rc::new(Reactor::new(Rc::new(flow_control::Quotas::new(
        AdmissionPolicy::new(Limits {
            plaintext_bytes: n,
            ciphertext_bytes: n,
            dirty_bytes: n,
            registered_bytes: n,
            request_context_bytes: n,
            flights: n,
            waiters_per_flight: n,
            queue_entries: NonZeroUsize::new(32).unwrap(),
            connections_per_neighbor: n,
            client_connections: n,
            pipes: n,
            range_window_pages: n,
            header_bytes: n,
            cached_rankings: n,
            cached_paths: n,
            retained_snapshots: n,
            metadata_entries: n,
            relay_transfers: n,
        }),
    ))));
    reactor.init().unwrap();
    reactor
}

pub fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> dataplane::error::Result<T> {
    let deadline = Instant::now() + Duration::from_secs(15);
    loop {
        if let std::task::Poll::Ready(result) = future.as_mut().poll(
            &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
        ) {
            return result;
        }
        assert!(Instant::now() < deadline, "enrollment reactor stalled");
        if reactor.poll_budgeted(8)? == 0 {
            reactor.wait(Duration::from_millis(1))?;
        }
    }
}

pub fn read_head(stream: &mut impl std::io::Read) -> std::io::Result<String> {
    let mut head = Vec::new();
    while !head.ends_with(b"\r\n\r\n") {
        let mut byte = [0];
        stream.read_exact(&mut byte)?;
        head.push(byte[0]);
        assert!(head.len() <= 32768, "oversized fixture head");
    }
    Ok(String::from_utf8(head).unwrap())
}
pub fn fields(head: &str) -> std::collections::BTreeMap<String, String> {
    head.lines()
        .skip(1)
        .filter_map(|line| line.split_once(':'))
        .map(|(key, value)| (key.to_ascii_lowercase(), value.trim().to_owned()))
        .collect()
}

pub fn ca() -> (rcgen::Certificate, rcgen::KeyPair) {
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    params.key_usages = vec![
        rcgen::KeyUsagePurpose::KeyCertSign,
        rcgen::KeyUsagePurpose::CrlSign,
    ];
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    (params.self_signed(&key).unwrap(), key)
}

pub fn signing_identity(
    pending: racer_identity::PendingIdentity,
    ca: &rcgen::Certificate,
    ca_key: &rcgen::KeyPair,
    cluster: dataplane::model::ClusterId,
    node: dataplane::model::NodeId,
) -> std::sync::Arc<racer_identity::SigningIdentity> {
    let secret = pending.export_pkcs8_for_persistence().unwrap();
    let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
        &rustls::pki_types::PrivatePkcs8KeyDer::from(secret.as_slice()),
        &rcgen::PKCS_ED25519,
    )
    .unwrap();
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.subject_alt_names = vec![rcgen::SanType::URI(
        format!("spiffe://{}/node/{}", cluster.0, node.0)
            .try_into()
            .unwrap(),
    )];
    params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
    params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
    let cert = params.signed_by(&key, ca, ca_key).unwrap();
    std::sync::Arc::new(
        pending
            .accept(
                cluster,
                node,
                vec![cert.der().to_vec()],
                &[ca.der().to_vec()],
            )
            .unwrap(),
    )
}
