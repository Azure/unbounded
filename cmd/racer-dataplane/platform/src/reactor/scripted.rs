//! A scripted backend for adapter tests: every pushed operation is answered
//! by a test-provided responder when submitted, or stays pending.

use super::descriptor::Descriptor;
use std::{cell::RefCell, collections::VecDeque, rc::Rc, time::Duration};
use uring::backend::{
    BackendConfig, CompletionEntry, Op, OpKind, PushError, PushHints, RemoteWaker,
};

/// Buffer pointer and length of a data operation.
pub(crate) type Region = Option<(*mut u8, u32)>;

/// Decides the result of a submitted operation; `None` keeps it pending.
pub(crate) type Responder = Box<dyn FnMut(OpKind, Region) -> Option<i32>>;

pub(crate) struct State {
    queued: Vec<(u64, OpKind, Region)>,
    pending: Vec<(u64, OpKind)>,
    cancels: Vec<(u64, u64)>,
    completions: VecDeque<CompletionEntry>,
    responder: Responder,
    /// Every push, in order.
    pub(crate) pushes: Vec<(OpKind, PushHints)>,
    /// Make the next pushes fail with `Full`.
    pub(crate) full: usize,
}

/// The backend half; tests keep the shared state.
pub struct Scripted(pub(crate) Rc<RefCell<State>>);

impl Scripted {
    pub(crate) fn new(responder: impl FnMut(OpKind, Region) -> Option<i32> + 'static) -> Self {
        Self(Rc::new(RefCell::new(State {
            queued: Vec::new(),
            pending: Vec::new(),
            cancels: Vec::new(),
            completions: VecDeque::new(),
            responder: Box::new(responder),
            pushes: Vec::new(),
            full: 0,
        })))
    }

    pub(crate) fn state(&self) -> Rc<RefCell<State>> {
        self.0.clone()
    }

    pub(crate) fn init(&mut self, _: &BackendConfig) -> uring::Result<()> {
        Ok(())
    }

    pub(crate) unsafe fn push_with(
        &mut self,
        user_data: u64,
        op: &Op<'_, Rc<Descriptor>>,
        hints: PushHints,
    ) -> Result<(), PushError> {
        let mut state = self.0.borrow_mut();
        state.pushes.push((op.kind(), hints));
        if state.full > 0 {
            state.full -= 1;
            return Err(PushError::Full);
        }
        let region = match *op {
            Op::Read { buf, len, .. } | Op::Recv { buf, len, .. } => Some((buf, len)),
            Op::Write { buf, len, .. } | Op::Send { buf, len, .. } => Some((buf.cast_mut(), len)),
            _ => None,
        };
        state.queued.push((user_data, op.kind(), region));
        Ok(())
    }

    pub(crate) fn cancel(&mut self, user_data: u64, target: u64) -> Result<(), PushError> {
        self.0.borrow_mut().cancels.push((user_data, target));
        Ok(())
    }

    pub(crate) unsafe fn adopt_fd(&mut self, _raw: i32) -> Rc<Descriptor> {
        Rc::new(Descriptor::from(
            std::fs::File::open("/dev/null").expect("open /dev/null"),
        ))
    }

    pub(crate) fn submit(&mut self) -> uring::Result<usize> {
        let mut state = self.0.borrow_mut();
        let queued = std::mem::take(&mut state.queued);
        let count = queued.len();
        for (user_data, kind, region) in queued {
            match (state.responder)(kind, region) {
                Some(result) => state.completions.push_back(CompletionEntry {
                    user_data,
                    result,
                    flags: 0,
                }),
                None => state.pending.push((user_data, kind)),
            }
        }
        for (user_data, target) in std::mem::take(&mut state.cancels) {
            let found = state.pending.iter().position(|(key, _)| *key == target);
            state.completions.push_back(CompletionEntry {
                user_data,
                result: if found.is_some() { 0 } else { -libc::ENOENT },
                flags: 0,
            });
            if let Some(index) = found {
                state.pending.remove(index);
                state.completions.push_back(CompletionEntry {
                    user_data: target,
                    result: -libc::ECANCELED,
                    flags: 0,
                });
            }
        }
        Ok(count)
    }

    pub(crate) fn pop(&mut self) -> Option<CompletionEntry> {
        self.0.borrow_mut().completions.pop_front()
    }

    pub(crate) fn wait(&mut self, _: Duration) -> uring::Result<()> {
        Ok(())
    }

    pub(crate) fn remote_waker(&self) -> uring::Result<RemoteWaker> {
        Ok(RemoteWaker::new(|| {}))
    }

    pub(crate) fn supports(&self, _: OpKind) -> bool {
        true
    }
}

impl State {
    /// Kinds of pending (submitted, unanswered) operations.
    pub(crate) fn pending_kinds(&self) -> Vec<OpKind> {
        self.pending.iter().map(|(_, kind)| *kind).collect()
    }

    /// Complete every pending operation of `kind` with `result`.
    pub(crate) fn complete(&mut self, kind: OpKind, result: i32) {
        let (done, keep): (Vec<_>, Vec<_>) = self.pending.drain(..).partition(|(_, k)| *k == kind);
        self.pending = keep;
        for (user_data, _) in done {
            self.completions.push_back(CompletionEntry {
                user_data,
                result,
                flags: 0,
            });
        }
    }
}
