//! Shared harness for extent volume tests under the simulated reactor.

#![allow(dead_code)]

use page_alloc::volume::{
    Key, Limits, MountReport, Reservation, SLOT_BYTES, Tier, UNIT_BYTES, Value, Volume,
    VolumeOptions,
};
use page_alloc::{AlignedBuffer, Error, Slab};
use std::{
    future::Future,
    pin::Pin,
    rc::Rc,
    task::{Context, Poll, Waker},
};
use uring_runtime::{Scope, reactor::Reactor};

/// Allocator and runtime errors kept apart.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TestError {
    /// Allocator error.
    Alloc(Error),

    /// Runtime error.
    Runtime(uring_runtime::Error),
}

impl From<Error> for TestError {
    /// Keep the allocator category.
    fn from(error: Error) -> Self {
        Self::Alloc(error)
    }
}

impl From<uring_runtime::Error> for TestError {
    /// Keep the runtime category.
    fn from(error: uring_runtime::Error) -> Self {
        Self::Runtime(error)
    }
}

/// Always-live scope.
#[derive(Clone)]
pub struct TestScope;

impl Scope for TestScope {
    type Error = TestError;

    /// Never cancel.
    fn check(&self) -> Result<(), TestError> {
        Ok(())
    }
}

/// Shared scope instance.
pub static SCOPE: TestScope = TestScope;

/// Test reactor type.
pub type TestReactor = Reactor<TestScope, ()>;

/// Boxed local future.
pub type Task<'a, T> = Pin<Box<dyn Future<Output = T> + 'a>>;

/// Drive one future to completion with a bounded number of reactor turns.
pub fn drive<T>(reactor: &TestReactor, mut operation: Task<'_, T>) -> T {
    for _ in 0..1_000_000 {
        if let Poll::Ready(value) = operation
            .as_mut()
            .poll(&mut Context::from_waker(Waker::noop()))
        {
            return value;
        }
        reactor.poll_budgeted(64).unwrap();
    }
    panic!("operation did not complete");
}

/// Drive several futures together until all complete.
pub fn drive_all<'a, T>(reactor: &TestReactor, operations: Vec<Task<'a, T>>) -> Vec<T> {
    let mut slots: Vec<(Option<Task<'a, T>>, Option<T>)> =
        operations.into_iter().map(|op| (Some(op), None)).collect();
    for _ in 0..1_000_000 {
        let mut pending = false;
        for (operation, output) in &mut slots {
            if let Some(op) = operation {
                if let Poll::Ready(value) =
                    op.as_mut().poll(&mut Context::from_waker(Waker::noop()))
                {
                    *output = Some(value);
                    *operation = None;
                } else {
                    pending = true;
                }
            }
        }
        if !pending {
            return slots.into_iter().map(|(_, o)| o.unwrap()).collect();
        }
        reactor.poll_budgeted(64).unwrap();
    }
    panic!("operations did not complete");
}

/// Slab path used by the tests.
pub const PATH: &str = "/volume/slab";

/// New slab of `slots` real-size slots (sparse in the simulation).
pub fn slab(slots: u64) -> Rc<Slab<()>> {
    Rc::new(Slab::new(
        PATH.into(),
        slots * SLOT_BYTES,
        SLOT_BYTES,
        SLOT_BYTES as usize,
    ))
}

/// Mount options with no accounting.
pub fn options(limits: Limits) -> VolumeOptions<()> {
    VolumeOptions {
        limits,
        volume_id: [0x5a; 16],
        format: true,
        charge: Rc::new(|_| Ok(())),
    }
}

/// A mounted volume, its slab, and the mount report.
pub type Mounted = (Rc<Volume<()>>, Rc<Slab<()>>, MountReport);

/// Mount a volume on a new slab.
pub fn mount(reactor: &TestReactor, slots: u64, limits: Limits) -> Result<Mounted, TestError> {
    let slab = slab(slots);
    let (volume, report) = drive(
        reactor,
        Volume::mount(reactor, slab.clone(), options(limits), &SCOPE),
    )?;
    Ok((volume, slab, report))
}

/// Bytes of a test record header: key then generation.
pub const HEADER: usize = 32;

/// Record bytes for `key`, `generation`, and a payload byte.
pub fn record(slab: &Slab<()>, key: &Key, generation: u64, length: u64) -> AlignedBuffer<()> {
    let padded = (length as usize).div_ceil(UNIT_BYTES) * UNIT_BYTES;
    let mut buffer = slab.allocate(padded, ()).unwrap();
    let bytes = buffer.as_mut_slice();
    bytes.fill(key.0[23]);
    bytes[..24].copy_from_slice(&key.0);
    bytes[24..32].copy_from_slice(&generation.to_le_bytes());
    buffer
}

/// Whether a read record carries `key` and `generation`.
pub fn record_matches(bytes: &[u8], key: &Key, generation: u64) -> bool {
    bytes[..24] == key.0 && bytes[24..32] == generation.to_le_bytes()
}

/// Write a reserved record and return the reservation when the write worked.
pub fn write(
    reactor: &TestReactor,
    volume: &Volume<()>,
    slab: &Slab<()>,
    reservation: &Reservation,
    key: &Key,
    generation: u64,
    length: u64,
) -> Result<(), TestError> {
    let buffer = record(slab, key, generation, length);
    drive(reactor, volume.write(reactor, reservation, buffer, &SCOPE)).map(drop)
}

/// Allocate, write, and publish one record. Returns its value.
pub fn put(
    reactor: &TestReactor,
    volume: &Volume<()>,
    slab: &Slab<()>,
    tier: Tier,
    key: Key,
    length: u64,
    retain_until: u64,
) -> Result<Value, TestError> {
    let reservation = volume.allocate(tier, length)?;
    let generation = volume.next_generation();
    write(
        reactor,
        volume,
        slab,
        &reservation,
        &key,
        generation,
        length,
    )?;
    volume.publish(key, reservation, length as u32, generation, retain_until)?;
    Ok(volume.get(&key).unwrap())
}

/// Read the record of `value` and check its header.
pub fn check(
    reactor: &TestReactor,
    volume: &Volume<()>,
    slab: &Slab<()>,
    key: &Key,
    value: &Value,
) -> Result<bool, TestError> {
    let buffer = slab.allocate(UNIT_BYTES, ()).unwrap();
    let buffer = drive(reactor, volume.read(reactor, value, buffer, &SCOPE))?;
    Ok(record_matches(buffer.as_slice(), key, value.generation))
}

/// Test key with a cache prefix and a number.
pub fn key(cache: u8, n: u32) -> Key {
    Key::new([cache; 8], [n as u8; 12], n)
}
