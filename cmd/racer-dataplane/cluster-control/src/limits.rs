//! Fixed resource bounds of the control thread.

use std::time::Duration;

/// Seat and primary Lease duration.
pub const LEASE_DURATION: Duration = Duration::from_secs(30);

/// Lease renew interval.
pub const LEASE_RENEW: Duration = Duration::from_secs(10);

/// Writes and serving stop when the Lease was last renewed longer ago than this.
/// It is shorter than [`LEASE_DURATION`] so a new holder never overlaps.
pub const LEASE_FRESHNESS: Duration = Duration::from_secs(20);

/// Committed state older than this, by local clock, is not served.
///
/// Every seat rereads the state Secret at least every
/// [`crate::seat::FOLLOW_INTERVAL`] (5s), so 30s tolerates several failed
/// reads while bounding how long a seat that cannot reach the API keeps
/// serving a stale image. Seat Lease freshness ([`LEASE_FRESHNESS`]) is the
/// tighter bound on authority in the common case.
pub const STATE_FRESHNESS: Duration = Duration::from_secs(30);

/// Deadline for one Kubernetes API request without a watch.
pub const API_TIMEOUT: Duration = Duration::from_secs(10);

/// Server-side watch timeout.
pub const WATCH_TIMEOUT: Duration = Duration::from_secs(290);

/// Deadline to finish a TLS handshake, to read a request head after its first
/// byte, to read a request body, or to write a small response.
pub const IO_TIMEOUT: Duration = Duration::from_secs(10);

/// Idle time allowed between requests on one connection.
pub const IDLE_TIMEOUT: Duration = Duration::from_secs(90);

/// Minimum response write rate. A response gets [`IO_TIMEOUT`] plus its size
/// at this rate. 64 MiB is then about 11 minutes, and authority still cuts a
/// write short at any chunk.
pub const MIN_WRITE_BYTES_PER_SEC: u64 = 100 * 1024;

/// Maximum concurrent inbound control connections.
///
/// Sizing: 100k Nodes over [`crate::annotations::MAX_SEATS`] (16) seats is
/// 6250 Nodes per seat, each holding a snapshot and a keyring long poll:
/// 12500 connections. The rest is headroom for enrollment, replicas, and
/// clients that fail over from a lost seat. Idle connections hold only TLS
/// state (no plaintext buffer, see `Conn::wait_for_data`), and full
/// publication bodies are shared, never copied per connection. The effective
/// cap is lowered further when `RLIMIT_NOFILE` is smaller, see
/// [`connection_capacity`].
///
/// Connections are not the binding limit when a publication lands. All
/// waiting long polls wake together, and at most [`MAX_WRITES`] (64) write
/// at once. The rest queue for a slot in FIFO order (`exec::Slots::acquire`,
/// [`MAX_WRITE_WAITERS`], [`WRITE_QUEUE_GRACE`]) instead of answering 429, so
/// a seat with 12500 polls drains a new publication in about 12500 / 64
/// write rounds with no client retries. Waking W polls costs O(W): each
/// `Signal::changed` waiter owns one waker slot and re-registers in O(1)
/// (see [`crate::step::cost::notify`]).
pub const MAX_CONTROL_CONNECTIONS: usize = 16384;

/// Maximum concurrent TLS handshakes, the CPU-heavy stage of a connection.
pub const MAX_HANDSHAKES: usize = 256;

/// Descriptors kept back from [`MAX_CONTROL_CONNECTIONS`] for listeners,
/// Kubernetes API clients, webhook connections, and the runtime.
pub const RESERVED_FDS: usize = 256;

/// Effective control connection cap for a descriptor limit.
pub fn connection_capacity(nofile: u64) -> usize {
    let usable = usize::try_from(nofile)
        .unwrap_or(usize::MAX)
        .saturating_sub(RESERVED_FDS + MAX_WEBHOOK_CONNECTIONS);
    usable.clamp(1, MAX_CONTROL_CONNECTIONS)
}

/// Raise the soft `RLIMIT_NOFILE` toward what [`MAX_CONTROL_CONNECTIONS`]
/// needs, within the hard limit, and return the resulting soft limit.
pub fn raise_nofile() -> u64 {
    let want = (MAX_CONTROL_CONNECTIONS + MAX_WEBHOOK_CONNECTIONS + RESERVED_FDS) as libc::rlim_t;
    let mut l = libc::rlimit {
        rlim_cur: 0,
        rlim_max: 0,
    };
    // SAFETY: valid out pointer for one rlimit.
    if unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &raw mut l) } != 0 {
        return 1024;
    }
    if l.rlim_cur < want {
        let next = libc::rlimit {
            rlim_cur: want.min(l.rlim_max),
            rlim_max: l.rlim_max,
        };
        // SAFETY: valid pointer to an rlimit within the hard limit.
        if unsafe { libc::setrlimit(libc::RLIMIT_NOFILE, &raw const next) } == 0 {
            l.rlim_cur = next.rlim_cur;
        }
    }
    #[allow(clippy::unnecessary_cast)] // rlim_t is not u64 everywhere.
    let soft = l.rlim_cur as u64;
    soft
}

/// Maximum concurrent inbound webhook connections.
pub const MAX_WEBHOOK_CONNECTIONS: usize = 64;

/// Maximum concurrent bootstrap requests, including TokenReview and API reads.
pub const MAX_BOOTSTRAPS: usize = 32;

/// Maximum concurrent responses with a body (publications, keyrings,
/// enrollments). A slot is held until the last byte is written. Bodies are
/// shared, so this bounds bandwidth and TLS buffers, not copies.
pub const MAX_WRITES: usize = 64;

/// Maximum responses queued for a write slot per server. Woken long polls
/// wait here instead of answering 429 when the only problem is that all
/// [`MAX_WRITES`] slots are busy. Sized to the connection bound, so every
/// connection may wait; the deadline ([`WRITE_QUEUE_GRACE`]) is what turns
/// sustained overload into 429.
pub const MAX_WRITE_WAITERS: usize = MAX_CONTROL_CONNECTIONS;

/// How long past its poll deadline a woken long poll may wait for a write
/// slot. Clients give a long poll `POLL_WAIT` plus 10 s
/// (`src/control/session.rs` `turn`), so 5 s of queueing leaves 5 s to
/// write. The wait is also bounded by seat authority and the caller's
/// credential. Past it the poll answers 429, as before.
pub const WRITE_QUEUE_GRACE: Duration = Duration::from_secs(5);

/// Maximum concurrent outbound Kubernetes API requests.
pub const MAX_API_REQUESTS: usize = 48;

/// Maximum request head size.
pub const MAX_HEAD_BYTES: usize = 16 * 1024;

/// Maximum Kubernetes API response, except watch streams.
pub const MAX_API_RESPONSE: usize = 256 * 1024 * 1024;

/// Maximum single watch event line.
pub const MAX_WATCH_EVENT: usize = 4 * 1024 * 1024;

/// Maximum AdmissionReview request body.
pub const MAX_ADMISSION_BYTES: usize = 3 * 1024 * 1024;

/// Maximum bearer token length.
pub const MAX_TOKEN_BYTES: usize = 16 * 1024;

/// Longest RDMA device name discovery can report. Discovery copies
/// `ibv_get_device_name` into the 64-byte `name` of `struct rdma_verbs_port`
/// and skips any port whose name does not fit with its NUL terminator
/// (`verbs/native/verbs.c`, mirrored by `Port::name` in `verbs/src/ffi.rs`;
/// libibverbs bounds names by `IBV_SYSFS_NAME_MAX` (64) as well). This crate
/// does not depend on `rdma-verbs`, so the value is restated here.
///
/// The wire itself does not bound device names, so a wire-valid report with
/// longer names is accepted only while the whole report fits.
pub const MAX_REPORT_DEVICE_BYTES: usize = 63;

/// Largest canonical report for the largest discoverable NIC inventory. It
/// does not cover every wire-valid report, since wire device names are
/// unbounded.
///
/// The worst case NIC has a device name of [`MAX_REPORT_DEVICE_BYTES`] control
/// characters, each escaped by JSON as `\u00XX` (6 bytes; other UTF-8 is not
/// escaped and costs at most its byte length), and every optional and numeric
/// field at its widest:
/// `{"device":"..","port":255,"rail":65535,"gid":"<32 hex>","numa_node":4294967295}`.
/// The report holds the wire maximum of 64 (`normalize_rails`) of them in
/// `{"shares":4294967295,"rdma_nics":[..]}`.
pub const MAX_REPORT_INVENTORY_BYTES: usize = {
    const NIC: usize = r#"{"device":"","port":255,"rail":65535,"gid":"","numa_node":4294967295}"#
        .len()
        + MAX_REPORT_DEVICE_BYTES * 6
        + 32;
    const RAILS: usize = 64;
    r#"{"shares":4294967295,"rdma_nics":[]}"#.len() + RAILS * NIC + (RAILS - 1)
};

/// Maximum Pod report annotation length. It is the inventory bound rounded up
/// to 32 KiB, well below the 256 KiB Kubernetes limit on all annotations of
/// an object. `deploy/racer/policies.yaml.tmpl` enforces the same value in
/// UTF-8 bytes; `report_limit_matches_policy` checks they agree.
pub const MAX_REPORT_BYTES: usize = 32 * 1024;

const _: () = assert!(MAX_REPORT_INVENTORY_BYTES <= MAX_REPORT_BYTES);
const _: () = assert!(MAX_REPORT_BYTES <= 256 * 1024 / 4);

/// Members per seat when seats are automatic.
pub const MEMBERS_PER_SEAT: usize = 2048;

/// Minimum automatic seat count.
pub const MIN_AUTO_SEATS: usize = 3;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn capacity_fits_target_scale() {
        // 100k Nodes, 16 seats, two long polls per Node.
        let per_seat = 100_000usize.div_ceil(crate::annotations::MAX_SEATS) * 2;
        assert!(MAX_CONTROL_CONNECTIONS >= per_seat + MAX_BOOTSTRAPS + MAX_HANDSHAKES);
        const { assert!(MAX_HANDSHAKES < MAX_CONTROL_CONNECTIONS) };
        assert_eq!(
            connection_capacity(1024),
            1024 - RESERVED_FDS - MAX_WEBHOOK_CONNECTIONS
        );
        assert_eq!(connection_capacity(1 << 20), MAX_CONTROL_CONNECTIONS);
        assert_eq!(connection_capacity(0), 1);
    }

    #[test]
    fn report_limit_matches_policy() {
        let policy = include_str!("../../../../deploy/racer/policies.yaml.tmpl");
        let check = format!(
            "size(bytes(variables.annotations['racer.unbounded-cloud.io/report'])) <= {MAX_REPORT_BYTES}"
        );
        assert_eq!(policy.matches(&check).count(), 1, "{check}");
        let message = format!("limited to {MAX_REPORT_BYTES} UTF-8 bytes");
        assert_eq!(policy.matches(&message).count(), 1, "{message}");
    }

    #[test]
    fn freshness_bounds() {
        assert!(STATE_FRESHNESS >= crate::seat::FOLLOW_INTERVAL * 4);
        assert!(STATE_FRESHNESS <= LEASE_DURATION);
        assert!(LEASE_FRESHNESS < LEASE_DURATION);
    }
}
