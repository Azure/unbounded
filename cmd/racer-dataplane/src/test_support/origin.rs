//! Scripted adapter for changed ETags, credential rejection, and malformed bodies.
use super::clock::Clock;
use crate::{
    error::{Error, Operation, Result},
    model::{
        context::OriginContext,
        identity::{ObjectId, PageId, PageNumber, StrongEtag},
        metadata::MetadataSelector,
    },
    origin::{client::Origin, metadata::MetadataReply, page::OriginPage},
    read::candidates::OriginAuthority,
    runtime::deadline::{Deadline, RequestScope},
};
use std::{
    cell::RefCell,
    collections::VecDeque,
    future::{Future, poll_fn},
    task::Poll,
    time::Duration,
};

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Call {
    Metadata {
        object: ObjectId,
        pinned: Option<StrongEtag>,
    },
    Page(PageId),
}

pub enum Reply {
    Metadata(Result<MetadataReply>),
    Page(Result<OriginPage>),
}

pub struct Step {
    pub expected: Call,
    /// Absolute virtual time, allowing independent calls to finish out of order.
    pub ready_at: Duration,
    pub reply: Reply,
}

/// Scripts yield the real Origin result types, including deliberately inconsistent
/// metadata/buffers for downstream validation tests. They mint no fill authority or
/// verified pages. No request credentials are stored in scripts or observations.
pub struct ScriptedOrigin {
    clock: Clock,
    steps: RefCell<VecDeque<Step>>,
    calls: RefCell<Vec<Call>>,
}

impl Default for ScriptedOrigin {
    fn default() -> Self {
        Self::new(Clock::default())
    }
}

impl ScriptedOrigin {
    pub fn new(clock: Clock) -> Self {
        Self {
            clock,
            steps: RefCell::new(VecDeque::new()),
            calls: RefCell::new(Vec::new()),
        }
    }

    pub fn push(&self, step: Step) -> Result<()> {
        if !matches!(
            (&step.expected, &step.reply),
            (Call::Metadata { .. }, Reply::Metadata(_)) | (Call::Page(_), Reply::Page(_))
        ) {
            return Err(Error::InvalidRequest);
        }
        self.steps.borrow_mut().push_back(step);
        Ok(())
    }

    pub fn calls(&self) -> Vec<Call> {
        self.calls.borrow().clone()
    }

    pub fn remaining(&self) -> usize {
        self.steps.borrow().len()
    }

    fn take(&self, call: Call) -> Result<Step> {
        self.calls.borrow_mut().push(call.clone());
        let mut steps = self.steps.borrow_mut();
        let step = steps.front().ok_or(Error::Unavailable)?;
        if step.expected != call {
            return Err(Error::InvalidRequest);
        }
        Ok(steps.pop_front().unwrap())
    }

    async fn deliver(&self, step: Step) -> Result<Reply> {
        self.clock.wait_until(step.ready_at).await?;
        Ok(step.reply)
    }

    async fn deliver_scoped(&self, step: Step, scope: &RequestScope) -> Result<Reply> {
        self.deliver_checked(step, scope.deadline, || scope.check())
            .await
    }

    async fn deliver_checked(
        &self,
        step: Step,
        expires: Deadline,
        check: impl Fn() -> Result<()>,
    ) -> Result<Reply> {
        let until_deadline = expires.0.saturating_duration_since(self.clock.now());
        let deadline_at = self
            .clock
            .elapsed()
            .checked_add(until_deadline)
            .ok_or(Error::InvalidRange)?;
        let mut deadline = Box::pin(self.clock.wait_until(deadline_at));
        let mut delivery = Box::pin(self.deliver(step));
        poll_fn(|cx| {
            // Check on every poll, including an externally woken cancellation. The
            // virtual deadline has its own wakeup even if the reply is much later.
            check()?;
            self.clock.check_deadline(expires)?;
            if let Poll::Ready(result) = deadline.as_mut().poll(cx) {
                result?;
                return Poll::Ready(Err(Error::DeadlineExceeded));
            }
            delivery.as_mut().poll(cx)
        })
        .await
    }
}

impl Origin for ScriptedOrigin {
    fn metadata<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        selector: MetadataSelector,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        Box::pin(async move {
            scope.check()?;
            self.clock.check_deadline(scope.deadline)?;
            authority.validate(&context.object, PageNumber(0))?;
            let pinned = match selector {
                MetadataSelector::Fresh => None,
                MetadataSelector::Pinned(etag) => Some(etag),
            };
            let step = self.take(Call::Metadata {
                object: context.object.clone(),
                pinned,
            })?;
            let reply = self.deliver_scoped(step, scope).await?;
            scope.check()?;
            match reply {
                Reply::Metadata(result) => result,
                _ => Err(Error::InvalidRequest),
            }
        })
    }
    fn page<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        Box::pin(async move {
            scope.check()?;
            if context.object != page.version.object {
                return Err(Error::InvalidRequest);
            }
            self.clock.check_deadline(scope.deadline)?;
            authority.validate(&context.object, page.number)?;
            let step = self.take(Call::Page(page.clone()))?;
            let reply = self.deliver_scoped(step, scope).await?;
            scope.check()?;
            match reply {
                Reply::Page(result) => result,
                _ => Err(Error::InvalidRequest),
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        model::{
            identity::{CacheId, CacheKey, ObjectVersion},
            metadata::{ExpiresAt, ObjectMetadata},
        },
        test_support::poll_once,
    };
    use std::{task::Poll, time::UNIX_EPOCH};

    fn object() -> ObjectId {
        ObjectId {
            cache: CacheId("cache".into()),
            key: CacheKey([3; 32]),
        }
    }
    fn call() -> Call {
        Call::Metadata {
            object: object(),
            pinned: None,
        }
    }
    fn metadata(etag: &str) -> MetadataReply {
        MetadataReply {
            metadata: ObjectMetadata {
                version: ObjectVersion {
                    object: object(),
                    etag: StrongEtag::test_value(etag),
                },
                length: 0,
                expires_at: ExpiresAt(UNIX_EPOCH),
            },
            page_zero: None,
        }
    }

    #[test]
    fn credential_rejection_is_one_attempt_and_changed_etags_are_not_cached() {
        let origin = ScriptedOrigin::default();
        for reply in [
            Err(Error::Unauthorized),
            Ok(metadata("v1")),
            Ok(metadata("v2")),
        ] {
            origin
                .push(Step {
                    expected: call(),
                    ready_at: Duration::ZERO,
                    reply: Reply::Metadata(reply),
                })
                .unwrap();
        }
        for expected in [Err(Error::Unauthorized), Ok("v1"), Ok("v2")] {
            let step = origin.take(call()).unwrap();
            let mut future = Box::pin(origin.deliver(step));
            let Poll::Ready(Ok(Reply::Metadata(reply))) = poll_once(future.as_mut()) else {
                panic!("metadata reply expected")
            };
            assert_eq!(
                reply.map(|reply| reply.metadata.version.etag),
                expected.map(StrongEtag::test_value)
            );
        }
        assert_eq!(origin.remaining(), 0);
        assert!(matches!(origin.take(call()), Err(Error::Unavailable)));
        assert_eq!(origin.calls().len(), 4);
    }

    #[test]
    fn mismatch_does_not_consume_script_and_delays_allow_reordering() {
        let clock = Clock::default();
        let origin = ScriptedOrigin::new(clock.clone());
        for at in [2, 1] {
            origin
                .push(Step {
                    expected: call(),
                    ready_at: Duration::from_secs(at),
                    reply: Reply::Metadata(Ok(metadata("v1"))),
                })
                .unwrap();
        }
        let pinned = Call::Metadata {
            object: object(),
            pinned: Some(StrongEtag::test_value("v1")),
        };
        assert!(matches!(origin.take(pinned), Err(Error::InvalidRequest)));
        assert_eq!(origin.remaining(), 2);
        let mut first = Box::pin(origin.deliver(origin.take(call()).unwrap()));
        let mut second = Box::pin(origin.deliver(origin.take(call()).unwrap()));
        assert!(poll_once(first.as_mut()).is_pending());
        clock.advance(Duration::from_secs(1)).unwrap();
        assert!(poll_once(first.as_mut()).is_pending());
        assert!(poll_once(second.as_mut()).is_ready());
        clock.advance(Duration::from_secs(1)).unwrap();
        assert!(poll_once(first.as_mut()).is_ready());
        assert_eq!(
            origin.push(Step {
                expected: call(),
                ready_at: Duration::ZERO,
                reply: Reply::Page(Err(Error::Io))
            }),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn page_failures_remain_distinct_and_dropping_reply_does_not_replay_attempt() {
        let origin = ScriptedOrigin::default();
        let page = PageId {
            version: metadata("v1").metadata.version,
            number: PageNumber(0),
        };
        for error in [Error::VersionUnavailable, Error::Io, Error::CorruptRecord] {
            origin
                .push(Step {
                    expected: Call::Page(page.clone()),
                    ready_at: Duration::ZERO,
                    reply: Reply::Page(Err(error)),
                })
                .unwrap();
            let mut future =
                Box::pin(origin.deliver(origin.take(Call::Page(page.clone())).unwrap()));
            assert!(
                matches!(poll_once(future.as_mut()), Poll::Ready(Ok(Reply::Page(Err(actual)))) if actual == error)
            );
        }
        origin
            .push(Step {
                expected: call(),
                ready_at: Duration::MAX,
                reply: Reply::Metadata(Err(Error::Io)),
            })
            .unwrap();
        let mut future = Box::pin(origin.deliver(origin.take(call()).unwrap()));
        assert!(poll_once(future.as_mut()).is_pending());
        drop(future);
        assert_eq!(origin.remaining(), 0);
    }

    #[test]
    fn delayed_reply_wakes_at_original_deadline_and_checks_cancellation_on_repoll() {
        use crate::test_support::WakeCounter;
        use std::{
            cell::Cell,
            sync::Arc,
            task::{Context, Waker},
        };

        for cancel in [false, true] {
            let clock = Clock::default();
            let origin = ScriptedOrigin::new(clock.clone());
            let cancelled = Cell::new(false);
            let deadline = Deadline(clock.now() + Duration::from_secs(2));
            let step = Step {
                expected: call(),
                ready_at: Duration::from_secs(20),
                reply: Reply::Metadata(Ok(metadata("v1"))),
            };
            let mut future = Box::pin(origin.deliver_checked(step, deadline, || {
                if cancelled.get() {
                    Err(Error::Cancelled)
                } else {
                    Ok(())
                }
            }));
            let wakes = Arc::new(WakeCounter::default());
            let waker = Waker::from(wakes.clone());
            assert!(
                future
                    .as_mut()
                    .poll(&mut Context::from_waker(&waker))
                    .is_pending()
            );
            clock
                .jump_wall(UNIX_EPOCH - Duration::from_secs(100))
                .unwrap();
            if cancel {
                cancelled.set(true);
                assert!(matches!(
                    poll_once(future.as_mut()),
                    Poll::Ready(Err(Error::Cancelled))
                ));
            } else {
                clock.advance(Duration::from_secs(2)).unwrap();
                assert_eq!(wakes.count(), 1);
                assert!(matches!(
                    poll_once(future.as_mut()),
                    Poll::Ready(Err(Error::DeadlineExceeded))
                ));
            }
            drop(future);
            let count = wakes.count();
            clock.advance(Duration::from_secs(20)).unwrap();
            assert_eq!(
                wakes.count(),
                count,
                "completed attempt unregisters both timers"
            );
        }
    }
}
