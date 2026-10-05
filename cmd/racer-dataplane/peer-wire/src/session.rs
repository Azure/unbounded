//! Socket-owned ordered sessions and the mutually authenticated challenge exchange.

use crate::*;
use http1::connection::{ConnectionLease, HttpIo};
use uring_runtime::Scope;

const SESSION_TARGET: &str = "/racer/peer/v2/session";
const SESSION_DOMAIN: &[u8] = b"racer-peer-v2/connection\0";

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
    fn new(signatures: Rc<Signatures>, peer: NodeId, id: [u8; 32], direction: u8) -> Self {
        Self {
            state: SessionState {
                signatures,
                peer,
                id,
                direction,
                tx: 0,
                rx: 0,
                expires: uring_runtime::environment::now() + Duration::from_secs(3600),
            },
        }
    }

    /// Return the authenticated immediate peer.
    pub fn peer(&self) -> &NodeId {
        &self.state.peer
    }

    fn check(&self) -> Result<()> {
        if uring_runtime::environment::now() >= self.state.expires {
            return Err(Error::DeadlineExceeded);
        }
        Ok(())
    }

    /// Sign the next outer head, advancing only after successful construction.
    pub fn sign(&mut self, mut head: MessageHead) -> Result<MessageHead> {
        self.check()?;
        if head
            .headers
            .iter()
            .any(|h| is_auth_field(&h.name.to_ascii_lowercase()))
        {
            return Err(Error::Unauthorized);
        }
        let next = self.state.tx.checked_add(1).ok_or(Error::Replay)?;
        push(&mut head, "racer-receiver", &self.state.peer.0);
        push_binary(&mut head, "racer-session", &self.state.id);
        push(&mut head, "racer-direction", self.state.direction);
        push(&mut head, "racer-sequence", next);
        let signed = self.state.signatures.sign_fields(head)?;
        self.state.tx = next;
        Ok(signed.head)
    }

    /// Authenticate and admit exactly the next frame before exposing its fields.
    pub fn admit(&mut self, head: MessageHead) -> Result<MessageHead> {
        self.check()?;
        let signed = signed(head)?;
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
        for i in 0..MAX_HOPS {
            if let Some(hop) = head.unique(&format!("racer-hop-{i}"))? {
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
        head.headers
            .retain(|h| !is_auth_field(&h.name.to_ascii_lowercase()));
        self.state.rx = next;
        Ok(head)
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
        session.check()?;
        if session.peer() != peer {
            return Err(Error::Unauthorized.into());
        }
        return Ok(connection);
    }
    let scope = C::handshake_scope(parent)?;
    let io = &io.capped(65536);
    let a = random()?;
    let response = io
        .exchange_head(
            connection,
            session_head(&signatures, peer, "hello", &a, &[0; 32], false)?,
            &scope,
        )
        .await?;
    let (_, echoed, b) =
        verify_session(&signatures, response.value, Some(peer), "challenge", true)?;
    if echoed != a || b == [0; 32] {
        return Err(Error::Unauthorized.into());
    }
    connection = response.connection;
    connection.next_round()?;
    let response = io
        .exchange_head(
            connection,
            session_head(&signatures, peer, "finish", &a, &b, false)?,
            &scope,
        )
        .await?;
    let (_, echoed, remote) =
        verify_session(&signatures, response.value, Some(peer), "ready", true)?;
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
        session.check()?;
        return Ok(connection);
    }
    let scope = C::handshake_scope(parent)?;
    let io = &io.capped(65536);
    let incoming = io.receive_head(connection, &scope).await?;
    scope.check()?;
    let (peer, a, zero) = verify_session(&signatures, incoming.value, None, "hello", false)?;
    if a == [0; 32] || zero != [0; 32] {
        return Err(Error::Unauthorized.into());
    }
    scope.check()?;
    let b = random()?;
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
    let (_, echoed, remote) =
        verify_session(&signatures, incoming.value, Some(&peer), "finish", false)?;
    if echoed != a || remote != b {
        return Err(Error::Unauthorized.into());
    }
    scope.check()?;
    connection = io
        .send_head(
            incoming.connection,
            session_head(&signatures, &peer, "ready", &a, &b, true)?,
            &scope,
        )
        .await?
        .connection;
    scope.check()?;
    connection.next_round()?;
    let id = transcript(&peer, signatures.node(), &a, &b);
    install_session(&mut connection, Session::new(signatures, peer, id, 1))?;
    Ok(connection)
}

fn session_array<const N: usize>(head: &MessageHead, name: &str) -> Result<[u8; N]> {
    decode_binary(field(head, name)?.as_bytes())?
        .try_into()
        .map_err(|_| Error::Unauthorized)
}

fn signed(head: MessageHead) -> Result<SignedHead> {
    let value = field(&head, "signature")?;
    let value = value
        .strip_prefix("racer=:")
        .and_then(|v| v.strip_suffix(':'))
        .ok_or(Error::Unauthorized)?;
    Ok(SignedHead {
        signature: decode_binary(value.as_bytes())?,
        head,
    })
}

fn random() -> Result<[u8; 32]> {
    let mut bytes = [0; 32];
    uring_runtime::environment::fill_random(&mut bytes).map_err(|_| Error::Unavailable)?;
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
    push(&mut h, "content-length", 0);
    push(&mut h, "racer-receiver", &peer.0);
    push(&mut h, "racer-handshake", phase);
    push_binary(&mut h, "racer-client-challenge", a);
    push_binary(&mut h, "racer-server-challenge", b);
    Ok(signatures.sign(h)?.head)
}

fn verify_session(
    signatures: &Signatures,
    head: MessageHead,
    peer: Option<&NodeId>,
    phase: &str,
    response: bool,
) -> Result<(NodeId, [u8; 32], [u8; 32])> {
    let verified = signatures.verify_proof(signed(head)?)?;
    let h = &verified.signed.head;
    let start = if response {
        matches!(h.start, StartLine::Response { status: 200 })
    } else {
        matches!(&h.start, StartLine::Request { method, target } if method == "POST" && target == SESSION_TARGET)
    };
    if !start
        || h.content_length()? != Some(0)
        || field(h, "racer-handshake")? != phase
        || peer.is_some_and(|p| p != verified.peer.node())
    {
        return Err(Error::Unauthorized);
    }
    for field in &h.headers {
        if !is_auth_field(&field.name)
            && !matches!(
                field.name.as_str(),
                "content-length"
                    | "racer-handshake"
                    | "racer-client-challenge"
                    | "racer-server-challenge"
            )
        {
            return Err(Error::Unauthorized);
        }
    }
    Ok((
        verified.peer.node().clone(),
        session_array(h, "racer-client-challenge")?,
        session_array(h, "racer-server-challenge")?,
    ))
}

#[cfg(feature = "test-util")]
pub mod test_util {
    use super::*;

    /// Create deterministic socket state for adapter fault-injection tests.
    pub fn session(
        signatures: Rc<Signatures>,
        peer: NodeId,
        id: [u8; 32],
        direction: u8,
    ) -> Session {
        Session::new(signatures, peer, id, direction)
    }

    /// Generate one challenge using the active environment.
    pub fn random() -> Result<[u8; 32]> {
        super::random()
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
        super::verify_session(signatures, head, peer, phase, response)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn frame() -> MessageHead {
        MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: REQUEST_TARGET.into(),
            },
            headers: vec![Header::new("content-length", b"0")],
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
    fn duplicate_wrong_session_direction_identity_and_forged_high_sequence() {
        let n = crate::tests::network(2);
        let id = random().unwrap();
        let mut a = Session::new(n[0].clone(), n[1].node().clone(), id, 0);
        let mut b = Session::new(n[1].clone(), n[0].node().clone(), id, 1);
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
            let mut bad = Session::new(n[0].clone(), n[1].node().clone(), id, 0);
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
        let n = crate::tests::network(2);
        let id = random().unwrap();
        let mut a = Session::new(n[0].clone(), n[1].node().clone(), id, 0);
        let mut b = Session::new(n[1].clone(), n[0].node().clone(), id, 1);
        let old = a.sign(frame()).unwrap();
        let mut reconnected = Session::new(n[1].clone(), n[0].node().clone(), random().unwrap(), 1);
        assert!(reconnected.admit(copy(&old)).is_err());
        assert_eq!(reconnected.state.rx, 0);
        b.state.expires = uring_runtime::environment::now();
        assert!(matches!(b.admit(old), Err(Error::DeadlineExceeded)));
        a.state.tx = u64::MAX;
        assert!(matches!(a.sign(frame()), Err(Error::Replay)));
        reconnected.state.rx = u64::MAX;
        let mut sender = Session::new(n[0].clone(), n[1].node().clone(), reconnected.state.id, 0);
        assert!(matches!(
            reconnected.admit(sender.sign(frame()).unwrap()),
            Err(Error::Replay)
        ));
    }
}
