//! A bounded, ordered record of what the simulated OS did.
//!
//! Every resource creation and close, submission, completion, consumed
//! fault, partition change and disk event appends one [`Event`]. Sequence
//! numbers increase by one per event and are never reset, so two runs of the
//! same seeded program can be compared with `assert_eq!` on their traces.
//!
//! The trace keeps at most [`TRACE_CAPACITY`] events; when it fills, the
//! oldest half is discarded.

use std::collections::VecDeque;

use uring_runtime::backend::OpKind;

use crate::disk::DiskState;
use crate::fault::{FaultTarget, SyncOp};

/// The maximum number of retained trace events.
pub const TRACE_CAPACITY: usize = 16384;

/// One recorded event.
#[derive(Clone, Debug, Eq, PartialEq, Hash)]
pub struct Event {
    /// Position in the world's event sequence, starting at 0.
    pub sequence: u64,
    /// What happened.
    pub kind: EventKind,
    /// The resource, inode or `user_data` the event concerns (see
    /// [`EventKind`]); 0 when not applicable.
    pub resource: u64,
    /// An event-specific result: a byte count, a negated errno, or 0.
    pub result: i64,
}

/// The kind of an [`Event`].
#[non_exhaustive]
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum EventKind {
    /// A resource was created; `resource` is its id.
    Create,
    /// A resource was closed; `resource` is its id.
    Close,
    /// An asynchronous operation was queued; `resource` is its `user_data`.
    Submit(OpKind),
    /// A push was rejected by
    /// [`World::reject_submissions`](crate::World::reject_submissions);
    /// `resource` is its `user_data`.
    Reject(OpKind),
    /// An asynchronous operation produced its result; `resource` is its
    /// `user_data` and `result` the CQE result.
    Complete(OpKind),
    /// A cancel request was handled; `resource` is the target `user_data`,
    /// `result` is 1 if the target was removed, else 0.
    Cancel,
    /// A fault was consumed at this site; `result` is the negated errno for
    /// [`Fault::Errno`](crate::Fault::Errno), otherwise the fault's count.
    Fault(FaultTarget),
    /// A synchronous call completed; `resource` is the resource id and
    /// `result` a byte count or 0.
    Call(SyncOp),
    /// A stream transfer or connect stalled on a partition.
    Blocked(SyncOp),
    /// A partition was added.
    Partition,
    /// A partition was removed.
    Heal,
    /// A stream was disconnected; `resource` is its id.
    Disconnect,
    /// A file (`resource` = inode) was made durable.
    SyncFile,
    /// A directory (`resource` = inode) and its entries were made durable.
    SyncDirectory,
    /// A disk crash; `resource` is the crash generation and `result` the
    /// number of lost inodes.
    Crash,
    /// Bytes were corrupted in the given image; `resource` is the inode.
    Corrupt(DiskState),
    /// A symbolic link was created.
    Symlink,
    /// A path was renamed.
    Rename,
    /// A path was removed.
    Unlink,
    /// A directory was created; `resource` is its inode.
    Mkdir,
}

#[derive(Debug, Default)]
pub(crate) struct Trace {
    events: VecDeque<Event>,
    next: u64,
}

impl Trace {
    pub(crate) fn record(&mut self, kind: EventKind, resource: u64, result: i64) {
        if self.events.len() == TRACE_CAPACITY {
            self.events.drain(..TRACE_CAPACITY / 2);
        }
        self.events.push_back(Event {
            sequence: self.next,
            kind,
            resource,
            result,
        });
        self.next += 1;
    }

    pub(crate) fn snapshot(&self) -> Vec<Event> {
        self.events.iter().cloned().collect()
    }

    pub(crate) fn take(&mut self) -> Vec<Event> {
        self.events.drain(..).collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn trace_is_bounded_and_sequences_continue() {
        let mut trace = Trace::default();
        for index in 0..20000 {
            trace.record(EventKind::Create, index, 0);
        }
        let events = trace.snapshot();
        assert!(events.len() <= TRACE_CAPACITY);
        assert_eq!(events.last().unwrap().sequence, 19999);
        assert!(
            events
                .windows(2)
                .all(|w| w[1].sequence == w[0].sequence + 1)
        );
        assert_eq!(trace.take().len(), events.len());
        trace.record(EventKind::Close, 0, 0);
        assert_eq!(trace.snapshot()[0].sequence, 20000);
    }
}
