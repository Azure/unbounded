// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use anyhow::Context;
use openssl::{pkey::PKey, x509::X509};
use rustls::{
    ClientConfig, RootCertStore, ServerConfig,
    pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer, ServerName},
};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex, RwLock},
    time::Duration,
};
use tokio::{
    io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt},
    net::TcpStream,
};
use tokio_rustls::{TlsAcceptor, TlsConnector};

/// Only successful rustls handshakes inside this module construct peer evidence.
/// HTTP adapters may inspect it and must bind it to the durable boot with
/// `CaState::verify_member` on every request (including long-lived connections).
#[derive(Clone, Debug)]
pub struct VerifiedPeer {
    pub(super) fingerprint: String,
    pub(super) uri: String,
    pub(super) root: String,
    pub(super) not_before: i64,
    pub(super) not_after: i64,
    pub(super) is_server: bool,
}

impl VerifiedPeer {
    pub fn fingerprint(&self) -> &str {
        &self.fingerprint
    }
    pub fn uri(&self) -> &str {
        &self.uri
    }
    pub fn issuer(&self) -> &str {
        &self.root
    }

    pub fn node_pod(&self, universe: &str, node: &str, now: i64) -> Result<&str> {
        ensure!(
            hex_id(universe) && hex_id(node) && !self.is_server,
            "not a verified node client"
        );
        ensure!(
            now >= self.not_before && now < self.not_after,
            "expired TLS identity"
        );
        let prefix = format!("spiffe://racer/universe/{universe}/node/{node}/pod/");
        let uid = self
            .uri
            .strip_prefix(&prefix)
            .context("TLS route identity mismatch")?;
        ensure!(process_id(uid), "invalid Pod URI");
        Ok(uid)
    }
}

/// No public constructor, Clone, Deserialize, or header-to-proof conversion.
pub struct TlsProof {
    pub(super) fence: String,
    pub(super) peer: VerifiedPeer,
    pub(super) local_root: String,
    pub(super) bundle: String,
    pub(super) at: i64,
    pub(super) session: String,
    pub(super) ack: Acknowledgment,
}

pub struct TlsSnapshot {
    bundle: TrustBundle,
    root: String,
    expires: i64,
    server: Arc<ServerConfig>,
    mutual_server: Arc<ServerConfig>,
    client: Arc<ClientConfig>,
}

fn cert_times(cert: &X509) -> Result<(i64, i64)> {
    let der = cert.to_der()?;
    let (_, parsed) = x509_parser::parse_x509_certificate(&der)
        .map_err(|e| anyhow::anyhow!("invalid certificate: {e}"))?;
    Ok((
        parsed.validity().not_before.timestamp(),
        parsed.validity().not_after.timestamp(),
    ))
}

impl TlsSnapshot {
    pub fn new(bundle_json: &[u8], certificate_pem: &[u8], key_pem: &[u8]) -> Result<Self> {
        let bundle = TrustBundle::parse(bundle_json)?;
        let certs = super::certificates::parse_certificates(certificate_pem)?;
        let leaf = &certs[0];
        super::certificates::leaf_uri(leaf)?;
        let root = super::certificates::leaf_root(leaf, &bundle, true)?;
        let key = PKey::private_key_from_pem(key_pem)?;
        ensure!(
            key.public_eq(leaf.public_key()?.as_ref()),
            "TLS key does not match leaf"
        );
        let key = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.private_key_to_pkcs8()?));
        let chain: Vec<_> = certs
            .iter()
            .map(|c| c.to_der().map(CertificateDer::from))
            .collect::<std::result::Result<_, _>>()?;
        let mut roots = RootCertStore::empty();
        for cert in super::certificates::parse_certificates(bundle.certificates.as_bytes())? {
            roots.add(CertificateDer::from(cert.to_der()?))?;
        }
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let builder = ServerConfig::builder_with_provider(provider.clone())
            .with_protocol_versions(&[&rustls::version::TLS13])?;
        let mut server = builder
            .clone()
            .with_no_client_auth()
            .with_single_cert(chain.clone(), key.clone_key())?;
        let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
            Arc::new(roots.clone()),
            provider.clone(),
        )
        .build()?;
        let mut mutual = builder
            .with_client_cert_verifier(verifier)
            .with_single_cert(chain.clone(), key.clone_key())?;
        for config in [&mut server, &mut mutual] {
            config.send_tls13_tickets = 0;
            config.session_storage = Arc::new(rustls::server::NoServerSessionStorage {});
            config.alpn_protocols = vec![b"http/1.1".to_vec()];
            config.max_early_data_size = 0;
        }
        let mut client = ClientConfig::builder_with_provider(provider)
            .with_protocol_versions(&[&rustls::version::TLS13])?
            .with_root_certificates(roots)
            .with_client_auth_cert(chain, key)?;
        client.resumption = rustls::client::Resumption::disabled();
        client.enable_early_data = false;
        client.alpn_protocols = vec![b"http/1.1".to_vec()];
        Ok(Self {
            bundle,
            root,
            expires: cert_times(leaf)?.1,
            server: Arc::new(server),
            mutual_server: Arc::new(mutual),
            client: Arc::new(client),
        })
    }

    pub fn bundle(&self) -> &TrustBundle {
        &self.bundle
    }
    pub fn issuer(&self) -> &str {
        &self.root
    }
    pub fn ready(&self, now: i64) -> bool {
        now < self.expires
    }

    fn peer(&self, chain: Option<&[CertificateDer<'_>]>, is_server: bool) -> Result<VerifiedPeer> {
        let der = chain
            .and_then(|c| c.first())
            .context("missing authenticated peer certificate")?;
        let cert = X509::from_der(der.as_ref())?;
        let (not_before, not_after) = cert_times(&cert)?;
        Ok(VerifiedPeer {
            fingerprint: digest(der.as_ref()),
            uri: super::certificates::leaf_uri(&cert)?,
            root: super::certificates::leaf_root(&cert, &self.bundle, is_server)?,
            not_before,
            not_after,
            is_server,
        })
    }

    /// Production listener handshake. Fresh config is selected per accepted raw
    /// TCP connection; existing streams retain their original TLS snapshot.
    pub async fn accept(
        &self,
        raw: TcpStream,
        mutual: bool,
    ) -> Result<(
        tokio_rustls::server::TlsStream<TcpStream>,
        Option<VerifiedPeer>,
    )> {
        let config = if mutual {
            &self.mutual_server
        } else {
            &self.server
        };
        let stream = tokio::time::timeout(
            Duration::from_secs(10),
            TlsAcceptor::from(config.clone()).accept(raw),
        )
        .await??;
        let peer = if mutual {
            Some(self.peer(stream.get_ref().1.peer_certificates(), false)?)
        } else {
            None
        };
        Ok((stream, peer))
    }

    /// Performs a fresh full TLS handshake and exactly one bounded node proof
    /// request. Acknowledgment headers are read here, from this authenticated
    /// stream, and cannot be supplied by another caller/connection.
    pub async fn receive_node_proof(
        &self,
        raw: TcpStream,
        term: &Leadership,
    ) -> Result<NodeProofExchange> {
        term.check()?;
        tokio::time::timeout(Duration::from_secs(10), async {
            let (mut stream, peer) = self.accept(raw, true).await?;
            let conn = stream.get_ref().1;
            ensure!(
                conn.handshake_kind() == Some(rustls::HandshakeKind::Full),
                "proof requires full handshake"
            );
            let session = conn.export_keying_material(
                [0u8; 32],
                b"racer-ca-proof/v1",
                Some(self.bundle.digest().as_bytes()),
            )?;
            let at = unix_now();
            let peer = peer.context("missing mutual TLS identity")?;
            let message = read_http_head(&mut stream).await?;
            ensure!(
                message.start == "POST /v3/proof HTTP/1.1"
                    || message.start == "POST /v4/proof HTTP/1.1",
                "invalid proof request"
            );
            ensure!(
                message.content_length()? == 0,
                "proof request body forbidden"
            );
            let boot = message.header("x-racer-boot")?;
            ensure!(hex_id(boot), "invalid process boot nonce");
            ensure!(
                message.header("x-racer-certificate-issuer")? == peer.root,
                "claimed issuer differs from verified issuer"
            );
            let generation = message.header("x-racer-trust-generation")?.parse()?;
            let digest = message.header("x-racer-trust-digest")?.to_owned();
            ensure!(generation > 0 && hex_id(&digest), "invalid acknowledgment");
            let old: u64 = message.header("x-racer-old-connections")?.parse()?;
            let uid = peer
                .uri
                .rsplit('/')
                .next()
                .context("missing Pod identity")?;
            ensure!(process_id(uid), "invalid Pod identity");
            let key = format!("{uid}/{boot}");
            term.check()?;
            let proof = TlsProof {
                fence: term.token.clone(),
                peer,
                local_root: self.root.clone(),
                bundle: self.bundle.digest(),
                at,
                session: hex::encode(session),
                ack: Acknowledgment {
                    generation,
                    digest,
                    old_connections_drained: old == 0,
                },
            };
            Ok(NodeProofExchange { key, proof, stream })
        })
        .await?
    }

    /// Connect directly to the expected Pod IP using the shared CP DNS SAN.
    /// The actual enrolled fingerprint binds the Pod and boot when recorded.
    /// A ConfigMap acknowledgment alone never counts as proof.
    pub async fn probe_replica(
        &self,
        raw: TcpStream,
        namespace: &str,
        expected: &ReplicaAcknowledgment,
        term: &Leadership,
    ) -> Result<TlsProof> {
        term.check()?;
        ensure!(
            super::certificates::valid_namespace(namespace),
            "invalid namespace"
        );
        tokio::time::timeout(Duration::from_secs(10), async {
            let name = format!("racer-controlplane.{namespace}.svc");
            let mut stream = TlsConnector::from(self.client.clone()).connect(ServerName::try_from(name.clone())?, raw).await?;
            let conn = stream.get_ref().1;
            ensure!(conn.handshake_kind() == Some(rustls::HandshakeKind::Full), "proof requires full handshake");
            let peer = self.peer(conn.peer_certificates(), true)?;
            ensure!(peer.uri == "spiffe://racer/controlplane", "not a CP server identity");
            let session = conn.export_keying_material([0u8; 32], b"racer-ca-proof/v1", Some(self.bundle.digest().as_bytes()))?;
            let at = unix_now();
            stream.write_all(format!("GET /v3/replica-proof HTTP/1.1\r\nHost: {name}\r\nConnection: close\r\n\r\n").as_bytes()).await?;
            let message = read_http_head(&mut stream).await?;
            ensure!(message.start == "HTTP/1.1 200 OK", "replica proof HTTP status rejected");
            let length = message.content_length()?;
            ensure!(length > 0 && length <= 4096, "replica acknowledgment length invalid");
            let mut body = vec![0; length];
            stream.read_exact(&mut body).await?;
            let actual: ReplicaAcknowledgment = serde_json::from_slice(&body)?;
            ensure!(&actual == expected, "replica proof changed boot, CSR or installed state");
            term.check()?;
            Ok(TlsProof { fence: term.token.clone(), peer, local_root: self.root.clone(), bundle: self.bundle.digest(), at, session: hex::encode(session), ack: Acknowledgment { generation: actual.generation, digest: actual.digest, old_connections_drained: actual.old_connections_drained } })
        }).await?
    }
}

pub struct NodeProofExchange {
    pub key: String,
    pub proof: TlsProof,
    /// Send 204 only after record_proof succeeds; otherwise send a failure and close.
    pub stream: tokio_rustls::server::TlsStream<TcpStream>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ReplicaAcknowledgment {
    pub pod_uid: String,
    pub boot_id: String,
    pub csr_digest: String,
    pub generation: u64,
    pub digest: String,
    pub old_connections_drained: bool,
}

/// Response for a warm replica's installed process-local state. Call after
/// validating GET /v3/replica-proof on its proof listener, even on followers.
pub async fn write_replica_ack<W: AsyncWrite + Unpin>(
    stream: &mut W,
    ack: &ReplicaAcknowledgment,
) -> Result<()> {
    let body = serde_json::to_vec(ack)?;
    stream.write_all(format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n", body.len()).as_bytes()).await?;
    stream.write_all(&body).await?;
    stream.flush().await?;
    Ok(())
}

struct HttpHead {
    start: String,
    headers: BTreeMap<String, String>,
}
impl HttpHead {
    fn header(&self, name: &str) -> Result<&str> {
        self.headers
            .get(name)
            .map(String::as_str)
            .context("missing proof header")
    }
    fn content_length(&self) -> Result<usize> {
        ensure!(
            !self.headers.contains_key("transfer-encoding"),
            "chunked proof messages unsupported"
        );
        match self.headers.get("content-length") {
            Some(v) => Ok(v.parse()?),
            None => Ok(0),
        }
    }
}

async fn read_http_head<R: AsyncRead + Unpin>(stream: &mut R) -> Result<HttpHead> {
    let mut bytes = Vec::new();
    loop {
        ensure!(bytes.len() < 16384, "proof headers too large");
        bytes.push(stream.read_u8().await?);
        if bytes.ends_with(b"\r\n\r\n") {
            break;
        }
    }
    let text = std::str::from_utf8(&bytes)?;
    let mut lines = text[..text.len() - 4].split("\r\n");
    let start = lines.next().context("missing HTTP start")?.to_owned();
    let mut headers = BTreeMap::new();
    for line in lines {
        let (key, value) = line.split_once(':').context("invalid HTTP header")?;
        ensure!(
            !key.is_empty() && key.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-'),
            "invalid HTTP header name"
        );
        ensure!(
            headers
                .insert(key.to_ascii_lowercase(), value.trim().into())
                .is_none(),
            "duplicate proof header"
        );
    }
    Ok(HttpHead { start, headers })
}

/// Atomically installs complete production/proof pairs. Existing streams keep
/// the last valid snapshot. The adapter holds a ConnectionGuard for every
/// production stream, including enrollment, and closes old streams after update.
#[derive(Default)]
pub struct HotTls {
    current: RwLock<Option<Arc<InstalledTls>>>,
    connections: Arc<Mutex<BTreeMap<u64, usize>>>,
}

pub struct InstalledTls {
    pub production: TlsSnapshot,
    pub proof: TlsSnapshot,
    epoch: u64,
}

pub struct ConnectionGuard {
    epoch: u64,
    connections: Arc<Mutex<BTreeMap<u64, usize>>>,
}
impl Drop for ConnectionGuard {
    fn drop(&mut self) {
        let mut counts = self.connections.lock().unwrap();
        if let Some(count) = counts.get_mut(&self.epoch) {
            *count -= 1;
            if *count == 0 {
                counts.remove(&self.epoch);
            }
        }
    }
}

impl HotTls {
    /// Compare the epoch captured with the TLS pair. Reading an independent
    /// counter after acquiring an old snapshot can mistake it for a new one.
    pub fn is_current_epoch(&self, snapshot: &InstalledTls) -> bool {
        self.current
            .read()
            .unwrap()
            .as_ref()
            .is_some_and(|current| current.epoch == snapshot.epoch)
    }
    pub fn install(
        &self,
        production: TlsSnapshot,
        proof: TlsSnapshot,
    ) -> Result<Arc<InstalledTls>> {
        ensure!(
            production.bundle == proof.bundle,
            "TLS contexts disagree on trust"
        );
        let mut current = self.current.write().unwrap();
        let mut epoch = 1;
        if let Some(old) = current.as_ref() {
            ensure!(
                production.bundle.generation >= old.production.bundle.generation,
                "TLS trust rollback"
            );
            ensure!(
                production.bundle.generation != old.production.bundle.generation
                    || production.bundle == old.production.bundle,
                "TLS trust equivocation"
            );
            epoch = old.epoch;
            if production.bundle != old.production.bundle || production.root != old.production.root
            {
                epoch = epoch.checked_add(1).context("TLS epoch exhausted")?;
            }
        }
        let installed = Arc::new(InstalledTls {
            production,
            proof,
            epoch,
        });
        *current = Some(installed.clone());
        Ok(installed)
    }

    pub fn snapshot(&self) -> Result<Arc<InstalledTls>> {
        self.current
            .read()
            .unwrap()
            .clone()
            .context("TLS not installed")
    }

    /// Acquire snapshot and guard atomically with respect to installation, before
    /// beginning the handshake; otherwise an old accepted connection can escape
    /// the drain barrier. Guard lifetime must equal transport lifetime.
    pub fn production_connection(&self) -> Result<(Arc<InstalledTls>, ConnectionGuard)> {
        let current = self.current.read().unwrap();
        let snapshot = current.as_ref().context("TLS not installed")?.clone();
        *self
            .connections
            .lock()
            .unwrap()
            .entry(snapshot.epoch)
            .or_default() += 1;
        Ok((
            snapshot.clone(),
            ConnectionGuard {
                epoch: snapshot.epoch,
                connections: self.connections.clone(),
            },
        ))
    }

    pub fn drained(&self) -> bool {
        let current = self.current.read().unwrap();
        let Some(snapshot) = current.as_ref() else {
            return false;
        };
        self.connections
            .lock()
            .unwrap()
            .iter()
            .all(|(epoch, count)| *epoch == snapshot.epoch || *count == 0)
    }

    pub fn ready(
        &self,
        now: i64,
        production_listeners: bool,
        replica_proof_listener: bool,
    ) -> bool {
        production_listeners
            && replica_proof_listener
            && self
                .snapshot()
                .is_ok_and(|s| s.production.ready(now) && s.proof.ready(now))
    }
}
