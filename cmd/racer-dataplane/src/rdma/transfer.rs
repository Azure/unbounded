//! Ciphertext-only movement. Control exchange supplies signed per-transfer grants;
//! failed attempts are fenced before the peer owner starts a new HTTP attempt.
use super::{
    permission::{AuthenticatedDescriptor, Grant, Permissions, completion_bytes},
    registered::{RegisteredLease, RegisteredPool},
    session::{SessionLease, Sessions},
    verbs::QueuePairHandle,
};
use crate::{
    error::{Error, Operation, Result},
    memory::pool::{BufferPool, CiphertextPage},
    model::{envelope::PageEnvelope, identity::TransferId, limits::ResourceClass},
    runtime::{admission::Admission, deadline::RequestScope},
    security::signing::VerifiedHead,
    topology::rails::RailId,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use std::{future::poll_fn, rc::Rc, task::Poll};

pub struct RdmaTransfer {
    sessions: Rc<Sessions>,
    buffers: Rc<RegisteredPool>,
    permissions: Rc<Permissions>,
}
/// Produced only by a successful native write CQE. Sign this header as part of
/// the request-bound HTTP control response; the ciphertext is not hashed here.
pub struct SendCompletion {
    binding: [u8; 32],
    transfer: TransferId,
}
impl SendCompletion {
    pub fn header_value(&self) -> Vec<u8> {
        STANDARD
            .encode(completion_bytes(self.binding, self.transfer))
            .into_bytes()
    }
}
struct AbortOnDrop(Rc<QueuePairHandle>);
impl Drop for AbortOnDrop {
    fn drop(&mut self) {
        let _ = self.0.stop();
    }
}

impl RdmaTransfer {
    pub fn register_driver(&self, waker: &std::task::Waker) {
        self.sessions.register_driver(waker);
    }
    pub fn new(
        sessions: Rc<Sessions>,
        buffers: Rc<RegisteredPool>,
        permissions: Rc<Permissions>,
    ) -> Self {
        Self {
            sessions,
            buffers,
            permissions,
        }
    }
    pub fn ready(&self, rail: RailId) -> bool {
        self.sessions.ready(rail)
    }
    pub fn progress(&self) -> Result<usize> {
        self.sessions.progress()
    }
    /// Compatibility surface: no descriptor means no authority to write. The
    /// caller must choose HTTP or use send_to with a verified scoped capability.
    pub fn send<'a>(
        &'a self,
        _session: &'a SessionLease,
        _page: CiphertextPage,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            Err(Error::Unavailable)
        })
    }
    pub fn send_to<'a>(
        &'a self,
        session: &'a SessionLease,
        page: CiphertextPage,
        descriptor: AuthenticatedDescriptor,
        scope: &'a RequestScope,
    ) -> Operation<'a, SendCompletion> {
        Box::pin(async move {
            scope.check()?;
            descriptor.validate(session, page.bytes().len())?;
            validate_envelope(page.envelope())?;
            if page.bytes().len() != page.envelope().ciphertext_length as usize {
                return Err(Error::InvalidRange);
            }
            session.wait_ready(scope).await?;
            session.claim()?;
            session.qp.expire_at(scope.deadline.0);
            let _abort = AbortOnDrop(session.qp.clone());
            let mut buffer = self.buffers.acquire_for(session, page.bytes().len())?;
            buffer.copy_from(page.bytes())?;
            let ticket = session.qp.write(
                buffer.region.clone(),
                descriptor.descriptor.address,
                descriptor.descriptor.scoped_key,
            )?;
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(|cx| {
                cancellation.register(cx.waker());
                if let Err(error) = scope.check() {
                    return Poll::Ready(Err(error));
                }
                if let Err(error) = session.progress() {
                    return Poll::Ready(Err(error));
                }
                ticket.poll(cx)
            })
            .await?;
            // Source buffer is safe after its write CQE. Stop this single-use QP
            // before returning a control completion or admitting a fallback.
            futures::future::poll_fn(|cx| session.qp.poll_stopped(cx)).await?;
            Ok(SendCompletion {
                binding: session.binding(),
                transfer: descriptor.descriptor.transfer,
            })
        })
    }
    pub fn prepare_receive(
        &self,
        session: &SessionLease,
        envelope: &PageEnvelope,
        transfer: TransferId,
        scope: &RequestScope,
    ) -> Result<Grant> {
        scope.check()?;
        validate_envelope(envelope)?;
        let buffer = self
            .buffers
            .acquire_for(session, envelope.ciphertext_length as usize)?;
        self.permissions
            .grant(session, buffer, transfer, scope.deadline)
    }
    /// This handoff requires a signed completion and returns ciphertext only.
    pub fn finish_receive<'a>(
        &'a self,
        session: &'a SessionLease,
        grant: Grant,
        head: &'a VerifiedHead,
        envelope: PageEnvelope,
        admission: &'a Rc<Admission>,
        scope: &'a RequestScope,
    ) -> Operation<'a, CiphertextPage> {
        Box::pin(async move {
            validate_envelope(&envelope)?;
            let reservation = admission.reserve(
                Some(&envelope.page.version.object.cache),
                ResourceClass::Ciphertext,
                envelope.ciphertext_length as usize,
            )?;
            let buffer: RegisteredLease = grant.finish(head, session, scope).await?;
            if buffer.len() != envelope.ciphertext_length as usize {
                return Err(Error::InvalidRange);
            }
            let bytes = buffer.to_vec()?;
            BufferPool::new(admission.clone()).ciphertext(reservation, envelope, bytes)
        })
    }
    pub fn drain(&self) -> Operation<'_, ()> {
        self.sessions.drain()
    }
    pub fn fence_cut(&self) -> Operation<'static, ()> {
        self.sessions.fence_cut()
    }
    pub fn receive<'a>(
        &'a self,
        _session: &'a SessionLease,
        _envelope: PageEnvelope,
        scope: &'a RequestScope,
    ) -> Operation<'a, CiphertextPage> {
        Box::pin(async move {
            scope.check()?;
            Err(Error::Unavailable)
        })
    }
}
fn validate_envelope(envelope: &PageEnvelope) -> Result<()> {
    if envelope.plaintext_length == 0
        || envelope.plaintext_length > 16 * 1024 * 1024
        || envelope.ciphertext_length != envelope.plaintext_length + 16
    {
        return Err(Error::InvalidRange);
    }
    Ok(())
}
#[cfg(test)]
mod tests {
    // Native completion ordering and cancellation are tested at the ownership boundary.
}
