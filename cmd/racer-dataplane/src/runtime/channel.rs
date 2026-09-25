//! Bounded cache-line-separated handoffs and explicit wakeup ownership.

use crate::error::{Error, Result, pending};
use std::marker::PhantomData;

#[repr(align(64))]
pub struct Sender<T> {
    marker: PhantomData<T>,
}
#[repr(align(64))]
pub struct Receiver<T> {
    marker: PhantomData<T>,
}

/// Returns the command on saturation; never loses a resource-bearing message.
pub struct SendFailure<T> {
    pub command: T,
    pub error: Error,
}

pub fn bounded<T>(_capacity: usize) -> Result<(Sender<T>, Receiver<T>)> {
    pending("channel.bounded")
}

impl<T> Sender<T> {
    pub fn try_send(&self, command: T) -> std::result::Result<(), SendFailure<T>> {
        Err(SendFailure {
            command,
            error: Error::Unimplemented("channel.try_send"),
        })
    }
}
impl<T> Receiver<T> {
    pub fn receive(&mut self) -> Result<Option<T>> {
        pending("channel.receive")
    }
}

#[cfg(test)]
mod tests {
    // Cover lost wakeups, queue saturation, fairness, and command ownership on error.
}
