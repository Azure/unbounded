//! Registration mode: devices without memory windows or implicit on-demand
//! paging (for example Azure MANA), and the connection manager that connects
//! their QPs.
//!
//! Such devices support RC QPs, RDMA write with immediate, and plain MRs, but
//! only connect QPs through the RDMA connection manager. The data path stays
//! zero copy:
//!
//! - A grant registers the granted page buffer itself as a remote-write MR
//!   and hands the peer that MR's rkey. The registration covers only that
//!   buffer (rounded up to whole 2 MiB pages inside it, see [`grant_span`]).
//!   Invalidation deregisters it, so the rkey stops working, exactly where a
//!   window would be invalidated. Both complete at once with a completion the
//!   QP's ledger produces itself.
//! - A write registers the caller's source memory for local access, posts
//!   the RDMA writes straight from it in [`CHUNK`] pieces, and deregisters it
//!   when the write completes. The last piece carries the immediate, whose top
//!   byte counts the pieces before it, so the receiver reports the whole
//!   length: Racer tags use only the low 24 bits.
//! - QPs connect through the connection manager, driven by [`Cm::progress`]
//!   from its nonblocking event channel: the initiator resolves the
//!   responder's address and route, creates its QP, and connects; the
//!   responder's listener accepts a request whose tokens match a QP that
//!   registered the initiator's token, and rejects (then destroys) any other.
//!   Every event is copied and acknowledged before any id is destroyed,
//!   because `rdma_destroy_id` waits for that.
//!
//! Nothing here spawns a thread or blocks on the network.
use super::*;

/// Page memory is 2 MiB aligned and huge-page backed; grant registrations
/// round up to whole pages of this size.
pub(crate) const HUGE: u64 = 2 << 20;

/// Writes go out in pieces of this size (at most a port's message size on
/// supported devices).
pub(crate) const CHUNK: u64 = 8 << 20;

/// Send queue entries per counted send: enough pieces for a 32 MiB write,
/// more than any Racer page needs.
pub(crate) const WRITE_WRS: u32 = 4;

/// Tags use the low 24 bits of the immediate; the top byte counts pieces.
pub(crate) const TAG_MASK: u32 = 0x00ff_ffff;

/// Magic in connection request private data: "RCR1".
const MAGIC: u32 = 0x5243_5231;

/// Address and route resolution timeout.
const RESOLVE_MS: c_int = 2000;

/// Default connection manager port.
const DEFAULT_PORT: u16 = 18515;

/// Connection manager event types (`enum rdma_cm_event_type`), pinned by C
/// static asserts.
pub(crate) mod event {
    pub(crate) const ADDR_RESOLVED: i32 = 0;

    pub(crate) const ADDR_ERROR: i32 = 1;

    pub(crate) const ROUTE_RESOLVED: i32 = 2;

    pub(crate) const ROUTE_ERROR: i32 = 3;

    pub(crate) const CONNECT_REQUEST: i32 = 4;

    pub(crate) const CONNECT_ERROR: i32 = 6;

    pub(crate) const UNREACHABLE: i32 = 7;

    pub(crate) const REJECTED: i32 = 8;

    pub(crate) const ESTABLISHED: i32 = 9;

    pub(crate) const DISCONNECTED: i32 = 10;

    pub(crate) const DEVICE_REMOVAL: i32 = 11;
}

/// The connection manager port: `RACER_RDMA_CM_PORT`, else 18515.
pub(crate) fn cm_port() -> u16 {
    std::env::var("RACER_RDMA_CM_PORT")
        .ok()
        .and_then(|v| v.parse::<u16>().ok())
        .filter(|p| *p != 0)
        .unwrap_or(DEFAULT_PORT)
}

/// The private data both sides exchange: magic, initiator token, responder
/// token, little endian.
pub(crate) fn request(initiator: u32, responder: u32) -> [u8; 12] {
    let mut out = [0; 12];
    out[..4].copy_from_slice(&MAGIC.to_le_bytes());
    out[4..8].copy_from_slice(&initiator.to_le_bytes());
    out[8..].copy_from_slice(&responder.to_le_bytes());
    out
}

/// Parse [`request`] data: `(initiator, responder)`.
pub(crate) fn parse_request(data: &[u8]) -> Option<(u32, u32)> {
    let word = |i: usize| u32::from_le_bytes(data[i..i + 4].try_into().unwrap());
    (data.len() >= 12 && word(0) == MAGIC).then(|| (word(4), word(8)))
}

/// The IPv4 address in an IPv4-mapped GID (`::ffff:a.b.c.d`).
pub(crate) fn ipv4_of(gid: &[u8; 16]) -> Option<[u8; 4]> {
    (gid[..10] == [0; 10] && gid[10..12] == [0xff; 2]).then(|| gid[12..].try_into().unwrap())
}

/// Bytes a grant registers: `length` rounded up to whole 2 MiB pages when
/// the grant starts on one, but never past `limit` (the bytes left in the
/// granted buffer from its start), else exactly `length`.
pub(crate) fn grant_span(address: u64, length: u32, limit: u64) -> u64 {
    let length = u64::from(length);
    if !address.is_multiple_of(HUGE) {
        return length;
    }
    let rounded = length.div_ceil(HUGE) * HUGE;
    if rounded <= limit { rounded } else { length }
}

/// Where a registration-mode QP is in its connection.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum State {
    /// Not started (initiator) or waiting for the request (responder).
    Idle,

    /// Initiator: resolving the address, then the route.
    Resolving,

    /// QP created; connecting or accepted, waiting for `ESTABLISHED`.
    Connecting,

    /// Data may flow.
    Established,

    /// The connection failed or ended; the QP must stop.
    Failed,
}

/// The QP handle and connection state the connection manager and the
/// owning [`NativeQueuePair`] share. In window mode only `qp` and `state`
/// are used.
pub(crate) struct Conn {
    /// The QP, once it exists (window mode: from creation).
    pub(crate) qp: Cell<Option<NonNull<c_void>>>,

    /// The connection manager id, once it exists.
    pub(crate) id: Cell<Option<NonNull<c_void>>>,

    /// The id moved off the device's shared event channel (see
    /// [`Cm::detach`]).
    pub(crate) detached: Cell<bool>,

    /// The QP's CQ, for both queues.
    pub(crate) cq: NonNull<c_void>,

    /// Hardware send queue depth.
    pub(crate) send_wrs: u32,

    /// Hardware receive queue depth.
    pub(crate) recv_wrs: u32,

    /// This QP's token: its `qpn` in the setup exchange.
    pub(crate) token: u32,

    /// The peer's token once connecting.
    pub(crate) peer: Cell<u32>,

    pub(crate) state: Cell<State>,

    /// Receives posted before the QP exists, replayed when it is created,
    /// before the connection is accepted or requested.
    pub(crate) queued: RefCell<Vec<(u64, Sge)>>,
}

impl Conn {
    /// A connection with no QP yet.
    pub(crate) fn new(cq: NonNull<c_void>, send_wrs: u32, recv_wrs: u32, token: u32) -> Self {
        Self {
            qp: Cell::new(None),
            id: Cell::new(None),
            cq,
            send_wrs,
            recv_wrs,
            token,
            detached: Cell::new(false),
            peer: Cell::new(0),
            state: Cell::new(State::Idle),
            queued: RefCell::new(Vec::new()),
        }
    }
}

/// One device's connection manager: a nonblocking event channel and a
/// listener on the port's IPv4 address.
pub(crate) struct Cm {
    channel: NonNull<c_void>,

    fd: c_int,

    listener: Cell<Option<NonNull<c_void>>>,

    /// The local IPv4 address.
    pub(crate) local: [u8; 4],

    port: u16,

    /// Registered QPs by token.
    tokens: RefCell<BTreeMap<u32, Weak<Conn>>>,

    /// QPs by connection manager id.
    ids: RefCell<BTreeMap<usize, Weak<Conn>>>,

    /// Last simulated token, so simulations stay deterministic.
    next_token: Cell<u32>,

    /// Ids whose destruction failed. Never freed.
    pub(crate) leaked_ids: Cell<usize>,

    /// A `DEVICE_REMOVAL` event arrived: the device is gone (for example an
    /// Azure VF hot-unplug), and every id and QP on it is dead.
    removed: Cell<bool>,
}

impl Cm {
    /// Create the channel and listen on `local`:`port`. `None` on failure,
    /// with everything created released, or `Err` if that cleanup failed.
    pub(crate) fn start(
        api: &Api,
        device: NonNull<c_void>,
        local: [u8; 4],
        port: u16,
    ) -> std::result::Result<Option<Self>, ()> {
        let Some(channel) = NonNull::new(unsafe { (api.cm_channel)(device.as_ptr()) }) else {
            return Ok(None);
        };
        let fd = unsafe { (api.cm_channel_fd)(channel.as_ptr()) };
        let mut listener = std::ptr::null_mut();
        if unsafe {
            (api.cm_listen)(
                channel.as_ptr(),
                device.as_ptr(),
                local.as_ptr(),
                port,
                &mut listener,
            )
        } != 0
        {
            return if unsafe { (api.cm_channel_free)(channel.as_ptr()) } == 0 {
                Ok(None)
            } else {
                Err(())
            };
        }
        Ok(Some(Self {
            channel,
            fd,
            listener: Cell::new(NonNull::new(listener)),
            local,
            port,
            tokens: RefCell::new(BTreeMap::new()),
            ids: RefCell::new(BTreeMap::new()),
            next_token: Cell::new(0),
            leaked_ids: Cell::new(0),
            removed: Cell::new(false),
        }))
    }

    /// A connection manager over `channel` with no listener, for tests.
    #[cfg(test)]
    pub(crate) fn test_only(channel: NonNull<c_void>) -> Self {
        Self {
            channel,
            fd: -1,
            listener: Cell::new(None),
            local: [0; 4],
            port: 0,
            tokens: RefCell::new(BTreeMap::new()),
            ids: RefCell::new(BTreeMap::new()),
            next_token: Cell::new(0),
            leaked_ids: Cell::new(0),
            removed: Cell::new(false),
        }
    }

    /// True once any id on this device, the listener included, reported
    /// `DEVICE_REMOVAL`.
    pub(crate) fn removed(&self) -> bool {
        self.removed.get()
    }

    /// The event channel's nonblocking fd.
    pub(crate) fn fd(&self) -> c_int {
        self.fd
    }

    /// Destroy the listener, freeing its address and port for another
    /// device (the same VF back under a new name, say). False if it leaked.
    pub(crate) fn stop_listening(&self, api: &Api) -> bool {
        if let Some(listener) = self.listener.get() {
            if unsafe { (api.cm_destroy_id)(listener.as_ptr()) } != 0 {
                return false;
            }
            self.listener.set(None);
        }
        true
    }

    /// Destroy the listener and the channel. False if either leaked.
    pub(crate) fn close(&mut self, api: &Api) -> bool {
        if !self.stop_listening(api) {
            return false;
        }
        self.leaked_ids.get() == 0 && unsafe { (api.cm_channel_free)(self.channel.as_ptr()) } == 0
    }

    /// A fresh nonzero 24-bit token no live QP holds.
    pub(crate) fn token(&self, simulated: bool, rng: &racer_io::Rng) -> Result<u32> {
        let tokens = self.tokens.borrow();
        for _ in 0..1024 {
            let token = if simulated {
                self.next_token.set((self.next_token.get() + 1) & TAG_MASK);
                self.next_token.get()
            } else {
                let mut bytes = [0; 4];
                rng.fill(&mut bytes).map_err(|_| Error::Io)?;
                u32::from_le_bytes(bytes) & TAG_MASK
            };
            if token != 0 && !tokens.contains_key(&token) {
                return Ok(token);
            }
        }
        Err(Error::Unavailable)
    }

    /// Register `conn` under its token.
    pub(crate) fn register(&self, conn: &Rc<Conn>) {
        self.tokens
            .borrow_mut()
            .insert(conn.token, Rc::downgrade(conn));
    }

    /// Forget `conn`'s token.
    pub(crate) fn unregister(&self, conn: &Conn) {
        self.tokens.borrow_mut().remove(&conn.token);
    }

    /// Stop routing events to `id`: it is being destroyed elsewhere.
    pub(crate) fn forget(&self, id: NonNull<c_void>) {
        self.ids.borrow_mut().remove(&(id.as_ptr() as usize));
    }

    /// Move `conn`'s id to a channel of its own, so the shared channel no
    /// longer reports its events, before another thread destroys its QP.
    /// Reading some events makes librdmacm modify the id's QP (to error on
    /// `REJECTED`, to RTR and RTS on a connect response), which would use a
    /// QP freed concurrently. Every event read for the id must be
    /// acknowledged already, as [`Self::progress`] does. True if moved, now
    /// or earlier; false if the id stays on the shared channel.
    pub(crate) fn detach(&self, api: &Api, conn: &Conn) -> bool {
        if conn.detached.get() {
            return true;
        }
        let Some(id) = conn.id.get() else {
            return true;
        };
        if unsafe { (api.cm_detach)(id.as_ptr()) } != 0 {
            return false;
        }
        conn.detached.set(true);
        true
    }

    /// Destroy `conn`'s id, if any. Its QP must already be gone. False if
    /// the id could not be destroyed (it stays on `conn`).
    pub(crate) fn destroy_id(&self, api: &Api, conn: &Conn) -> bool {
        let Some(id) = conn.id.get() else {
            return true;
        };
        if unsafe { (api.cm_destroy_id)(id.as_ptr()) } != 0 {
            return false;
        }
        self.ids.borrow_mut().remove(&(id.as_ptr() as usize));
        conn.id.set(None);
        true
    }

    /// Start the initiator side: resolve the responder's address. The rest
    /// runs from [`Self::progress`].
    pub(crate) fn connect(&self, api: &Api, conn: &Rc<Conn>, remote: [u8; 4]) -> Result<()> {
        let mut id = std::ptr::null_mut();
        if unsafe { (api.cm_id)(self.channel.as_ptr(), &mut id) } != 0 {
            return Err(Error::Unavailable);
        }
        let id = NonNull::new(id).ok_or(Error::Unavailable)?;
        conn.id.set(Some(id));
        self.ids
            .borrow_mut()
            .insert(id.as_ptr() as usize, Rc::downgrade(conn));
        conn.state.set(State::Resolving);
        if unsafe {
            (api.cm_resolve_addr)(
                id.as_ptr(),
                self.local.as_ptr(),
                remote.as_ptr(),
                self.port,
                RESOLVE_MS,
            )
        } != 0
        {
            conn.state.set(State::Failed);
            return Err(Error::Unavailable);
        }
        Ok(())
    }

    /// Handle every waiting event without blocking.
    pub(crate) fn progress(&self, device: &NativeDevice) {
        loop {
            let mut event = CmEvent::default();
            if unsafe { (device.api.cm_get_event)(self.channel.as_ptr(), &mut event) } != 0 {
                return;
            }
            // Acknowledge first: the copy holds everything we need, and
            // destroying any id below waits for its events' acknowledgments.
            unsafe { (device.api.cm_ack)(event.handle) };
            self.handle(device, &event);
        }
    }

    /// The live connection for an event's id.
    fn conn(&self, id: *mut c_void) -> Option<Rc<Conn>> {
        self.ids.borrow().get(&(id as usize))?.upgrade()
    }

    fn handle(&self, device: &NativeDevice, event: &CmEvent) {
        let api = &device.api;
        if event.kind == event::DEVICE_REMOVAL {
            // Reported on every id bound to the device, the listener too, so
            // the owner learns of the loss even with no link on the rail.
            self.removed.set(true);
        }
        if event.kind == event::CONNECT_REQUEST {
            self.request(device, event);
            return;
        }
        let Some(conn) = self.conn(event.id) else {
            return;
        };
        let id = event.id;
        match event.kind {
            event::ADDR_RESOLVED if conn.state.get() == State::Resolving => {
                if unsafe { (api.cm_resolve_route)(id, RESOLVE_MS) } != 0 {
                    fail(api, &conn);
                }
            }
            event::ROUTE_RESOLVED if conn.state.get() == State::Resolving => {
                let data = request(conn.token, conn.peer.get());
                let ok = create_qp(device, &conn, id)
                    && unsafe { (api.cm_connect)(id, data.as_ptr(), data.len() as u32) } == 0;
                if ok {
                    conn.state.set(State::Connecting);
                } else {
                    fail(api, &conn);
                }
            }
            event::ESTABLISHED if conn.state.get() == State::Connecting => {
                conn.state.set(State::Established);
            }
            event::ADDR_ERROR
            | event::ROUTE_ERROR
            | event::CONNECT_ERROR
            | event::UNREACHABLE
            | event::REJECTED
            | event::DISCONNECTED
            | event::DEVICE_REMOVAL => fail(api, &conn),
            _ => {}
        }
    }

    /// Accept a request whose tokens match a waiting responder QP; reject
    /// and destroy any other.
    fn request(&self, device: &NativeDevice, event: &CmEvent) {
        let api = &device.api;
        let id = event.id;
        let data = &event.private[..(event.private_len as usize).min(event.private.len())];
        let conn = parse_request(data).and_then(|(initiator, responder)| {
            let conn = self.tokens.borrow().get(&responder)?.upgrade()?;
            (conn.state.get() == State::Idle
                && conn.qp.get().is_none()
                && conn.id.get().is_none()
                && conn.peer.get() != 0
                && conn.peer.get() == initiator)
                .then_some(conn)
        });
        if let Some(conn) = conn
            && let Some(raw) = NonNull::new(id)
        {
            conn.id.set(Some(raw));
            self.ids
                .borrow_mut()
                .insert(id as usize, Rc::downgrade(&conn));
            let reply = request(conn.peer.get(), conn.token);
            if create_qp(device, &conn, id)
                && unsafe { (api.cm_accept)(id, reply.as_ptr(), reply.len() as u32) } == 0
            {
                conn.state.set(State::Connecting);
                return;
            }
            // Leave the id and any QP on the connection: its stop destroys
            // both in order, and the initiator sees the connection fail.
            let _ = unsafe { (api.cm_reject)(id) };
            fail(api, &conn);
            return;
        }
        let _ = unsafe { (api.cm_reject)(id) };
        // Acknowledged already, so this does not wait.
        if unsafe { (api.cm_destroy_id)(id) } != 0 {
            self.leaked_ids.set(self.leaked_ids.get() + 1);
        }
    }
}

/// Create `conn`'s QP on `id` and replay its queued receives.
fn create_qp(device: &NativeDevice, conn: &Conn, id: *mut c_void) -> bool {
    let api = &device.api;
    // The adapter writes the real QP number; the simulation names the QP by
    // the token passed in.
    let mut qpn = conn.token;
    let Some(qp) = NonNull::new(unsafe {
        (api.cm_create_qp)(
            id,
            device.raw.as_ptr(),
            conn.cq.as_ptr(),
            conn.send_wrs,
            conn.recv_wrs,
            &mut qpn,
        )
    }) else {
        return false;
    };
    conn.qp.set(Some(qp));
    let queued = std::mem::take(&mut *conn.queued.borrow_mut());
    queued
        .iter()
        .all(|(wr, sge)| unsafe { (api.post_recv)(qp.as_ptr(), sge, 1, *wr) } == 0)
}

/// Mark `conn` failed and flush its QP, so its link breaks promptly.
fn fail(api: &Api, conn: &Conn) {
    conn.state.set(State::Failed);
    if let Some(qp) = conn.qp.get() {
        let _ = unsafe { (api.qp_error)(qp.as_ptr()) };
    }
}

/// Post a write of `length` bytes of caller memory at `address` (a virtual
/// address) to the peer's `remote` through `rkey`: register the source for
/// local access (a 2 MiB aligned head and a small tail, so the huge-page part
/// registers fast), then post it in [`CHUNK`] pieces, the last signaled with
/// the immediate. Registrations go to `mrs`, released when the write
/// completes. Checks everything before registering anything.
#[allow(clippy::too_many_arguments)]
pub(crate) fn post_dynamic(
    device: &NativeDevice,
    qp: NonNull<c_void>,
    address: u64,
    length: u32,
    remote: u64,
    rkey: u32,
    imm: u32,
    id: u64,
    mrs: &mut Vec<NonNull<c_void>>,
    held: &RefCell<Vec<NonNull<c_void>>>,
) -> c_int {
    let api = &device.api;
    let total = u64::from(length);
    let pieces = total.div_ceil(CHUNK);
    if imm & !TAG_MASK != 0 || pieces == 0 || pieces > u64::from(WRITE_WRS) {
        return libc::EINVAL;
    }
    let head = if address.is_multiple_of(HUGE) {
        total & !(HUGE - 1)
    } else {
        0
    };
    let mut keys = [0u32; 2];
    for (part, (start, len)) in [(address, head), (address + head, total - head)]
        .into_iter()
        .enumerate()
    {
        if len == 0 {
            continue;
        }
        let (mut lkey, mut rkey) = (0, 0);
        let Some(mr) = NonNull::new(unsafe {
            (api.reg_mr)(
                device.raw.as_ptr(),
                start,
                len,
                Access::CONTROL.bits(),
                &mut lkey,
                &mut rkey,
            )
        }) else {
            release(device, std::mem::take(mrs), held);
            return libc::ENOMEM;
        };
        keys[part] = lkey;
        mrs.push(mr);
    }
    let imm = imm | ((pieces as u32 - 1) << 24);
    let mut offset = 0;
    for piece in 0..pieces {
        let end = (offset + CHUNK).min(total);
        let mut sges = [Sge::default(); 2];
        let mut n = 0;
        let mut at = offset;
        while at < end {
            let part = usize::from(at >= head);
            let stop = if part == 1 { end } else { end.min(head) };
            sges[n] = Sge {
                addr: address + at,
                length: (stop - at) as u32,
                lkey: keys[part],
            };
            n += 1;
            at = stop;
        }
        let last = piece + 1 == pieces;
        let (flags, wr) = if last {
            (POST_IMM | POST_SIGNALED, id)
        } else {
            (0, 0)
        };
        let rc = unsafe {
            (api.post_write)(
                qp.as_ptr(),
                sges.as_ptr(),
                n as u32,
                remote + offset,
                rkey,
                imm,
                flags,
                wr,
            )
        };
        if rc != 0 {
            if piece == 0 {
                release(device, std::mem::take(mrs), held);
            } else {
                // Earlier pieces may already be in flight: keep the
                // registrations until the QP stops.
                held.borrow_mut().append(mrs);
            }
            return rc;
        }
        offset = end;
    }
    0
}

/// Deregister `mrs`, keeping any that fail in `held` for a retry at stop.
pub(crate) fn release(
    device: &NativeDevice,
    mrs: Vec<NonNull<c_void>>,
    held: &RefCell<Vec<NonNull<c_void>>>,
) {
    for mr in mrs {
        if unsafe { (device.api.dereg_mr)(mr.as_ptr()) } != 0 {
            held.borrow_mut().push(mr);
        }
    }
}

#[cfg(test)]
mod tests {
    //! Pure registration-mode helpers.
    use super::*;

    #[test]
    /// Grants round up to whole huge pages only from an aligned start, and
    /// never past the granted buffer.
    fn grant_span_rounds_within_the_buffer() {
        assert_eq!(grant_span(HUGE, 10, 2 * HUGE), HUGE);
        assert_eq!(grant_span(0, HUGE as u32 + 1, 2 * HUGE), 2 * HUGE);
        // Unaligned start: exact length.
        assert_eq!(grant_span(4096, 10, 2 * HUGE), 10);
        // Rounding would leave the buffer: exact length.
        assert_eq!(grant_span(0, 10, 4096), 10);
        assert_eq!(grant_span(0, HUGE as u32, HUGE), HUGE);
    }

    #[test]
    /// Private data round-trips; short or foreign data is refused.
    fn request_data_round_trips_and_rejects_garbage() {
        assert_eq!(parse_request(&request(7, 9)), Some((7, 9)));
        assert_eq!(parse_request(&request(7, 9)[..11]), None);
        let mut bad = request(7, 9);
        bad[0] ^= 1;
        assert_eq!(parse_request(&bad), None);
        assert_eq!(parse_request(&[]), None);
    }

    #[test]
    /// Only IPv4-mapped GIDs name an IPv4 address.
    fn ipv4_mapped_gids_only() {
        let mut gid = [0; 16];
        gid[10] = 0xff;
        gid[11] = 0xff;
        gid[12..].copy_from_slice(&[10, 0, 0, 7]);
        assert_eq!(ipv4_of(&gid), Some([10, 0, 0, 7]));
        gid[0] = 0xfe;
        assert_eq!(ipv4_of(&gid), None);
        assert_eq!(ipv4_of(&[0; 16]), None);
    }

    #[test]
    /// The port comes from the environment when valid.
    fn cm_port_defaults_and_overrides() {
        // Other tests do not set the variable; read it as the process has it.
        let expected = std::env::var("RACER_RDMA_CM_PORT")
            .ok()
            .and_then(|v| v.parse::<u16>().ok())
            .filter(|p| *p != 0)
            .unwrap_or(DEFAULT_PORT);
        assert_eq!(cm_port(), expected);
    }
}
