//! Ciphertext-only movement. Control exchange supplies signed per-transfer grants;
//! failed attempts are fenced before the peer owner starts a new HTTP attempt.
use super::{
    permission::{AuthenticatedDescriptor, Grant, completion_bytes},
    registered::RegisteredLease,
    session::{SessionLease, Sessions},
    verbs::QueuePairHandle,
};
use crate::{
    error::{Error, Operation, Result},
    memory::pool::{BufferPool, CiphertextPage},
    model::{PageEnvelope, ResourceClass, TransferId},
    runtime::{admission::Admission, deadline::RequestScope},
    security::signing::VerifiedHead,
    topology::rails::RailId,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use std::{future::poll_fn, rc::Rc, task::Poll};

pub struct RdmaTransfer {
    sessions: Rc<Sessions>,
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
    pub fn new(sessions: Rc<Sessions>) -> Self {
        Self { sessions }
    }
    pub fn ready(&self, rail: RailId) -> bool {
        self.sessions.ready(rail)
    }
    pub fn progress(&self) -> Result<usize> {
        self.sessions.progress()
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
            let mut buffer = RegisteredLease::acquire(session, page.bytes().len(), scope).await?;
            buffer.copy_from(page.bytes(), scope).await?;
            let ticket = super::verbs::wait(scope, |cx| {
                session.qp.register_waiter(cx);
                session.qp.poll_write(
                    buffer.region.clone(),
                    descriptor.descriptor.address,
                    descriptor.descriptor.scoped_key,
                )
            })
            .await?;
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
            // Request termination abandons this wait, not the native owner's
            // quarantine. Only a successful terminal fence permits completion.
            super::verbs::wait(scope, |cx| session.qp.poll_stopped(cx)).await?;
            Ok(SendCompletion {
                binding: session.binding(),
                transfer: descriptor.descriptor.transfer,
            })
        })
    }
    pub fn prepare_receive<'a>(
        &'a self,
        session: &'a SessionLease,
        envelope: &'a PageEnvelope,
        transfer: TransferId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Grant> {
        Box::pin(async move {
            scope.check()?;
            validate_envelope(envelope)?;
            let buffer =
                RegisteredLease::acquire(session, envelope.ciphertext_length as usize, scope)
                    .await?;
            Grant::bind(session, buffer, transfer, scope).await
        })
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
            let bytes = buffer.to_vec(scope).await?;
            BufferPool::new(admission.clone()).ciphertext(reservation, envelope, bytes)
        })
    }
    pub fn drain(&self) -> Operation<'_, ()> {
        self.sessions.drain()
    }
    pub fn fence_cut(&self) -> Operation<'static, ()> {
        self.sessions.fence_cut()
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
