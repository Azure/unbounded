//! Test fixtures generate ephemeral keys only inside the crate's target directory.
use std::{
    path::PathBuf,
    sync::atomic::{AtomicU64, Ordering},
};
pub(super) struct Directory(pub PathBuf);
impl Directory {
    pub fn new() -> Self {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target/control-tests")
            .join(format!(
                "{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
        std::fs::create_dir_all(&path).unwrap();
        Self(path)
    }
}
impl Drop for Directory {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}
pub(super) fn scope() -> crate::runtime::deadline::RequestScope {
    crate::runtime::deadline::RequestScope::new(
        crate::model::identity::RequestId([7; 16]),
        std::time::Instant::now() + std::time::Duration::from_secs(10),
    )
    .unwrap()
}
pub(super) fn ca() -> (rcgen::Certificate, rcgen::KeyPair) {
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    params.key_usages = vec![
        rcgen::KeyUsagePurpose::KeyCertSign,
        rcgen::KeyUsagePurpose::CrlSign,
    ];
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    (params.self_signed(&key).unwrap(), key)
}
pub(super) fn reactor() -> Option<std::rc::Rc<crate::runtime::reactor::Reactor>> {
    match io_uring::IoUring::new(2) {
        Ok(ring) => drop(ring),
        Err(e)
            if matches!(
                e.raw_os_error(),
                Some(libc::ENOSYS | libc::EPERM | libc::EACCES)
            ) =>
        {
            eprintln!("control io_uring unavailable: {e}");
            return None;
        }
        Err(e) => panic!("io_uring setup: {e}"),
    }
    let n = std::num::NonZeroUsize::new(1024 * 1024).unwrap();
    let limits = crate::model::limits::Limits {
        plaintext_bytes: n,
        ciphertext_bytes: n,
        dirty_bytes: n,
        registered_bytes: n,
        request_context_bytes: n,
        flights: n,
        waiters_per_flight: n,
        queue_entries: std::num::NonZeroUsize::new(32).unwrap(),
        connections_per_neighbor: n,
        client_connections: n,
        pipes: n,
        range_window_pages: n,
        replay_entries: n,
        header_bytes: n,
        route_search_work: n,
        cached_rankings: n,
        cached_paths: n,
        retained_snapshots: n,
        metadata_entries: n,
        relay_transfers: n,
    };
    let r = std::rc::Rc::new(crate::runtime::reactor::Reactor::new(std::rc::Rc::new(
        crate::runtime::admission::Admission::new(limits),
    )));
    r.init().unwrap();
    Some(r)
}
pub(super) fn drive<T>(
    r: &crate::runtime::reactor::Reactor,
    mut future: crate::error::Operation<'_, T>,
) -> crate::error::Result<T> {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(15);
    loop {
        if let std::task::Poll::Ready(result) = future.as_mut().poll(
            &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
        ) {
            return result;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "control reactor stalled"
        );
        r.poll_budgeted(8)?;
        r.wait(std::time::Duration::from_millis(1))?;
    }
}
pub(super) fn issue(
    request: &super::wire::EnrollmentRequest,
    ca: &rcgen::Certificate,
    key: &rcgen::KeyPair,
    node: &str,
) -> super::wire::EnrollmentResponse {
    let der = rustls::pki_types::CertificateSigningRequestDer::from(request.csr_der.clone());
    let mut csr = rcgen::CertificateSigningRequestParams::from_der(&der).unwrap();
    csr.params.not_before =
        (std::time::SystemTime::now() - std::time::Duration::from_secs(1)).into();
    csr.params.not_after =
        (std::time::SystemTime::now() + std::time::Duration::from_secs(86400 - 1)).into();
    csr.params.subject_alt_names = vec![rcgen::SanType::URI(
        format!("spiffe://{}/node/{node}", request.cluster.0)
            .try_into()
            .unwrap(),
    )];
    csr.params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
    csr.params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
    let cert = csr.signed_by(ca, key).unwrap();
    super::wire::EnrollmentResponse {
        schema_version: 1,
        cluster: request.cluster.clone(),
        node: crate::model::identity::NodeId(node.into()),
        enrollment: request.enrollment.clone(),
        certificate_chain: vec![cert.der().to_vec()],
    }
}
