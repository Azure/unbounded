//! Select RDMA only with compatible authenticated mappings over the entire path.
//! Published Node-annotation mappings must match local hardware; discovery never
//! reports or overrides membership. Missing/incompatible mappings fall back to HTTP.
use super::paths::Route;
use crate::{
    error::{Result, pending},
    model::identity::PageId,
};
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RailId(pub u16);
#[derive(Clone, Debug)]
pub struct RailMapping {
    pub rail: RailId,
    pub fabric: String,
    pub numa_node: Option<usize>,
}
pub enum TransportPlan {
    Http,
    Rdma { rail: RailId },
}
pub struct Rails;
impl Rails {
    pub fn select(&self, _route: &Route, _page: &PageId) -> Result<TransportPlan> {
        pending("rails.select")
    }
}
#[cfg(test)]
mod tests { /* Multi-hop mismatches, disable annotation, deterministic page-to-rail. */
}
