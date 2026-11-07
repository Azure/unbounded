//! Effect identifiers shared by the read-path machines.
//!
//! Every effect a machine emits carries an [`EffectId`]; the interpreter answers it with an input
//! carrying the same id. A machine ignores an input whose id it is not waiting for, so a late
//! completion of an abandoned effect cannot move it.

/// Identifies one outstanding effect.
#[derive(Clone, Copy, Debug, Default, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct EffectId(pub u64);

/// Allocates [`EffectId`]s. One allocator per worker keeps ids unique across its machines.
#[derive(Debug, Default)]
pub struct EffectIds {
    next: u64,
}

impl EffectIds {
    pub fn new() -> Self {
        Self::default()
    }

    /// A fresh id. Ids never repeat within one allocator.
    pub fn allocate(&mut self) -> EffectId {
        self.next += 1;
        EffectId(self.next)
    }
}

/// An input answered an effect the machine is not waiting for.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Stale(pub EffectId);

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ids_are_unique_and_increasing() {
        let mut ids = EffectIds::new();
        let a = ids.allocate();
        let b = ids.allocate();
        assert_ne!(a, b);
        assert!(a < b);
        assert_ne!(a, EffectId::default());
    }
}
