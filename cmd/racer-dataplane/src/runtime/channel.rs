//! Racer error adapters for the runtime's bounded SPSC ownership handoffs.
use crate::error::{Error, Result};
use std::task::{Context, Poll, Waker};
use uring_runtime::channel;

pub struct Sender<T>(channel::Sender<T>);
pub struct Receiver<T>(channel::Receiver<T>);
pub struct SendFailure<T> {
    pub command: T,
    pub error: Error,
}

pub fn bounded<T>(capacity: usize) -> Result<(Sender<T>, Receiver<T>)> {
    let (sender, receiver) = channel::bounded(capacity)?;
    Ok((Sender(sender), Receiver(receiver)))
}

impl<T> Sender<T> {
    pub fn discard_closed(&self) -> bool {
        self.0.discard_closed()
    }

    pub fn try_send(&self, command: T) -> std::result::Result<(), SendFailure<T>> {
        self.0.try_send(command).map_err(|failure| SendFailure {
            command: failure.command,
            error: failure.error.into(),
        })
    }

    pub fn close(&self) {
        self.0.close();
    }

    pub fn poll_ready(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        self.0
            .poll_ready(cx)
            .map(|result| result.map_err(Into::into))
    }
}

impl<T> Receiver<T> {
    pub fn is_closed(&self) -> bool {
        self.0.is_closed()
    }

    pub fn register(&self, waker: &Waker) {
        self.0.register(waker);
    }

    pub fn receive(&mut self) -> Result<Option<T>> {
        self.0.receive().map_err(Into::into)
    }

    pub fn poll_receive(&mut self, cx: &mut Context<'_>) -> Poll<Result<Option<T>>> {
        self.0
            .poll_receive(cx)
            .map(|result| result.map_err(Into::into))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn adapter_maps_errors_without_losing_command_ownership() {
        assert!(matches!(bounded::<u8>(0), Err(Error::InvalidConfiguration)));
        let (sender, mut receiver) = bounded(1).unwrap();
        assert!(sender.try_send(String::from("first")).is_ok());
        let failure = sender.try_send(String::from("second")).err().unwrap();
        assert_eq!(failure.error, Error::Overloaded);
        assert_eq!(failure.command, "second");
        assert_eq!(receiver.receive().unwrap().as_deref(), Some("first"));
        drop(receiver);
        let failure = sender.try_send(failure.command).err().unwrap();
        assert_eq!(failure.error, Error::Unavailable);
        assert_eq!(failure.command, "second");
        assert_eq!(
            sender.poll_ready(&mut Context::from_waker(futures::task::noop_waker_ref())),
            Poll::Ready(Err(Error::Unavailable))
        );
        assert!(sender.discard_closed());
    }
}
