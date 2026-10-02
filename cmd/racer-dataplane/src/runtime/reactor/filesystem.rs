//! Racer filesystem buffer error boundary.
use super::IoBuffer;
use crate::error::{Error, Result};
pub struct Buffer(pub(super) uring_runtime::reactor::filesystem::Buffer);
// SAFETY: the runtime owner retains its private stable allocation and charge.
unsafe impl IoBuffer for Buffer {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        self.0.bytes().map_err(Into::into)
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        self.0.bytes_mut().map_err(Into::into)
    }
}
impl Buffer {
    pub fn advance(&mut self, n: usize) -> Result<()> {
        self.0.advance(n).map_err(Into::into)
    }
    pub fn remaining(&self) -> usize {
        self.0.remaining()
    }
    pub fn prefix(&self, n: usize) -> Result<&[u8]> {
        self.0.prefix(n).map_err(Into::into)
    }
}
