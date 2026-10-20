//! Per-worker facade over the node RDMA owner (`designs/racer-rdma-sessions.md`,
//! "Threading", "Link setup over HTTP", "Per-page protocol", and "Racer types").
//!
//! One [`rdma_verbs::Owner`] per node owns every link. Each I/O worker holds a
//! [`Links`] around its own [`rdma_verbs::Client`]. Pages never wait for a link:
//! while a link is not ready, pages use HTTP and [`Links`] sets the link up in the
//! background.

use super::link_setup;
use super::*;
use rdma_verbs::Arrival;
use rdma_verbs::Client;
use rdma_verbs::Epoch;
use rdma_verbs::LinkKey;
use rdma_verbs::LinkState;
use rdma_verbs::Op;
use rdma_verbs::Source;
use std::collections::HashMap;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::PoisonError;
use std::time::Duration;
use std::time::Instant;

/// Signed `Accept` extension naming the receiver's in-band grant.
pub const GRANT_HEADER: &str = "racer-rdma-grant";

/// Page buffer length: `MAX_CIPHERTEXT` rounded up to 4 KiB.
pub const BUFFER_LEN: usize = (MAX_CIPHERTEXT + 4095) & !4095;

/// A ready link with no grant, write, or release this long closes.
pub const LINK_IDLE: Duration = Duration::from_secs(120);

/// Incoming setups accepted per (peer, rail) per this interval.
pub const SETUP_INTERVAL: Duration = Duration::from_secs(1);

/// Background setup deadline.
pub const SETUP_DEADLINE: Duration = rdma_verbs::SETUP_TIMEOUT;

const GRANT_PREFIX: &[u8] = b"racer-rdma-grant-v2\0";
const GRANT_LEN: usize = GRANT_PREFIX.len() + 8 + 4 + 4 + 2;
const COMPLETE_PREFIX: &[u8] = b"racer-rdma-complete-v2\0";
const COMPLETE_LEN: usize = COMPLETE_PREFIX.len() + 8 + 4 + 4 + 16;

/// Page buffers a node-wide registered budget funds.
pub fn buffer_count(registered_bytes: usize) -> usize {
    registered_bytes / BUFFER_LEN
}

/// RDMA devices a node-wide registered budget funds out of `devices`.
/// Each device needs one send and one receive buffer at least.
pub fn funded_devices(registered_bytes: usize, devices: usize) -> usize {
    (buffer_count(registered_bytes) / 2).min(devices)
}

/// Truncate `publication` to the prefix a node-wide registered budget funds.
/// Activation and snapshot refresh both use this so they agree on the rails.
pub fn fund_publication<T>(publication: &mut Vec<T>, registered_bytes: usize) {
    publication.truncate(funded_devices(registered_bytes, publication.len()));
}

/// Grants per link: each concurrent page to a peer already needs one HTTP connection.
pub fn grants_per_link(connections_per_neighbor: usize) -> u32 {
    u32::try_from(connections_per_neighbor)
        .unwrap_or(u32::MAX)
        .clamp(1, link_setup::MAX_GRANTS_PER_LINK)
}

/// Opaque verbs key for a peer and rail: `SHA-256(NodeId)` and the rail.
pub fn link_key(peer: &NodeId, rail: RailId) -> LinkKey {
    LinkKey {
        peer: Sha256::digest(peer.0.as_bytes()).into(),
        rail: rail.0,
    }
}

/// Node-wide state shared by every worker's [`Links`].
#[derive(Default)]
pub struct LinkNode {
    /// Rails the owner activated; empty while inactive.
    active: Mutex<Vec<RailMapping>>,

    /// Activation finished (successfully or not) for the current round.
    settled: std::sync::atomic::AtomicBool,

    /// Last accepted incoming setup per key, for the rate limit.
    setups: Mutex<HashMap<LinkKey, Instant>>,

    /// Setups this node initiated and has not finished.
    initiating: Mutex<HashMap<LinkKey, Epoch>>,
}

impl LinkNode {
    fn lock<T>(mutex: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
        mutex.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// Rails active for this round.
    pub fn active(&self) -> Vec<RailMapping> {
        Self::lock(&self.active).clone()
    }

    /// Activation finished for this round, with or without rails.
    pub fn settled(&self) -> bool {
        self.settled.load(std::sync::atomic::Ordering::Acquire)
    }

    fn settle(&self, rails: Vec<RailMapping>) {
        *Self::lock(&self.active) = rails;
        self.settled
            .store(true, std::sync::atomic::Ordering::Release);
    }

    fn reset(&self) {
        Self::lock(&self.active).clear();
        self.settled
            .store(false, std::sync::atomic::Ordering::Release);
    }
}

/// Result of checking an incoming setup before it reaches the owner.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SetupAdmission {
    /// Accept the offer.
    Accept,

    /// Our own concurrent offer wins: answer `409 Conflict`.
    Conflict,

    /// Rate limited or overloaded: answer `429 Too Many Requests`.
    Busy,
}

/// Per-worker facade over the node owner.
pub struct Links {
    client: Client,

    node: Arc<LinkNode>,

    local: NodeId,

    grants_per_link: u32,

    retry: Rc<NativeRetry>,

    tasks: RefCell<Vec<Operation<'static, ()>>>,

    draining: Cell<bool>,

    /// This worker's crypto thread, which stages send copies.
    stager: Option<Rc<crate::security::CryptoClient>>,

    /// Sends staged on the crypto thread and sends left to the owner.
    #[cfg(test)]
    pub(crate) staging: Cell<(usize, usize)>,
}

impl Links {
    /// Wrap this worker's client. `reactor` arms native retry alarms.
    pub fn new(
        client: Client,
        node: Arc<LinkNode>,
        local: NodeId,
        connections_per_neighbor: usize,
        reactor: Option<Rc<crate::runtime::Reactor>>,
    ) -> Self {
        // Simulation uses explicitly fired one-shot alarms, never ClockPoll.
        #[cfg(test)]
        let reactor = reactor.filter(|_| racer_platform::environment::simulation_seed().is_none());
        Self {
            client,
            node,
            local,
            grants_per_link: grants_per_link(connections_per_neighbor),
            retry: reactor.map_or_else(NativeRetry::unattached, NativeRetry::new),
            tasks: RefCell::new(Vec::new()),
            draining: Cell::new(false),
            stager: None,
            #[cfg(test)]
            staging: Cell::new((0, 0)),
        }
    }

    /// Copy outgoing ciphertext into registered memory on `crypto`'s thread
    /// instead of the shared owner thread (`designs/racer-rdma-sessions.md`,
    /// "Zero-copy").
    pub fn with_stager(mut self, crypto: Rc<crate::security::CryptoClient>) -> Self {
        self.stager = Some(crypto);
        self
    }

    /// The local node.
    pub fn local(&self) -> &NodeId {
        &self.local
    }

    /// Node-wide shared state.
    pub fn node(&self) -> &Arc<LinkNode> {
        &self.node
    }

    /// Rails the node activated.
    pub fn active(&self) -> Vec<RailMapping> {
        self.node.active()
    }

    /// Whether `rail` is active and the node is not closing.
    pub fn rail_ready(&self, rail: RailId) -> bool {
        !self.draining.get()
            && !self.client.closed()
            && LinkNode::lock(&self.node.active)
                .iter()
                .any(|m| m.rail == rail)
    }

    /// Snapshot of the link to `peer` on `rail`.
    pub fn state(&self, peer: &NodeId, rail: RailId) -> LinkState {
        self.client.link(link_key(peer, rail))
    }

    /// Epoch and link id when the link is ready.
    pub fn ready(&self, peer: &NodeId, rail: RailId) -> Option<(Epoch, u64)> {
        if !self.rail_ready(rail) {
            return None;
        }
        match self.state(peer, rail) {
            LinkState::Ready { epoch, link_id } => Some((epoch, link_id)),
            _ => None,
        }
    }

    /// Page buffers of `rail` in `state`, for diagnostics and tests.
    pub fn buffers(&self, rail: RailId, state: rdma_verbs::BufferState) -> Option<usize> {
        self.client.buffers(rail.0, state)
    }

    /// Await an owner op within `scope`.
    pub(crate) async fn wait<T>(&self, op: Op<T>, scope: &RequestScope) -> Result<T> {
        self.retry.wait(scope, poll_fn(|cx| op.poll(cx))).await
    }

    /// Discover, select, and register the node's devices. Run on one worker only;
    /// the others read the result through [`LinkNode`].
    pub fn activate<'a>(
        &'a self,
        publication: Vec<RailMapping>,
        admission: &'a flow_control::Quotas<AdmissionPolicy>,
        registered_bytes: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<RailMapping>> {
        Box::pin(async move {
            scope.check()?;
            let result = self
                .activate_inner(publication, admission, registered_bytes, scope)
                .await;
            match &result {
                Ok(rails) => self.node.settle(rails.clone()),
                Err(Error::Cancelled | Error::DeadlineExceeded) => {}
                Err(_) => self.node.settle(Vec::new()),
            }
            result
        })
    }

    async fn activate_inner(
        &self,
        mut publication: Vec<RailMapping>,
        admission: &flow_control::Quotas<AdmissionPolicy>,
        registered_bytes: usize,
        scope: &RequestScope,
    ) -> Result<Vec<RailMapping>> {
        let mut rails = std::collections::BTreeSet::new();
        if publication.is_empty() || publication.iter().any(|nic| !rails.insert(nic.rail)) {
            return Err(Error::InvalidConfiguration);
        }
        // Racer's quota profile charges 4 KiB physical pages.
        validate_native_page_size(unsafe { libc::sysconf(libc::_SC_PAGESIZE) })?;
        self.client.reopen()?;
        let eligible = publication.len();
        fund_publication(&mut publication, registered_bytes);
        if publication.is_empty() {
            eprintln!("racer-dataplane: RACER_REGISTERED_BYTES funds no RDMA device; using HTTP");
            return Err(Error::Unavailable);
        }
        if publication.len() < eligible {
            eprintln!(
                "racer-dataplane: RACER_REGISTERED_BYTES funds {} of {eligible} RDMA devices",
                publication.len()
            );
        }
        let pool_bytes = buffer_count(registered_bytes / publication.len()) * BUFFER_LEN;
        let guard = admission
            .reserve(
                None,
                ResourceClass::Registered,
                pool_bytes * publication.len(),
            )
            .map(|q| Arc::new(q) as rdma_verbs::Guard)?;
        let requested = publication.clone();
        let selected = self
            .wait(
                self.client.activate(rdma_verbs::NodeConfig {
                    selector: Box::new(move |ports| {
                        match_publication(&requested, ports)
                            .map(|selected| {
                                selected.into_iter().map(|(r, i)| (r.rail.0, i)).collect()
                            })
                            .map_err(|e| match e {
                                Error::InvalidConfiguration => {
                                    rdma_verbs::Error::InvalidConfiguration
                                }
                                Error::Overloaded => rdma_verbs::Error::Overloaded,
                                _ => rdma_verbs::Error::Unavailable,
                            })
                    }),
                    pool_bytes,
                    buffer_len: BUFFER_LEN,
                    max_links: 2 * crate::topology::MAX_DEGREE,
                    grants_per_link: self.grants_per_link,
                    idle: LINK_IDLE,
                    mr_remote_write: false,
                    guard,
                }),
                scope,
            )
            .await?;
        Ok(selected
            .into_iter()
            .filter_map(|selected| {
                let mut mapping = publication
                    .iter()
                    .find(|m| {
                        u32::from(m.rail.0) == selected.tag
                            && m.device == selected.port.device
                            && m.port == selected.port.port
                    })?
                    .clone();
                mapping.numa_node = mapping.numa_node.or(selected.port.numa_node);
                mapping.gid = Some(selected.port.gid);
                Some(mapping)
            })
            .collect())
    }

    /// Close the node: every link breaks and the owner drains. A later
    /// [`Self::activate`] reopens once drained.
    pub fn close(&self) {
        self.node.reset();
        self.client.close();
    }

    /// Stop background setups and close the node. The owner's host drains it.
    pub fn drain(&self) -> Operation<'_, ()> {
        Box::pin(async move {
            self.draining.set(true);
            self.tasks.borrow_mut().clear();
            self.close();
            Ok(())
        })
    }

    /// Run `task` in the background on this worker. Driven by [`Self::poll`].
    pub fn spawn(&self, task: Operation<'static, ()>) {
        if !self.draining.get() {
            self.tasks.borrow_mut().push(task);
        }
    }

    /// Drive background setups. Returns how many are still running.
    pub fn poll(&self, cx: &mut std::task::Context<'_>) -> usize {
        let mut tasks = std::mem::take(&mut *self.tasks.borrow_mut());
        tasks.retain_mut(|task| task.as_mut().poll(cx).is_pending());
        let mut current = self.tasks.borrow_mut();
        tasks.append(&mut current);
        *current = tasks;
        current.len()
    }

    /// Background setups still running on this worker.
    pub fn setups(&self) -> usize {
        self.tasks.borrow().len()
    }

    /// Whether a background setup to `peer` on `rail` should start now: the rail
    /// is active, the link is idle, and no setup from this node is running.
    pub fn wants(&self, peer: &NodeId, rail: RailId) -> bool {
        self.rail_ready(rail)
            && peer != &self.local
            && self.state(peer, rail) == LinkState::Idle
            && !LinkNode::lock(&self.node.initiating).contains_key(&link_key(peer, rail))
    }

    /// Initiator: open the link and return its epoch and encoded v2 setup.
    pub async fn open(
        &self,
        peer: &NodeId,
        rail: RailId,
        scope: &RequestScope,
    ) -> Result<(Epoch, Vec<u8>)> {
        let key = link_key(peer, rail);
        {
            let mut initiating = LinkNode::lock(&self.node.initiating);
            if initiating.contains_key(&key) {
                return Err(Error::Overloaded);
            }
            // Reserve the key before the owner answers; replaced with the epoch.
            initiating.insert(key, Epoch(0));
        }
        let opened = match self.wait(self.client.open(key), scope).await {
            Ok(opened) => opened,
            Err(error) => {
                LinkNode::lock(&self.node.initiating).remove(&key);
                return Err(error);
            }
        };
        LinkNode::lock(&self.node.initiating).insert(key, opened.epoch);
        let setup = link_setup::Setup::new(
            rail,
            opened.setup.endpoint.to_bytes(),
            opened.setup.recv_credits,
            self.grants_per_link,
        );
        match setup {
            Ok(setup) => Ok((opened.epoch, setup.encode())),
            Err(error) => {
                self.abandon(peer, rail, opened.epoch);
                Err(error)
            }
        }
    }

    /// Initiator: finish with the responder's signed setup and binding.
    #[allow(clippy::too_many_arguments)] // Setup evidence is checked together.
    pub async fn connect(
        &self,
        peer: &NodeId,
        rail: RailId,
        epoch: Epoch,
        ours: &[u8],
        theirs: &[u8],
        binding: &[u8],
        scope: &RequestScope,
    ) -> Result<()> {
        let key = link_key(peer, rail);
        let result = async {
            link_setup::check_response(ours, theirs, binding)?;
            let remote = link_setup::Setup::decode(theirs, rail)?;
            let link_id = link_setup::LinkId::for_setup(ours, &remote.nonce);
            let remote = rdma_verbs::Setup {
                endpoint: rdma_verbs::Endpoint::from_bytes(&remote.endpoint)?,
                recv_credits: remote.recv_credits,
                grants_per_link: remote.grants_per_link,
            };
            self.wait(self.client.connect(key, epoch, remote, link_id.0), scope)
                .await
        }
        .await;
        if result.is_err() {
            self.client.break_link(key, epoch);
        }
        self.finish_initiating(key, epoch);
        result
    }

    /// Initiator: give up on a setup (the link backs off).
    pub fn abandon(&self, peer: &NodeId, rail: RailId, epoch: Epoch) {
        let key = link_key(peer, rail);
        self.client.break_link(key, epoch);
        self.finish_initiating(key, epoch);
    }

    fn finish_initiating(&self, key: LinkKey, epoch: Epoch) {
        let mut initiating = LinkNode::lock(&self.node.initiating);
        if initiating.get(&key) == Some(&epoch) {
            initiating.remove(&key);
        }
    }

    /// Responder: rate limit and tie-break an authenticated offer from `peer`.
    /// Does not touch the owner.
    pub fn admit_setup(&self, peer: &NodeId, rail: RailId) -> SetupAdmission {
        let key = link_key(peer, rail);
        if !self.rail_ready(rail) || peer == &self.local {
            return SetupAdmission::Busy;
        }
        if LinkNode::lock(&self.node.initiating).contains_key(&key)
            && link_setup::tie_break(&self.local, peer) == link_setup::TieBreak::Reject
        {
            return SetupAdmission::Conflict;
        }
        let now = racer_platform::environment::now();
        let mut setups = LinkNode::lock(&self.node.setups);
        if setups
            .get(&key)
            .is_some_and(|last| now < *last + SETUP_INTERVAL)
        {
            return SetupAdmission::Busy;
        }
        setups.retain(|_, last| now < *last + SETUP_INTERVAL);
        setups.insert(key, now);
        SetupAdmission::Accept
    }

    /// Responder: accept `remote` (already authenticated and admitted) and
    /// return our encoded setup. Replaces any existing link to `peer` on `rail`.
    pub async fn accept(
        &self,
        peer: &NodeId,
        rail: RailId,
        remote: &[u8],
        scope: &RequestScope,
    ) -> Result<Vec<u8>> {
        let key = link_key(peer, rail);
        let decoded = link_setup::Setup::decode(remote, rail)?;
        // The link id needs our nonce before the owner returns our endpoint.
        let mut nonce = [0; 16];
        while nonce == [0; 16] {
            racer_platform::environment::fill_random(&mut nonce).map_err(|_| Error::Io)?;
        }
        let link_id = link_setup::LinkId::for_setup(remote, &nonce);
        let opened = self
            .wait(
                self.client.accept(
                    key,
                    rdma_verbs::Setup {
                        endpoint: rdma_verbs::Endpoint::from_bytes(&decoded.endpoint)?,
                        recv_credits: decoded.recv_credits,
                        grants_per_link: decoded.grants_per_link,
                    },
                    link_id.0,
                    false,
                ),
                scope,
            )
            .await?;
        // Our own concurrent offer, if any, lost the tie-break.
        LinkNode::lock(&self.node.initiating).remove(&key);
        let setup = link_setup::Setup {
            rail,
            nonce,
            endpoint: opened.setup.endpoint.to_bytes(),
            recv_credits: opened.setup.recv_credits,
            grants_per_link: self.grants_per_link,
        };
        Ok(setup.encode())
    }

    /// Receiver: grant a page window on the ready link to `peer`.
    pub async fn grant(
        &self,
        peer: &NodeId,
        rail: RailId,
        transfer: TransferId,
        scope: &RequestScope,
    ) -> Result<PendingReceive> {
        scope.check()?;
        let (epoch, link) = self.ready(peer, rail).ok_or(Error::Unavailable)?;
        let length = MAX_CIPHERTEXT as u32;
        let granted = self
            .wait(
                self.client.grant(
                    link_key(peer, rail),
                    epoch,
                    transfer.0,
                    length,
                    scope.deadline.0,
                ),
                scope,
            )
            .await?;
        Ok(PendingReceive {
            retry: self.retry.clone(),
            link,
            tag: granted.tag,
            length,
            rail,
            transfer,
            arrival: granted.arrival,
        })
    }

    /// Sender: write `page` into the peer's authenticated grant with immediate.
    pub async fn send(
        &self,
        peer: &NodeId,
        grant: &AuthenticatedGrant,
        page: &CiphertextPage,
        scope: &RequestScope,
    ) -> Result<NativeSent> {
        scope.check()?;
        validate_envelope(page.envelope())?;
        let length = page.bytes().len();
        if length != page.envelope().ciphertext_length as usize || length > grant.length as usize {
            return Err(Error::InvalidRange);
        }
        let (epoch, link) = self.ready(peer, grant.rail).ok_or(Error::Unavailable)?;
        if link != grant.link {
            return Err(Error::Unavailable);
        }
        let source = match self.stage(grant.rail, page, scope).await? {
            Some(buffer) => Source::Staged(buffer),
            None => Source::Bytes(Arc::new(PageSource(page.clone()))),
        };
        let key = link_key(peer, grant.rail);
        let written = self
            .wait(
                self.client
                    .write(key, epoch, grant.tag, source, scope.deadline.0),
                scope,
            )
            .await;
        if let Err(error) = written {
            // Cancellation or the deadline can win after the owner already
            // consumed the tag. A `Release` for a consumed tag would wait for
            // a grant that never comes, so break this epoch instead.
            if matches!(error, Error::Cancelled | Error::DeadlineExceeded) {
                self.client.break_link(key, epoch);
            }
            return Err(error);
        }
        Ok(NativeSent {
            value: completion_value(grant.link, grant.tag, length as u32, grant.transfer),
        })
    }

    /// Copy `page` into a free send buffer of `rail` on the crypto thread.
    /// `None` leaves the copy to the owner (`Source::Bytes`): no stager, no free
    /// buffer right now, or a stager failure other than the request's own
    /// cancellation. A failed copy drops the buffer, which returns it to the
    /// pool.
    async fn stage(
        &self,
        rail: RailId,
        page: &CiphertextPage,
        scope: &RequestScope,
    ) -> Result<Option<rdma_verbs::SendBuffer>> {
        let Some(crypto) = &self.stager else {
            return Ok(None);
        };
        // Never wait for a buffer here: the owner queues `Source::Bytes`
        // writes itself, so take one only if it is free right now.
        let staged = match self.client.try_send_buffer(rail.0, page.bytes().len()) {
            Ok(Some(buffer)) => match crypto.stage(page.clone(), buffer, scope).await {
                Ok(buffer) => Some(buffer),
                Err(error @ (Error::Cancelled | Error::DeadlineExceeded)) => return Err(error),
                Err(_) => None,
            },
            Ok(None) | Err(_) => None,
        };
        #[cfg(test)]
        {
            let (yes, no) = self.staging.get();
            self.staging.set(if staged.is_some() {
                (yes + 1, no)
            } else {
                (yes, no + 1)
            });
        }
        Ok(staged)
    }

    /// Sender: decline the peer's grant if its link is still the granted one.
    pub fn release(&self, peer: &NodeId, grant: &AuthenticatedGrant) {
        if let Some((epoch, link)) = self.ready(peer, grant.rail)
            && link == grant.link
        {
            let key = link_key(peer, grant.rail);
            let release = self.client.release(key, epoch, grant.tag);
            // An op that failed before the owner saw it (full queue, owner
            // gone) never sends `Release`; the peer would hold the window until
            // its deadline. Break the epoch on the guaranteed path instead.
            let mut cx = std::task::Context::from_waker(std::task::Waker::noop());
            if let std::task::Poll::Ready(Err(_)) = release.poll(&mut cx) {
                self.client.break_link(key, epoch);
            }
        }
    }
}

/// Lends a ciphertext page to the owner as write source bytes.
struct PageSource(CiphertextPage);

impl AsRef<[u8]> for PageSource {
    fn as_ref(&self) -> &[u8] {
        self.0.bytes()
    }
}

pub(crate) fn completion_value(link: u64, tag: u32, length: u32, transfer: TransferId) -> Vec<u8> {
    let mut bytes = Vec::with_capacity(COMPLETE_LEN);
    bytes.extend_from_slice(COMPLETE_PREFIX);
    bytes.extend_from_slice(&link.to_be_bytes());
    bytes.extend_from_slice(&tag.to_be_bytes());
    bytes.extend_from_slice(&length.to_be_bytes());
    bytes.extend_from_slice(&transfer.0);
    bytes
}

fn grant_value(link: u64, tag: u32, length: u32, rail: RailId) -> Vec<u8> {
    let mut bytes = Vec::with_capacity(GRANT_LEN);
    bytes.extend_from_slice(GRANT_PREFIX);
    bytes.extend_from_slice(&link.to_be_bytes());
    bytes.extend_from_slice(&tag.to_be_bytes());
    bytes.extend_from_slice(&length.to_be_bytes());
    bytes.extend_from_slice(&rail.0.to_be_bytes());
    bytes
}

/// A receiver's granted window, named in the signed request.
pub struct PendingReceive {
    retry: Rc<NativeRetry>,
    link: u64,
    tag: u32,
    length: u32,
    rail: RailId,
    transfer: TransferId,
    arrival: Op<Arrival>,
}

impl PendingReceive {
    /// Base64 value of the signed `GRANT_HEADER` extension.
    pub fn header_value(&self) -> Vec<u8> {
        STANDARD
            .encode(grant_value(self.link, self.tag, self.length, self.rail))
            .into_bytes()
    }

    /// Link id the grant was made on.
    pub fn link(&self) -> u64 {
        self.link
    }

    /// Per-link tag.
    pub fn tag(&self) -> u32 {
        self.tag
    }

    /// Poll the arrival directly (tests of the fallback path).
    #[cfg(test)]
    pub(crate) fn poll_arrival(
        &self,
        cx: &mut std::task::Context<'_>,
    ) -> Poll<rdma_verbs::Result<Arrival>> {
        self.arrival.poll(cx)
    }

    /// Requires the signed `COMPLETION_HEADER` to name this link, tag, transfer,
    /// and the envelope's length, then waits for the page. A failed arrival after
    /// a signed completion is `Io`: the HTTP body was never sent.
    ///
    /// The ciphertext stays in registered memory; retaining paths copy it out
    /// (`BufferPool::retainable`).
    pub async fn finish(
        self,
        head: &VerifiedHead,
        envelope: PageEnvelope,
        admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
        scope: &RequestScope,
    ) -> Result<CiphertextPage> {
        scope.check()?;
        validate_envelope(&envelope)?;
        let length = envelope.ciphertext_length;
        if signed_value(head, COMPLETION_HEADER, COMPLETE_LEN)?
            != completion_value(self.link, self.tag, length, self.transfer)
            || length > self.length
        {
            return Err(Error::Unauthorized);
        }
        let reservation = admission.reserve(
            Some(&envelope.page.version.object.cache),
            ResourceClass::Ciphertext,
            length as usize,
        )?;
        let arrival = self
            .retry
            .wait(scope, poll_fn(|cx| self.arrival.poll(cx)))
            .await;
        let page = match arrival {
            Ok(Arrival::Page(page)) => page,
            Ok(Arrival::Released) => return Err(Error::Io),
            Err(error @ Error::Cancelled) => return Err(error),
            Err(_) => {
                scope.check()?;
                return Err(Error::Io);
            }
        };
        if page.len() != length as usize {
            return Err(Error::InvalidRange);
        }
        BufferPool::new(admission.clone()).registered_ciphertext(reservation, envelope, page)
    }
}

/// A grant named in a verified, explicitly signed request extension.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuthenticatedGrant {
    pub link: u64,
    pub tag: u32,
    pub length: u32,
    pub rail: RailId,
    pub transfer: TransferId,
}

impl AuthenticatedGrant {
    /// Decode the signed `GRANT_HEADER` for the request's transfer and rail.
    pub fn from_verified(head: &VerifiedHead, rail: RailId, transfer: TransferId) -> Result<Self> {
        Self::decode(
            &signed_value(head, GRANT_HEADER, GRANT_LEN)?,
            rail,
            transfer,
        )
    }

    fn decode(bytes: &[u8], rail: RailId, transfer: TransferId) -> Result<Self> {
        if bytes.len() != GRANT_LEN || !bytes.starts_with(GRANT_PREFIX) {
            return Err(Error::InvalidRequest);
        }
        let b = &bytes[GRANT_PREFIX.len()..];
        let grant = Self {
            link: u64::from_be_bytes(b[..8].try_into().unwrap()),
            tag: u32::from_be_bytes(b[8..12].try_into().unwrap()),
            length: u32::from_be_bytes(b[12..16].try_into().unwrap()),
            rail: RailId(u16::from_be_bytes([b[16], b[17]])),
            transfer,
        };
        if grant.rail != rail {
            return Err(Error::Unauthorized);
        }
        if grant.length == 0 || grant.length as usize > MAX_CIPHERTEXT {
            return Err(Error::InvalidRange);
        }
        Ok(grant)
    }
}

/// Produced only by a successful write-with-immediate completion.
pub struct NativeSent {
    value: Vec<u8>,
}

impl NativeSent {
    /// Base64 value of the signed `COMPLETION_HEADER` extension.
    pub fn header_value(&self) -> Vec<u8> {
        STANDARD.encode(&self.value).into_bytes()
    }
}

#[cfg(test)]
pub(crate) mod tests;
