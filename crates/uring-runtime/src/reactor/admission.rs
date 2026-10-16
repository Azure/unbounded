//! Admission control: the ordinary partition, reservations and rejection
//! reporting.

use std::any::Any;
use std::cell::Cell;
use std::fmt;
use std::rc::Rc;

use crate::Error;
use crate::backend::OpKind;

/// Dedicated submission capacity, created by
/// [`Reactor::reserve`](super::Reactor::reserve).
///
/// Operations submitted with [`Operation::reserved`](super::Operation::reserved)
/// count against the reservation instead of the ordinary
/// [`max_in_flight`](super::ReactorConfig::max_in_flight) partition, so
/// critical work (health checks, control traffic, shutdown I/O) can still be
/// admitted while ordinary traffic has filled the reactor.
///
/// A reservation is a cheap, shared handle: clones share the same slots. Its
/// capacity returns to the reactor's
/// [`max_reserved`](super::ReactorConfig::max_reserved) pool when the last
/// clone is dropped and every operation submitted under it has been fenced.
#[derive(Clone)]
pub struct Reservation(pub(super) Rc<ReservationShared>);

pub(super) struct ReservationShared {
    pub(super) slots: usize,
    pub(super) used: Cell<usize>,
    /// The owning reactor's free reserved capacity.
    pub(super) pool: Rc<Cell<usize>>,
}

impl Drop for ReservationShared {
    fn drop(&mut self) {
        self.pool.set(self.pool.get() + self.slots);
    }
}

impl Reservation {
    /// The number of operations this reservation admits at once.
    pub fn slots(&self) -> usize {
        self.0.slots
    }

    /// The number of slots currently held by unfenced operations.
    pub fn in_use(&self) -> usize {
        self.0.used.get()
    }

    /// The number of slots available for new operations.
    pub fn available(&self) -> usize {
        self.0.slots - self.0.used.get()
    }
}

impl fmt::Debug for Reservation {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Reservation")
            .field("slots", &self.slots())
            .field("in_use", &self.in_use())
            .finish()
    }
}

/// The capacity an admitted operation holds until its fence.
pub(super) enum Partition {
    Ordinary,
    Reserved(Rc<ReservationShared>),
    /// Capacity was returned at the fence.
    Released,
}

/// Why an operation was not submitted.
#[non_exhaustive]
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum RejectionReason {
    /// The ordinary partition
    /// ([`max_in_flight`](super::ReactorConfig::max_in_flight)) is full.
    /// The error kind is [`Overloaded`](crate::ErrorKind::Overloaded).
    Capacity,
    /// Every slot of the operation's [`Reservation`] is in use.
    /// The error kind is [`Overloaded`](crate::ErrorKind::Overloaded).
    ReservationExhausted,
    /// The [`Reservation`] belongs to a different reactor.
    /// The error kind is [`InvalidInput`](crate::ErrorKind::InvalidInput).
    ForeignReservation,
    /// The reactor is draining or shutting down.
    /// The error kind is [`Unavailable`](crate::ErrorKind::Unavailable).
    Closed,
    /// An earlier backend failure stopped the reactor.
    /// The error kind is [`Unavailable`](crate::ErrorKind::Unavailable).
    Failed,
    /// The backend's submission queue stayed full after a submit.
    /// The error kind is [`Overloaded`](crate::ErrorKind::Overloaded).
    QueueFull,
    /// The backend does not support the operation.
    /// The error kind is [`Unsupported`](crate::ErrorKind::Unsupported).
    Unsupported,
    /// The backend refused the operation; see [`Rejection::error`].
    Backend,
}

impl RejectionReason {
    /// A short, lowercase description.
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Capacity => "in-flight capacity exhausted",
            Self::ReservationExhausted => "reservation exhausted",
            Self::ForeignReservation => "reservation belongs to another reactor",
            Self::Closed => "reactor closed",
            Self::Failed => "reactor failed",
            Self::QueueFull => "submission queue full",
            Self::Unsupported => "operation unsupported",
            Self::Backend => "backend rejected operation",
        }
    }
}

impl fmt::Display for RejectionReason {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// A rejected submission, as reported to a
/// [rejection observer](super::Reactor::set_rejection_observer).
///
/// Scope failures before submission are not rejections: they are the
/// caller's own cancellation and are returned without notifying the
/// observer.
#[non_exhaustive]
#[derive(Debug)]
pub struct Rejection<'a> {
    /// Why the operation was rejected.
    pub reason: RejectionReason,
    /// The operation that was rejected.
    pub kind: OpKind,
    /// Whether the operation asked for reserved capacity.
    pub reserved: bool,
    /// The operation's scope. Downcast it to your [`Scope`](crate::Scope)
    /// type to attribute the rejection to a request.
    pub scope: &'a dyn Any,
    /// The error returned to the caller (before conversion into the
    /// scope's error type).
    pub error: &'a Error,
}
