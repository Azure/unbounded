//! Fake grants and late completions; real kernel/NIC fences need gated hardware tests.
use crate::{
    error::{Result, pending},
    model::identity::TransferId,
};
pub struct FaultRdma;
impl FaultRdma {
    pub fn late_write(&self, _transfer: TransferId) -> Result<()> {
        pending("test.rdma.late_write")
    }
}
