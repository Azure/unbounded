//! Type-2B windows expose exactly one transfer buffer. Bind CQE precedes export;
//! invalidation plus terminal QP destruction precedes any CPU access or reuse.
use super::{
    registered::{MAX_CIPHERTEXT, RegisteredLease},
    session::{SessionLease, signed_value},
    verbs::{QueuePairHandle, Ticket, Window},
};
use crate::{
    error::{Error, Operation, Result},
    model::identity::TransferId,
    runtime::deadline::{Deadline, RequestScope},
    security::signing::VerifiedHead,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use std::{future::poll_fn, rc::Rc, task::Poll, time::Instant};

pub const DESCRIPTOR_HEADER: &str = "racer-rdma-descriptor";
pub const COMPLETION_HEADER: &str = "racer-rdma-completion";
pub struct Permissions;
pub struct Grant {
    transfer: TransferId,
    buffer: Option<RegisteredLease>,
    qp: Rc<QueuePairHandle>,
    window: Rc<Window>,
    bound: Ticket,
    deadline: Deadline,
    binding: [u8; 32],
}
pub struct RemoteDescriptor {
    pub transfer: TransferId,
    pub address: u64,
    pub length: u64,
    pub scoped_key: u32,
}
/// Only a verified, explicitly signed header can create a send capability.
pub struct AuthenticatedDescriptor {
    pub(crate) descriptor: RemoteDescriptor,
    binding: [u8; 32],
}
impl RemoteDescriptor {
    fn encode(&self, binding: [u8; 32]) -> Vec<u8> {
        let mut bytes = b"racer-rdma-grant-v1\0".to_vec();
        bytes.extend_from_slice(&binding);
        bytes.extend_from_slice(&self.transfer.0);
        bytes.extend_from_slice(&self.address.to_be_bytes());
        bytes.extend_from_slice(&self.length.to_be_bytes());
        bytes.extend_from_slice(&self.scoped_key.to_be_bytes());
        bytes
    }
    fn decode(bytes: &[u8], binding: [u8; 32]) -> Result<Self> {
        let prefix = b"racer-rdma-grant-v1\0";
        if bytes.len() != prefix.len() + 68 || !bytes.starts_with(prefix) {
            return Err(Error::InvalidRequest);
        }
        let b = &bytes[prefix.len()..];
        if b[..32] != binding {
            return Err(Error::Unauthorized);
        }
        let descriptor = Self {
            transfer: TransferId(b[32..48].try_into().unwrap()),
            address: u64::from_be_bytes(b[48..56].try_into().unwrap()),
            length: u64::from_be_bytes(b[56..64].try_into().unwrap()),
            scoped_key: u32::from_be_bytes(b[64..68].try_into().unwrap()),
        };
        if descriptor.length == 0
            || descriptor.length > MAX_CIPHERTEXT as u64
            || descriptor.address == 0
            || descriptor.address.checked_add(descriptor.length).is_none()
        {
            return Err(Error::InvalidRange);
        }
        Ok(descriptor)
    }
}
impl AuthenticatedDescriptor {
    pub fn from_verified(
        head: &VerifiedHead,
        session: &SessionLease,
        transfer: TransferId,
    ) -> Result<Self> {
        if head.peer.node() != session.peer() {
            return Err(Error::Unauthorized);
        }
        let bytes = signed_value(head, DESCRIPTOR_HEADER, 128)?;
        let descriptor = RemoteDescriptor::decode(&bytes, session.binding())?;
        if descriptor.transfer != transfer {
            return Err(Error::Unauthorized);
        }
        Ok(Self {
            descriptor,
            binding: session.binding(),
        })
    }
    pub(crate) fn validate(&self, session: &SessionLease, length: usize) -> Result<()> {
        if self.binding != session.binding() || self.descriptor.length != length as u64 {
            return Err(Error::Unauthorized);
        }
        Ok(())
    }
}
impl Permissions {
    pub fn grant<'a>(
        &'a self,
        session: &'a SessionLease,
        buffer: RegisteredLease,
        transfer: TransferId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Grant> {
        Box::pin(async move {
            scope.check()?;
            let deadline = scope.deadline;
            if buffer.rail != session.rail() {
                return Err(Error::InvalidRequest);
            }
            session.claim()?;
            session.qp.expire_at(deadline.0);
            struct Abort<'a>(Option<&'a QueuePairHandle>);
            impl Drop for Abort<'_> {
                fn drop(&mut self) {
                    if let Some(qp) = self.0 {
                        let _ = qp.stop();
                    }
                }
            }
            let mut abort = Abort(Some(&session.qp));
            let (window, bound) = super::verbs::wait(scope, |cx| {
                session.qp.register_waiter(cx);
                session.qp.poll_bind(buffer.region.clone())
            })
            .await?;
            // The returned Grant takes over abort-on-drop ownership.
            abort.0 = None;
            Ok(Grant {
                transfer,
                buffer: Some(buffer),
                qp: session.qp.clone(),
                window,
                bound,
                deadline,
                binding: session.binding(),
            })
        })
    }
    pub fn revoke_and_fence(&self, mut grant: Grant) -> Operation<'_, RegisteredLease> {
        Box::pin(async move {
            // Terminal destruction is deliberately used even after a successful
            // local invalidation. It fences writes already admitted by the RNIC.
            // A single-use QP prevents that fence from canceling unrelated pages.
            futures::future::poll_fn(|cx| grant.qp.poll_stopped(cx)).await?;
            grant.buffer.take().ok_or(Error::InvalidRequest)
        })
    }
}
impl Grant {
    pub fn transfer(&self) -> TransferId {
        self.transfer
    }
    pub fn descriptor(&self) -> Result<RemoteDescriptor> {
        if Instant::now() >= self.deadline.0 {
            return Err(Error::DeadlineExceeded);
        }
        if !self.qp.ready() {
            return Err(Error::Unavailable);
        }
        self.bound.result().ok_or(Error::Unavailable)??;
        let buffer = self.buffer.as_ref().ok_or(Error::InvalidRequest)?;
        Ok(RemoteDescriptor {
            transfer: self.transfer,
            address: self.window.address.get(),
            length: buffer.len() as u64,
            scoped_key: self.window.key.get(),
        })
    }
    pub fn header_value(&self) -> Result<Vec<u8>> {
        Ok(STANDARD
            .encode(self.descriptor()?.encode(self.binding))
            .into_bytes())
    }
    pub fn wait_bound<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(move |cx| {
                cancellation.register(cx.waker());
                if let Err(error) = scope.check() {
                    let _ = self.qp.stop();
                    return Poll::Ready(Err(error));
                }
                if Instant::now() >= self.deadline.0 {
                    let _ = self.qp.stop();
                    return Poll::Ready(Err(Error::DeadlineExceeded));
                }
                if let Err(error) = self.qp.progress() {
                    return Poll::Ready(Err(error));
                }
                self.bound.poll(cx)
            })
            .await
        })
    }
    /// Receiver accepts completion only after the sender's successful write CQE
    /// has been attested in the signed control exchange. AEAD still authenticates
    /// the bytes later; this message alone never makes plaintext publishable.
    pub fn finish<'a>(
        mut self,
        head: &'a VerifiedHead,
        session: &'a SessionLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, RegisteredLease> {
        Box::pin(async move {
            scope.check()?;
            if head.peer.node() != session.peer() || session.binding() != self.binding {
                return Err(Error::Unauthorized);
            }
            let bytes = signed_value(head, COMPLETION_HEADER, 128)?;
            if bytes != completion_bytes(self.binding, self.transfer) {
                return Err(Error::Unauthorized);
            }
            self.bound.result().ok_or(Error::Unavailable)??;
            let mut invalidated = None;
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(|cx| {
                cancellation.register(cx.waker());
                if let Err(error) = scope.check() {
                    return Poll::Ready(Err(error));
                }
                if Instant::now() >= self.deadline.0 {
                    return Poll::Ready(Err(Error::DeadlineExceeded));
                }
                if let Err(error) = self.qp.progress() {
                    return Poll::Ready(Err(error));
                }
                if invalidated.is_none() {
                    match self.qp.poll_invalidate(self.window.clone(), cx) {
                        Poll::Pending => return Poll::Pending,
                        Poll::Ready(Err(error)) => return Poll::Ready(Err(error)),
                        Poll::Ready(Ok(ticket)) => invalidated = Some(ticket),
                    }
                }
                invalidated.as_ref().unwrap().poll(cx)
            })
            .await?;
            futures::future::poll_fn(|cx| self.qp.poll_stopped(cx)).await?;
            self.buffer.take().ok_or(Error::InvalidRequest)
        })
    }
}
impl Drop for Grant {
    fn drop(&mut self) {
        // Dropping a future/grant is an abort, never implicit completion. On a
        // failed fence QP-owned window/region references preserve quarantine.
        let _ = self.qp.stop();
    }
}
pub(crate) fn completion_bytes(binding: [u8; 32], transfer: TransferId) -> Vec<u8> {
    let mut bytes = b"racer-rdma-complete-v1\0".to_vec();
    bytes.extend_from_slice(&binding);
    bytes.extend_from_slice(&transfer.0);
    bytes
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn descriptors_reject_overflow_wrong_session_and_trailing_bytes() {
        let d = RemoteDescriptor {
            transfer: TransferId([7; 16]),
            address: 4096,
            length: 17,
            scoped_key: 9,
        };
        let bytes = d.encode([2; 32]);
        assert_eq!(
            RemoteDescriptor::decode(&bytes, [2; 32]).unwrap().length,
            17
        );
        assert!(matches!(
            RemoteDescriptor::decode(&bytes, [3; 32]),
            Err(Error::Unauthorized)
        ));
        let mut extra = bytes;
        extra.push(0);
        assert!(RemoteDescriptor::decode(&extra, [2; 32]).is_err());
        let d = RemoteDescriptor {
            address: u64::MAX,
            ..d
        };
        assert!(matches!(
            RemoteDescriptor::decode(&d.encode([2; 32]), [2; 32]),
            Err(Error::InvalidRange)
        ));
    }
}
