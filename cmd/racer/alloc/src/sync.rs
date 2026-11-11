//! Atomics and `Arc` used by cross-thread memory paths, swapped for loom's
//! model-checked versions under `--cfg loom`.

#[cfg(loom)]
pub(crate) use loom::sync::{
    Arc,
    atomic::{AtomicBool, AtomicPtr, AtomicUsize, Ordering},
};

#[cfg(not(loom))]
pub(crate) use std::sync::{
    Arc,
    atomic::{AtomicBool, AtomicPtr, AtomicUsize, Ordering},
};
