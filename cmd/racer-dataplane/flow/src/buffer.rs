//! Fixed backing paired with a live quota charge and recycled on drop.
use crate::{Charge, Error, Policy, Result};

pub struct ChargedBuffer<P: Policy> {
    bytes: Vec<u8>,
    charge: Option<Charge<P>>,
}
impl<P: Policy> ChargedBuffer<P> {
    /// Allocate admitted, initialized backing. Class, key, and provenance policy
    /// must be checked by the caller before passing the charge.
    pub fn new(mut charge: Charge<P>, length: usize) -> Result<Self> {
        if length == 0 {
            return Err(Error::InvalidInput);
        }
        let bytes = charge.buffer(length)?;
        // Do not box or resize again while I/O pointers are live.
        let bytes = bytes.into_boxed_slice().into_vec();
        charge.shrink(bytes.len())?;
        Ok(Self {
            bytes,
            charge: Some(charge),
        })
    }
    pub fn charge(&self) -> &Charge<P> {
        self.charge.as_ref().expect("owned charge")
    }
    pub fn bytes(&self) -> &[u8] {
        &self.bytes
    }
    pub fn bytes_mut(&mut self) -> &mut [u8] {
        &mut self.bytes
    }
    pub fn into_parts(mut self) -> (Box<[u8]>, Charge<P>) {
        (
            std::mem::take(&mut self.bytes).into_boxed_slice(),
            self.charge.take().expect("owned charge"),
        )
    }
}
impl<P: Policy> Drop for ChargedBuffer<P> {
    fn drop(&mut self) {
        if let Some(charge) = &mut self.charge {
            charge.recycle(std::mem::take(&mut self.bytes));
        }
    }
}
// SAFETY: private fixed backing and charge remain exclusively owned.
unsafe impl<P: Policy> uring_runtime::reactor::IoBuffer for ChargedBuffer<P> {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.bytes)
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.bytes)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[derive(Clone, Copy)]
    struct Class;
    impl crate::Class for Class {
        const COUNT: usize = 1;
        fn index(self) -> usize {
            0
        }
    }
    struct TestPolicy;
    impl Policy for TestPolicy {
        type Class = Class;
        type Key = ();
        fn limit(&self, _: Class) -> usize {
            4 * 1024 * 1024
        }
        fn max_keys(&self) -> usize {
            1
        }
        fn wakes(_: Class) -> bool {
            false
        }
        fn covers(_: Class) -> bool {
            false
        }
        fn rejected(&self, _: crate::Rejection<Class>) {}
    }
    #[test]
    fn fixed_backing_recycles_and_transfers_charge_without_early_release() {
        let quotas = crate::Quotas::new(TestPolicy);
        let length = 1024 * 1024;
        let mut buffer =
            ChargedBuffer::new(quotas.reserve(None, Class, 2 * length).unwrap(), length).unwrap();
        assert_eq!(quotas.used(Class), length);
        assert_eq!(buffer.charge().amount(), length);
        let pointer = buffer.bytes().as_ptr();
        buffer.bytes_mut().fill(0xa7);
        let moved = buffer;
        assert_eq!(moved.bytes().as_ptr(), pointer);
        drop(moved);
        assert_eq!(quotas.used(Class), length);
        let buffer =
            ChargedBuffer::new(quotas.reserve(None, Class, length).unwrap(), length).unwrap();
        assert_eq!(buffer.bytes().as_ptr(), pointer);
        assert!(buffer.bytes().iter().all(|byte| *byte == 0));
        let (bytes, charge) = buffer.into_parts();
        assert_eq!(bytes.as_ptr(), pointer);
        assert_eq!(quotas.used(Class), length);
        drop((bytes, charge));
        assert_eq!(quotas.used(Class), 0);
    }
    #[test]
    fn invalid_lengths_return_charge() {
        let quotas = crate::Quotas::new(TestPolicy);
        for length in [0, 9] {
            assert!(matches!(
                ChargedBuffer::new(quotas.reserve(None, Class, 8).unwrap(), length),
                Err(Error::InvalidInput)
            ));
            assert_eq!(quotas.used(Class), 0);
        }
    }
}
