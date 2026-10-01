//! Fixed, typed syscall backing whose raw pointers survive owner moves.

/// A private one-element Vec provides T's layout and alignment without Box's
/// move retagging. Pointer accessors do not materialize pointee references and
/// there is no resize API. The reactor must retain this owner without consuming
/// it until all applicable CQEs are fenced.
pub(super) struct SyscallArg<T>(Vec<T>);

impl<T> SyscallArg<T> {
    pub(super) fn new(value: T) -> Self {
        Self(vec![value])
    }

    pub(super) fn as_ptr(&self) -> *const T {
        self.0.as_ptr()
    }

    pub(super) fn as_mut_ptr(&mut self) -> *mut T {
        self.0.as_mut_ptr()
    }

    pub(super) fn into_inner(mut self) -> T {
        self.0.pop().expect("one syscall argument")
    }
}
