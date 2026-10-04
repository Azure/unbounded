use super::dataplane;
use dataplane::admission::AdmissionPolicy;
use dataplane::config::Limits;
use dataplane::error::Operation;
use dataplane::runtime::Reactor;

use std::num::NonZeroUsize;
use std::rc::Rc;
use std::time::Duration;
use std::time::Instant;

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

pub fn drive<T>(reactor: &Reactor, future: Operation<'_, T>) -> dataplane::error::Result<T> {
    drive_until(reactor, future, Instant::now() + Duration::from_secs(15))
}

/// Drive network and enrollment operations on the same production reactor.
pub fn drive_until<T>(
    reactor: &Reactor,
    future: impl std::future::Future<Output = dataplane::error::Result<T>>,
    deadline: Instant,
) -> dataplane::error::Result<T> {
    let mut future = std::pin::pin!(future);
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
