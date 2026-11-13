//! Socket-owned ordered sessions and the mutually authenticated challenge exchange.

use crate::*;
use http1::connection::{ConnectionLease, HttpIo};
use racer_io::Scope;

const SESSION_TARGET: &str = "/racer/peer/v1/session";
const SESSION_DOMAIN: &[u8] = b"racer-peer-v1/connection\0";

/// Application hooks needed by the generic handshake driver.
pub trait Context: http1::connection::Context {
    /// Borrow the socket-owned session slot.
    fn session(state: &Self::State) -> Option<&Session>;

    /// Borrow the socket-owned session slot for one-time installation.
    fn session_mut(state: &mut Self::State) -> &mut Option<Session>;

    /// Preserve parent cancellation and shorten its deadline to at most five seconds.
    fn handshake_scope(parent: &Self::Scope) -> std::result::Result<Self::Scope, Self::Error>;
}

/// Never cloned, reset, or detached from its socket; idle pooling moves this value.
///
/// ```compile_fail
/// fn duplicate(session: &peer_wire::Session) -> peer_wire::Session {
///     session.clone()
/// }
/// ```
pub struct Session {
    state: SessionState,
}

/// Session state is only exposed by the optional adapter test helpers.
pub struct SessionState {
    /// Fixture signing authority.
    pub signatures: Rc<Signatures>,

    /// Fixture immediate peer.
    pub peer: NodeId,

    /// Fixture transcript identifier.
    pub id: [u8; 32],

    /// Fixture sending direction.
    pub direction: u8,

    /// Last emitted fixture sequence.
    pub tx: u64,

    /// Last admitted fixture sequence.
    pub rx: u64,

    /// Fixture monotonic expiry.
    pub expires: std::time::Instant,

    /// Chains each side has admitted from the other on this session.
    chains: crate::chains::SessionChains,
}

#[cfg(feature = "test-util")]
impl std::ops::Deref for Session {
    type Target = SessionState;

    fn deref(&self) -> &Self::Target {
        &self.state
    }
}

#[cfg(feature = "test-util")]
impl std::ops::DerefMut for Session {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.state
    }
}

impl Session {
    fn new(
        signatures: Rc<Signatures>,
        peer: NodeId,
        id: [u8; 32],
        direction: u8,
        peer_chain: Arc<Chain>,
        local_chain: [u8; 32],
    ) -> Self {
        crate::chains::remember(&peer_chain);
        Self {
            state: SessionState {
                peer,
                id,
                direction,
                tx: 0,
                rx: 0,
                expires: signatures.ctx().clock.now() + Duration::from_secs(3600),
                signatures,
                chains: crate::chains::SessionChains::new(peer_chain, local_chain),
            },
        }
    }

    /// Return the authenticated immediate peer.
    pub fn peer(&self) -> &NodeId {
        &self.state.peer
    }

    /// Fail closed on expiry, or when a session's chain sets could overflow on
    /// the next message, so pooled reuse reconnects instead.
    fn check(&self) -> Result<()> {
        self.check_live()?;
        if !self.state.chains.has_room() {
            return Err(Error::DeadlineExceeded);
        }
        Ok(())
    }

    /// Fail closed on expiry only.
    fn check_live(&self) -> Result<()> {
        if self.state.signatures.ctx().clock.now() >= self.state.expires {
            return Err(Error::DeadlineExceeded);
        }
        Ok(())
    }

    /// Sign the next outer head, advancing only after successful construction.
    ///
    /// Chains the peer has not yet admitted on this session, for the outer signer
    /// and every proof signer, ride in one signed `racer-chains` field. They count
    /// as sent once signing succeeds: like the sequence number, a send that fails
    /// after this point leaves the connection unusable, never reused.
    pub fn sign(&mut self, mut head: MessageHead) -> Result<MessageHead> {
        // Only our own send set gates signing: a peer that filled our received
        // set must still get the response to the request that filled it.
        self.check_live()?;
        if !self.state.chains.can_send() {
            return Err(Error::DeadlineExceeded);
        }
        let mut signers = Vec::new();
        if let Some(index) = head
            .headers
            .iter()
            .position(|h| h.name.eq_ignore_ascii_case(crate::PROOF_SIGNERS))
        {
            signers = crate::proof_signers(&head.headers.remove(index).value)?;
        }
        if head.headers.iter().any(|h| is_auth_field(&h.name)) {
            return Err(Error::Unauthorized);
        }
        let next = self.state.tx.checked_add(1).ok_or(Error::Replay)?;
        let identity = self.state.signatures.keys.signing_identity()?;
        let local = self.state.signatures.local_chain(&identity)?;
        let mut attach: Vec<Arc<Chain>> = Vec::new();
        for fingerprint in std::iter::once(*local.fingerprint()).chain(signers) {
            if self.state.chains.sent(&fingerprint)
                || attach.iter().any(|c| *c.fingerprint() == fingerprint)
            {
                continue;
            }
            attach.push(if fingerprint == *local.fingerprint() {
                local.clone()
            } else {
                crate::chains::recall(&fingerprint).ok_or(Error::Unauthorized)?
            });
        }
        push(&mut head, "racer-receiver", &self.state.peer.0);
        push_binary(&mut head, "racer-session", &self.state.id);
        push(&mut head, "racer-direction", self.state.direction);
        push(&mut head, "racer-sequence", next);
        if !attach.is_empty() {
            push(
                &mut head,
                "racer-chains",
                crate::chains::encode_attachment(&attach)?,
            );
        }
        let signed = self.state.signatures.sign_fields(head)?;
        self.state.tx = next;
        self.state
            .chains
            .mark_sent(attach.iter().map(|c| *c.fingerprint()));
        Ok(signed.head)
    }

    /// Authenticate and admit exactly the next frame before exposing its fields.
    ///
    /// Attached chains are parsed first, so a rotated outer signer can name its
    /// new chain in the same frame, but they enter the session only after the
    /// frame authenticates.
    pub fn admit(&mut self, head: MessageHead) -> Result<MessageHead> {
        // SessionChains::admit enforces the received cap exactly; our send set
        // never limits what we may receive.
        self.check_live()?;
        let attached = match head.unique("racer-chains")? {
            Some(value) => crate::chains::decode_attachment(value)?,
            None => Vec::new(),
        };
        let mut signed = signed(head)?;
        let outer = fingerprint(&signed.head)?;
        signed.chain = Some(match self.state.chains.received(&outer) {
            Some(chain) => chain.clone(),
            None => attached
                .iter()
                .find(|c| *c.fingerprint() == outer)
                .cloned()
                .ok_or(Error::InvalidRequest)?,
        });
        let verified = self.state.signatures.verify_proof(signed)?;
        let mut head = verified.signed.head;
        let next = self.state.rx.checked_add(1).ok_or(Error::Replay)?;
        if verified.peer.node() != &self.state.peer
            || session_array::<32>(&head, "racer-session")? != self.state.id
            || number(&head, "racer-direction")? != u64::from(1 - self.state.direction)
            || number(&head, "racer-sequence")? != next
        {
            return Err(Error::Replay);
        }
        let mut last = head.unique("racer-original")?;
        for name in HOP_FIELDS {
            if let Some(hop) = head.unique(name)? {
                last = Some(hop);
            }
        }
        if let Some(proof) = last {
            let proof = decode_signed(proof)?;
            if node_field(&proof.head, "racer-signer")? != self.state.peer
                || receiver(&proof.head)? != *self.state.signatures.node()
            {
                return Err(Error::Unauthorized);
            }
        }
        for chain in &attached {
            crate::chains::remember(chain);
        }
        self.state.chains.admit(attached)?;
        head.headers.retain(|h| !is_auth_field(&h.name));
        self.state.rx = next;
        Ok(head)
    }
}

impl Chains for Session {
    fn chain(&self, fingerprint: &[u8; 32]) -> Option<Arc<Chain>> {
        self.state.chains.received(fingerprint).cloned()
    }
}

/// Install once on a live exclusive lease, marking the handshake as unfinished I/O.
pub fn install_session<C: Context>(
    connection: &mut ConnectionLease<C>,
    session: Session,
) -> std::result::Result<(), C::Error>
where
    C::Error: From<Error>,
{
    if C::session(connection.state()).is_some() || connection.closing() {
        return Err(Error::Unauthorized.into());
    }
    *C::session_mut(connection.state_mut()) = Some(session);
    connection.begin_io();
    Ok(())
}

/// Authenticate a new outbound connection or validate its existing pooled session.
pub async fn connect<C: Context>(
    io: &HttpIo<C>,
    mut connection: ConnectionLease<C>,
    signatures: Rc<Signatures>,
    peer: &NodeId,
    parent: &C::Scope,
) -> std::result::Result<ConnectionLease<C>, C::Error>
where
    C::Error: From<Error>,
{
    parent.check()?;
    if let Some(session) = C::session(connection.state()) {
        if let Err(error) = session.check() {
            connection.poison();
            return Err(error.into());
        }
        if session.peer() != peer {
            connection.poison();
            return Err(Error::Unauthorized.into());
        }
        return Ok(connection);
    }
    let scope = C::handshake_scope(parent)?;
    let io = &io.capped(65536);
    let a = random(&signatures.ctx().rng)?;
    let response = io
        .exchange_head(
            connection,
            session_head(&signatures, peer, "hello", &a, &[0; 32], false)?,
            &scope,
        )
        .await?;
    // A listener with no peer ingress slot answers the overload head instead of a challenge
    // and closes the connection: fail `Overloaded` so the sender asks elsewhere.
    if crate::is_overloaded(&response.value) {
        let mut connection = response.connection;
        connection.poison();
        return Err(Error::Runtime(racer_io::Error::Overloaded).into());
    }
    let (_, echoed, b, _) =
        verify_session(&signatures, &response.value, Some(peer), "challenge", true)?;
    drop(response.value);
    if echoed != a || b == [0; 32] {
        return Err(Error::Unauthorized.into());
    }
    connection = response.connection;
    connection.next_round()?;
    let finish = session_head(&signatures, peer, "finish", &a, &b, false)?;
    let local_chain = fingerprint(&finish)?;
    let response = io.exchange_head(connection, finish, &scope).await?;
    let (_, echoed, remote, peer_chain) =
        verify_session(&signatures, &response.value, Some(peer), "ready", true)?;
    drop(response.value);
    if echoed != a || remote != b {
        return Err(Error::Unauthorized.into());
    }
    scope.check()?;
    connection = response.connection;
    connection.next_round()?;
    install_session(
        &mut connection,
        Session::new(
            signatures.clone(),
            peer.clone(),
            transcript(signatures.node(), peer, &a, &b),
            0,
            peer_chain,
            local_chain,
        ),
    )?;
    Ok(connection)
}

/// Authenticate an inbound connection before installing its replay state.
pub async fn accept<C: Context>(
    io: &HttpIo<C>,
    mut connection: ConnectionLease<C>,
    signatures: Rc<Signatures>,
    parent: &C::Scope,
) -> std::result::Result<ConnectionLease<C>, C::Error>
where
    C::Error: From<Error>,
{
    parent.check()?;
    if let Some(session) = C::session(connection.state()) {
        if let Err(error) = session.check() {
            connection.poison();
            return Err(error.into());
        }
        return Ok(connection);
    }
    let scope = C::handshake_scope(parent)?;
    let io = &io.capped(65536);
    let incoming = io.receive_head(connection, &scope).await?;
    scope.check()?;
    let (peer, a, zero, _) = verify_session(&signatures, &incoming.value, None, "hello", false)?;
    drop(incoming.value);
    if a == [0; 32] || zero != [0; 32] {
        return Err(Error::Unauthorized.into());
    }
    scope.check()?;
    let b = random(&signatures.ctx().rng)?;
    connection = io
        .send_head(
            incoming.connection,
            session_head(&signatures, &peer, "challenge", &a, &b, true)?,
            &scope,
        )
        .await?
        .connection;
    connection.next_round()?;
    let incoming = io.receive_head(connection, &scope).await?;
    scope.check()?;
    let (_, echoed, remote, peer_chain) =
        verify_session(&signatures, &incoming.value, Some(&peer), "finish", false)?;
    drop(incoming.value);
    if echoed != a || remote != b {
        return Err(Error::Unauthorized.into());
    }
    scope.check()?;
    let ready = session_head(&signatures, &peer, "ready", &a, &b, true)?;
    let local_chain = fingerprint(&ready)?;
    connection = io
        .send_head(incoming.connection, ready, &scope)
        .await?
        .connection;
    scope.check()?;
    connection.next_round()?;
    let id = transcript(&peer, signatures.node(), &a, &b);
    install_session(
        &mut connection,
        Session::new(signatures, peer, id, 1, peer_chain, local_chain),
    )?;
    Ok(connection)
}

fn session_array<const N: usize>(head: &MessageHead, name: &str) -> Result<[u8; N]> {
    decode_binary(field(head, name)?.as_bytes())?
        .try_into()
        .map_err(|_| Error::Unauthorized)
}

fn signed(head: MessageHead) -> Result<SignedHead> {
    Ok(SignedHead {
        signature: signature(&head)?,
        head,
        chain: None,
    })
}

fn signature(head: &MessageHead) -> Result<Vec<u8>> {
    let value = field(head, "signature")?;
    let value = value
        .strip_prefix("racer=:")
        .and_then(|v| v.strip_suffix(':'))
        .ok_or(Error::Unauthorized)?;
    decode_binary(value.as_bytes())
}

fn random(rng: &racer_io::Rng) -> Result<[u8; 32]> {
    let mut bytes = [0; 32];
    rng.fill(&mut bytes).map_err(|_| Error::Unavailable)?;
    Ok(bytes)
}

fn transcript(client: &NodeId, server: &NodeId, a: &[u8; 32], b: &[u8; 32]) -> [u8; 32] {
    let mut h = Sha256::new();
    h.update(SESSION_DOMAIN);
    h.update(client.0.as_bytes());
    h.update(server.0.as_bytes());
    h.update(a);
    h.update(b);
    h.finalize().into()
}

fn session_head(
    signatures: &Signatures,
    peer: &NodeId,
    phase: &str,
    a: &[u8; 32],
    b: &[u8; 32],
    response: bool,
) -> Result<MessageHead> {
    let mut h = MessageHead {
        start: if response {
            StartLine::Response { status: 200 }
        } else {
            StartLine::Request {
                method: "POST".into(),
                target: SESSION_TARGET.into(),
            }
        },
        headers: vec![],
    };
    if !response {
        push(&mut h, "host", "");
    }
    push(&mut h, "content-length", 0);
    push(&mut h, "racer-receiver", &peer.0);
    push(&mut h, "racer-handshake", phase);
    push_binary(&mut h, "racer-client-challenge", a);
    push_binary(&mut h, "racer-server-challenge", b);
    Ok(signatures.sign_handshake(h)?.head)
}

/// Verified handshake peer, client and server challenges, and the peer's chain.
type Handshake = (NodeId, [u8; 32], [u8; 32], Arc<Chain>);

fn verify_session(
    signatures: &Signatures,
    h: &MessageHead,
    peer: Option<&NodeId>,
    phase: &str,
    response: bool,
) -> Result<Handshake> {
    let chain = Chain::decode(&decode_binary(field(h, "racer-certificates")?.as_bytes())?)?;
    let verified = signatures.verify_signed_age(h, &signature(h)?, Some(&chain), true)?;
    if receiver(h)? != *signatures.node() {
        return Err(Error::Unauthorized);
    }
    let start = if response {
        matches!(h.start, StartLine::Response { status: 200 })
    } else {
        matches!(&h.start, StartLine::Request { method, target } if method == "POST" && target == SESSION_TARGET)
    };
    if !start
        || h.content_length()? != Some(0)
        || field(h, "racer-handshake")? != phase
        || peer.is_some_and(|p| p != verified.node())
    {
        return Err(Error::Unauthorized);
    }
    for field in &h.headers {
        let name = field.name.to_ascii_lowercase();
        if !is_auth_field(&name)
            && !matches!(
                name.as_str(),
                "host"
                    | "content-length"
                    | "racer-handshake"
                    | "racer-client-challenge"
                    | "racer-server-challenge"
            )
        {
            return Err(Error::Unauthorized);
        }
    }
    Ok((
        verified.node().clone(),
        session_array(h, "racer-client-challenge")?,
        session_array(h, "racer-server-challenge")?,
        chain,
    ))
}

#[cfg(feature = "test-util")]
pub mod test_util {
    use super::*;

    /// Create deterministic socket state for adapter fault-injection tests,
    /// seeded as if the handshake had exchanged both identities' chains.
    pub fn session(
        signatures: Rc<Signatures>,
        peer: &Signatures,
        id: [u8; 32],
        direction: u8,
    ) -> Session {
        let local = local_chain(&signatures);
        Session::new(
            signatures,
            peer.node().clone(),
            id,
            direction,
            local_chain(peer),
            *local.fingerprint(),
        )
    }

    /// The signer's current chain, as its handshake would carry it.
    pub fn local_chain(signatures: &Signatures) -> Arc<Chain> {
        signatures
            .local_chain(&signatures.keys.signing_identity().unwrap())
            .unwrap()
    }

    /// Chains this session has sent and received, including handshake seeds.
    pub fn chain_counts(session: &Session) -> (usize, usize) {
        session.state.chains.counts()
    }

    /// Generate one challenge from operating-system entropy.
    pub fn random() -> Result<[u8; 32]> {
        super::random(&racer_io::Rng::os())
    }

    /// Reconstruct the challenge transcript for regression assertions.
    pub fn transcript(client: &NodeId, server: &NodeId, a: &[u8; 32], b: &[u8; 32]) -> [u8; 32] {
        super::transcript(client, server, a, b)
    }

    /// Construct one signed handshake fixture.
    pub fn session_head(
        signatures: &Signatures,
        peer: &NodeId,
        phase: &str,
        a: &[u8; 32],
        b: &[u8; 32],
        response: bool,
    ) -> Result<MessageHead> {
        super::session_head(signatures, peer, phase, a, b, response)
    }

    /// Verify one signed handshake fixture.
    pub fn verify_session(
        signatures: &Signatures,
        head: MessageHead,
        peer: Option<&NodeId>,
        phase: &str,
        response: bool,
    ) -> Result<(NodeId, [u8; 32], [u8; 32])> {
        super::verify_session(signatures, &head, peer, phase, response)
            .map(|(n, a, b, _)| (n, a, b))
    }
}

#[cfg(test)]
mod handshake_tests;

#[cfg(test)]
mod tests {
    use super::*;

    fn frame() -> MessageHead {
        MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: REQUEST_TARGET.into(),
            },
            headers: vec![
                Header::new("host", b""),
                Header::new("content-length", b"0"),
            ],
        }
    }

    fn copy(head: &MessageHead) -> MessageHead {
        let codec = Codec::new(MAX_ENVELOPE_HEAD);
        codec
            .decode_head(&codec.encode_head(head).unwrap())
            .unwrap()
            .unwrap()
            .0
    }

    #[test]
    fn handshake_verification_borrows_original_head_without_consuming_it() {
        struct BorrowedMac {
            expected: std::cell::Cell<*const MessageHead>,
            calls: std::cell::Cell<usize>,
        }

        impl RequestMac for BorrowedMac {
            fn sign(&self, _: &Keyring, _: &mut MessageHead) -> Result<()> {
                Ok(())
            }

            fn verify(&self, _: &Keyring, head: &MessageHead) -> Result<()> {
                assert!(std::ptr::eq(head, self.expected.get()));
                self.calls.set(self.calls.get() + 1);
                Ok(())
            }
        }

        let n = crate::tests::network(3, &racer_io::WorkerCtx::host());
        let policy = Rc::new(BorrowedMac {
            expected: std::cell::Cell::new(std::ptr::null()),
            calls: std::cell::Cell::new(0),
        });
        let verifier = Signatures::new(
            n[1].keys.clone(),
            n[1].certificates.clone(),
            policy.clone(),
            racer_io::WorkerCtx::host(),
        );
        for response in [false, true] {
            let phase = if response { "challenge" } else { "hello" };
            let mut head =
                session_head(&n[0], n[1].node(), phase, &[1; 32], &[2; 32], response).unwrap();
            policy.expected.set(&head);
            let before = Codec::new(MAX_HEAD).encode_head(&head).unwrap();
            let (peer, a, b, _) =
                verify_session(&verifier, &head, Some(n[0].node()), phase, response).unwrap();
            assert_eq!(&peer, n[0].node());
            assert_eq!(a, [1; 32]);
            assert_eq!(b, [2; 32]);
            assert_eq!(Codec::new(MAX_HEAD).encode_head(&head).unwrap(), before);

            head.headers
                .iter_mut()
                .find(|h| h.name == "racer-client-challenge")
                .unwrap()
                .value = binary(&[3; 32]).into_bytes();
            assert!(verify_session(&verifier, &head, Some(n[0].node()), phase, response).is_err());
            assert_eq!(
                session_array::<32>(&head, "racer-client-challenge").unwrap(),
                [3; 32]
            );

            let wrong =
                session_head(&n[0], n[2].node(), phase, &[1; 32], &[2; 32], response).unwrap();
            policy.expected.set(&wrong);
            assert!(matches!(
                verify_session(&verifier, &wrong, Some(n[0].node()), phase, response),
                Err(Error::Unauthorized)
            ));
            assert_eq!(receiver(&wrong).unwrap(), *n[2].node());
        }
        assert_eq!(policy.calls.get(), 6);
    }

    #[test]
    fn handshake_accepts_header_case_but_rejects_unknown_fields() {
        let n = crate::tests::network(2, &racer_io::WorkerCtx::host());
        for response in [false, true] {
            let phase = if response { "challenge" } else { "hello" };
            let mut head =
                session_head(&n[0], n[1].node(), phase, &[1; 32], &[2; 32], response).unwrap();
            for field in &mut head.headers {
                field.name = field.name.to_ascii_uppercase();
            }
            let (peer, a, b, _) =
                verify_session(&n[1], &head, Some(n[0].node()), phase, response).unwrap();
            assert_eq!(&peer, n[0].node());
            assert_eq!(a, [1; 32]);
            assert_eq!(b, [2; 32]);

            head.headers.retain(|h| {
                !is_auth_field(&h.name) || h.name.eq_ignore_ascii_case("racer-receiver")
            });
            push(&mut head, "x-unknown", "value");
            let signed = n[0].sign(head).unwrap();
            assert!(matches!(
                verify_session(&n[1], &signed.head, Some(n[0].node()), phase, response),
                Err(Error::Unauthorized)
            ));
        }
    }

    #[test]
    fn duplicate_wrong_session_direction_identity_and_forged_high_sequence() {
        let n = crate::tests::network(2, &racer_io::WorkerCtx::host());
        let id = test_util::random().unwrap();
        let mut a = test_util::session(n[0].clone(), &n[1], id, 0);
        let mut b = test_util::session(n[1].clone(), &n[0], id, 1);
        let valid = a.sign(frame()).unwrap();
        for name in [
            "racer-sequence",
            "racer-session",
            "racer-direction",
            "racer-signer",
        ] {
            let mut bad = copy(&valid);
            bad.headers
                .iter_mut()
                .find(|h| h.name == name)
                .unwrap()
                .value = b"18446744073709551615".to_vec();
            assert!(b.admit(bad).is_err());
            assert_eq!(b.state.rx, 0, "forged {name} advanced sequence");
        }
        for mutation in 0..4 {
            let mut bad = test_util::session(n[0].clone(), &n[1], id, 0);
            match mutation {
                0 => bad.state.id[0] ^= 1,
                1 => bad.state.direction = 1,
                2 => bad.state.signatures = n[1].clone(),
                _ => bad.state.tx = 9000,
            }
            assert!(b.admit(bad.sign(frame()).unwrap()).is_err());
            assert_eq!(b.state.rx, 0);
        }
        b.admit(copy(&valid)).unwrap();
        assert!(matches!(b.admit(valid), Err(Error::Replay)));
        assert_eq!(b.state.rx, 1);
        b.admit(a.sign(frame()).unwrap()).unwrap();
    }

    #[test]
    fn reconnect_restart_expiry_and_counter_overflow_fail_closed() {
        let n = crate::tests::network(2, &racer_io::WorkerCtx::host());
        let id = test_util::random().unwrap();
        let mut a = test_util::session(n[0].clone(), &n[1], id, 0);
        let mut b = test_util::session(n[1].clone(), &n[0], id, 1);
        let old = a.sign(frame()).unwrap();
        let mut reconnected =
            test_util::session(n[1].clone(), &n[0], test_util::random().unwrap(), 1);
        assert!(reconnected.admit(copy(&old)).is_err());
        assert_eq!(reconnected.state.rx, 0);
        b.state.expires = b.state.signatures.ctx().clock.now();
        assert!(matches!(b.admit(old), Err(Error::DeadlineExceeded)));
        a.state.tx = u64::MAX;
        assert!(matches!(a.sign(frame()), Err(Error::Replay)));
        reconnected.state.rx = u64::MAX;
        let mut sender = test_util::session(n[0].clone(), &n[1], reconnected.state.id, 0);
        assert!(matches!(
            reconnected.admit(sender.sign(frame()).unwrap()),
            Err(Error::Replay)
        ));
    }
}

#[cfg(test)]
mod chain_tests {
    use super::*;

    /// Envelope frame carrying `original` and optional hops.
    fn envelope(original: SignedHead, hops: Vec<SignedHead>) -> MessageHead {
        encode_envelope(
            &ForwardedHead {
                original: Arc::new(original),
                hops,
            },
            false,
            0,
        )
        .unwrap()
    }

    fn proof(signer: &Signatures, receiver: &NodeId) -> SignedHead {
        let mut head = MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: "/racer/peer/v1".into(),
            },
            headers: vec![],
        };
        push(&mut head, "host", "");
        push(&mut head, "content-length", 0);
        push(&mut head, "racer-kind", "test");
        push(&mut head, "racer-receiver", &receiver.0);
        signer.sign(head).unwrap()
    }

    /// Node 0 originates, node 1 relays over this session, node 2 receives.
    fn relayed(n: &[Rc<Signatures>]) -> MessageHead {
        envelope(proof(&n[0], n[1].node()), vec![proof(&n[1], n[2].node())])
    }

    fn pair(n: &[Rc<Signatures>]) -> (Session, Session) {
        let id = test_util::random().unwrap();
        (
            test_util::session(n[1].clone(), &n[2], id, 0),
            test_util::session(n[2].clone(), &n[1], id, 1),
        )
    }

    fn chains_field(head: &MessageHead) -> Option<usize> {
        head.unique("racer-chains").unwrap().map(<[u8]>::len)
    }

    #[test]
    fn relay_attaches_a_foreign_chain_once_and_receiver_resolves_it() {
        let n = crate::tests::network(3, &racer_io::WorkerCtx::host());
        let (mut relay, mut receiver) = pair(&n);
        let first = relay.sign(relayed(&n)).unwrap();
        assert!(first.unique(crate::PROOF_SIGNERS).unwrap().is_none());
        assert!(
            chains_field(&first).is_some(),
            "first use attaches the chain"
        );
        let first = receiver.admit(first).unwrap();
        let (decoded, _) = decode_envelope(first, false, &receiver).unwrap();
        n[2].verify_historical(&decoded.original).unwrap();
        n[2].verify_proof(decoded.hops.into_iter().next().unwrap())
            .unwrap();

        let second = relay.sign(relayed(&n)).unwrap();
        assert_eq!(
            chains_field(&second),
            None,
            "chain is sent once per session"
        );
        let second = receiver.admit(second).unwrap();
        decode_envelope(second, false, &receiver).unwrap();
        assert_eq!(test_util::chain_counts(&relay), (2, 1));
        assert_eq!(test_util::chain_counts(&receiver), (1, 2));

        // Another session never received node 0's chain. A sender that omits it
        // makes the receiver's lookup miss, which is a protocol error.
        let (mut other_relay, mut other) = pair(&n);
        let mut omitted = relayed(&n);
        omitted
            .headers
            .retain(|h| !h.name.eq_ignore_ascii_case(crate::PROOF_SIGNERS));
        let admitted = other.admit(other_relay.sign(omitted).unwrap()).unwrap();
        assert_eq!(
            decode_envelope(admitted, false, &other).err(),
            Some(Error::InvalidRequest)
        );
    }

    #[test]
    fn tampered_or_unknown_chain_attachments_fail_closed() {
        let n = crate::tests::network(3, &racer_io::WorkerCtx::host());
        let (mut relay, mut receiver) = pair(&n);
        let mut frame = relay.sign(relayed(&n)).unwrap();
        let field = frame
            .headers
            .iter_mut()
            .find(|h| h.name == "racer-chains")
            .unwrap();
        let mut bytes = STANDARD.decode(&field.value).unwrap();
        *bytes.last_mut().unwrap() ^= 1;
        field.value = binary(&bytes).into_bytes();
        assert!(receiver.admit(frame).is_err());
        assert_eq!(test_util::chain_counts(&receiver), (1, 1));
        assert_eq!(receiver.state.rx, 0);

        // A sender cannot attach a chain no live proof holds.
        let (mut sender, _) = pair(&n);
        let mut head = envelope(proof(&n[1], n[2].node()), vec![]);
        head.headers
            .iter_mut()
            .find(|h| h.name == crate::PROOF_SIGNERS)
            .unwrap()
            .value = binary(&[7; 32]).into_bytes();
        assert_eq!(sender.sign(head).err(), Some(Error::Unauthorized));
        assert_eq!(sender.state.tx, 0);
        assert_eq!(test_util::chain_counts(&sender), (1, 1));
    }

    /// Report steady-state head bytes and sign/admit/verify cost. Run with
    /// `cargo test --release -p peer-wire --features test-util head_cost -- --ignored --nocapture`.
    #[test]
    #[ignore = "measurement"]
    fn head_cost() {
        let n = crate::tests::network(3, &racer_io::WorkerCtx::host());
        let codec = Codec::new(MAX_ENVELOPE_HEAD);
        for (name, hops) in [("direct", false), ("relayed", true)] {
            let (mut sender, mut receiver) = pair(&n);
            let make = || {
                if hops {
                    relayed(&n)
                } else {
                    envelope(proof(&n[1], n[2].node()), vec![])
                }
            };
            let first = sender.sign(make()).unwrap();
            let first_bytes = codec.encode_head(&first).unwrap().len();
            receiver.admit(first).unwrap();
            let steady = sender.sign(make()).unwrap();
            let steady_bytes = codec.encode_head(&steady).unwrap().len();
            receiver.admit(steady).unwrap();
            let rounds = 2000;
            let heads: Vec<_> = (0..rounds).map(|_| make()).collect();
            let start = std::time::Instant::now();
            let signed: Vec<_> = heads.into_iter().map(|h| sender.sign(h).unwrap()).collect();
            let sign = start.elapsed() / rounds;
            let start = std::time::Instant::now();
            let mut proofs = Vec::new();
            for head in signed {
                let head = receiver.admit(head).unwrap();
                proofs.push(decode_envelope(head, false, &receiver).unwrap().0);
            }
            let admit = start.elapsed() / rounds;
            let start = std::time::Instant::now();
            for proof in &proofs {
                n[2].verify_historical(&proof.original).unwrap();
            }
            let verify = start.elapsed() / rounds;
            println!(
                "{name}: first={first_bytes}B steady={steady_bytes}B sign={sign:?} admit+decode={admit:?} verify-proof={verify:?}"
            );
        }
    }

    #[test]
    fn full_received_set_still_allows_signing_the_response() {
        let n = crate::tests::network(3, &racer_io::WorkerCtx::host());
        let (mut client, mut server) = pair(&n);
        let fake = |i: u32| {
            let mut bytes = 4u32.to_be_bytes().to_vec();
            bytes.extend_from_slice(&i.to_be_bytes());
            Chain::decode(&bytes).unwrap()
        };
        let mut i = 0;
        while server.state.chains.admit(vec![fake(i)]).is_ok() {
            i += 1;
        }
        assert!(!server.state.chains.can_receive());
        assert!(server.state.chains.can_send());
        let request = client
            .sign(envelope(proof(&n[1], n[2].node()), vec![]))
            .unwrap();
        server.admit(request).unwrap();
        let response = server
            .sign(envelope(proof(&n[2], n[1].node()), vec![]))
            .expect("a full received set must not block the response");
        client.admit(response).unwrap();
        assert!(server.check().is_err(), "pooled reuse still stops");
    }

    #[test]
    fn session_chain_cap_is_a_protocol_error_and_stops_reuse() {
        let peer = Chain::decode(&[0, 0, 0, 1, 0]).unwrap();
        let mut chains = crate::chains::SessionChains::new(peer, [0; 32]);
        let fake = |i: u32| {
            let mut bytes = 4u32.to_be_bytes().to_vec();
            bytes.extend_from_slice(&i.to_be_bytes());
            Chain::decode(&bytes).unwrap()
        };
        let mut i = 0;
        while chains.has_room() {
            chains.admit(vec![fake(i)]).unwrap();
            i += 1;
        }
        assert_eq!(
            i as usize + 1,
            MAX_SESSION_CHAINS - MAX_CHAINS_PER_MESSAGE + 1
        );
        assert_eq!(
            chains.admit(vec![fake(0)]),
            Err(Error::InvalidRequest),
            "a repeated chain is a protocol error"
        );
        while chains.admit(vec![fake(i)]).is_ok() {
            i += 1;
        }
        assert_eq!(i as usize + 1, MAX_SESSION_CHAINS);
        assert_eq!(
            chains.admit(vec![fake(u32::MAX)]),
            Err(Error::InvalidRequest)
        );
    }
}
