//! Caller error conversion may inspect and close the mailbox that rejected work.

use std::{
    alloc::{GlobalAlloc, Layout, System},
    cell::{Cell, RefCell},
    sync::{Arc, mpsc},
    time::Duration,
};
use uring_runtime::{
    Error, Result, Scope,
    mailbox::{Command, Mailbox},
};

/// Scope with no allocation or policy failures before mailbox admission.
#[derive(Clone)]
struct TestScope;

/// Runtime failure and mailbox state observed during its conversion.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct ReentrantError {
    error: Error,

    queued: bool,

    outstanding: usize,
}

/// One-slot mailbox used by the conversion callback on the submitting thread.
type TestMailbox = Mailbox<(), (), (), TestScope>;

thread_local! {
    static CONVERSION_MAILBOX: RefCell<Option<Arc<TestMailbox>>> = const { RefCell::new(None) };
    static FAIL_QUEUE_ALLOCATION: Cell<bool> = const { Cell::new(false) };
}

impl Scope for TestScope {
    type Error = ReentrantError;

    /// Let submission reach the mailbox's own admission checks.
    fn check(&self) -> Result<(), Self::Error> {
        Ok(())
    }
}

impl From<Error> for ReentrantError {
    /// Reenter both a read and a write operation on the queue mutex.
    fn from(error: Error) -> Self {
        let mailbox = CONVERSION_MAILBOX.with(|slot| slot.borrow_mut().take().unwrap());
        let queued = mailbox.has_queued();
        mailbox.stop_admission();
        Self {
            error,
            queued,
            outstanding: mailbox.outstanding(),
        }
    }
}

/// Fail one queue-sized allocation on the test thread, not the test harness.
struct QueueAllocator;

/// Scope clones allocate only a small Arc; queue entries include the whole command.
fn fail_queue_allocation(layout: Layout) -> bool {
    layout.size() >= size_of::<(Arc<TestScope>, Command<(), (), (), TestScope>)>()
        && FAIL_QUEUE_ALLOCATION
            .try_with(|armed| armed.replace(false))
            .unwrap_or(false)
}

// SAFETY: successful allocations and all deallocations use the same system allocator.
unsafe impl GlobalAlloc for QueueAllocator {
    /// Reject the selected allocation or forward its unchanged layout.
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        if fail_queue_allocation(layout) {
            std::ptr::null_mut()
        } else {
            // SAFETY: the caller supplies a valid allocation layout.
            unsafe { System.alloc(layout) }
        }
    }

    /// Forward each live allocation with its original layout.
    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        // SAFETY: successful allocations came from System with this layout.
        unsafe { System.dealloc(ptr, layout) }
    }
}

#[global_allocator]
static ALLOCATOR: QueueAllocator = QueueAllocator;

/// Bound a regression deadlock without blocking the test harness until its timeout.
fn bounded(test: impl FnOnce() + Send + 'static) {
    let (done, result) = mpsc::channel();
    let worker = std::thread::spawn(move || {
        test();
        done.send(()).unwrap();
    });
    result
        .recv_timeout(Duration::from_secs(5))
        .expect("mailbox error conversion failed or deadlocked");
    worker.join().unwrap();
}

/// Uninstalled, closed, and full queues permit reentrant error conversion.
#[test]
fn rejection_conversion_can_reenter_mailbox() {
    bounded(|| {
        for (install, close, fill) in [
            (false, false, false),
            (true, true, false),
            (true, false, true),
        ] {
            let mailbox = Arc::new(TestMailbox::new(1).unwrap());
            if install {
                mailbox.install().unwrap();
            }
            let receipt = fill.then(|| mailbox.submit(1, (), &TestScope, None).unwrap());
            if close {
                mailbox.stop_admission();
            }
            CONVERSION_MAILBOX.with(|slot| *slot.borrow_mut() = Some(mailbox.clone()));
            assert_eq!(
                mailbox.submit(2, (), &TestScope, None).err(),
                Some(ReentrantError {
                    error: if fill {
                        Error::Overloaded
                    } else {
                        Error::Unavailable
                    },
                    queued: fill,
                    outstanding: usize::from(fill),
                })
            );
            assert_eq!(mailbox.install(), Err(Error::InvalidConfiguration));
            drop(mailbox.take_queued());
            drop(receipt);
            assert_eq!(mailbox.outstanding(), 0);
        }
    });
}

/// Failed queue growth converts outside the lock without reserving any credit.
#[test]
fn allocation_failure_conversion_can_reenter_mailbox() {
    bounded(|| {
        let mailbox = Arc::new(TestMailbox::new(1).unwrap());
        mailbox.install().unwrap();
        CONVERSION_MAILBOX.with(|slot| *slot.borrow_mut() = Some(mailbox.clone()));
        FAIL_QUEUE_ALLOCATION.with(|armed| armed.set(true));
        let result = mailbox.submit(1, (), &TestScope, None);
        assert!(!FAIL_QUEUE_ALLOCATION.with(Cell::get));
        assert_eq!(
            result.err(),
            Some(ReentrantError {
                error: Error::Overloaded,
                queued: false,
                outstanding: 0,
            })
        );
        assert!(!mailbox.has_queued());
        assert_eq!(mailbox.outstanding(), 0);
        assert_eq!(mailbox.install(), Err(Error::InvalidConfiguration));
        mailbox.uninstall().unwrap();
    });
}
