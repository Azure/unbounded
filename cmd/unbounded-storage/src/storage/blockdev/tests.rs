// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#![cfg(test)]

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll, Wake, Waker};

use super::{BlockDevice, MockDevice, MockDeviceConfig, MockFaultMode};
use crate::storage::types::{Error, Lba};

// Minimal noop-waker block_on so we can exercise async fns without
// pulling in tokio. Matches the pattern documented in the crate
// AGENTS.md.
struct NoopWake;
impl Wake for NoopWake {
    fn wake(self: Arc<Self>) {}
}

fn block_on<F: Future>(fut: F) -> F::Output {
    let waker: Waker = Arc::new(NoopWake).into();
    let mut cx = Context::from_waker(&waker);
    let mut fut = Box::pin(fut);
    let mut spins = 0u32;
    loop {
        match Pin::as_mut(&mut fut).poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => {
                spins += 1;
                assert!(spins < 1_000_000, "block_on spun without progress");
            }
        }
    }
}

#[test]
fn read_back_what_we_wrote() {
    let dev = MockDevice::new(MockDeviceConfig {
        page_size: 64,
        capacity_pages: 4,
        ..Default::default()
    });
    let src = vec![0xabu8; 64];
    block_on(dev.write(Lba(2), &src)).unwrap();
    let mut dst = vec![0u8; 64];
    block_on(dev.read(Lba(2), &mut dst)).unwrap();
    assert_eq!(dst, src);
    assert_eq!(dev.reads(), 1);
    assert_eq!(dev.writes(), 1);
}

#[test]
fn read_io_fault_returns_err() {
    let dev = MockDevice::new(MockDeviceConfig::default());
    dev.set_fault_mode(MockFaultMode::ReadIo);
    let mut dst = vec![0u8; 4096];
    let err = block_on(dev.read(Lba(0), &mut dst));
    assert!(matches!(err, Err(Error::Io(_))));
}

#[test]
fn read_corruption_flips_byte() {
    let dev = MockDevice::new(MockDeviceConfig {
        page_size: 16,
        capacity_pages: 2,
        ..Default::default()
    });
    let src = vec![0x11u8; 16];
    block_on(dev.write(Lba(0), &src)).unwrap();
    dev.set_fault_mode(MockFaultMode::ReadCorrupt);
    let mut dst = vec![0u8; 16];
    block_on(dev.read(Lba(0), &mut dst)).unwrap();
    assert_ne!(dst[0], src[0]);
    assert_eq!(&dst[1..], &src[1..]);
}

#[test]
fn out_of_range_lba_errors() {
    let dev = MockDevice::new(MockDeviceConfig {
        page_size: 16,
        capacity_pages: 2,
        ..Default::default()
    });
    let mut dst = vec![0u8; 16];
    assert!(matches!(
        block_on(dev.read(Lba(5), &mut dst)),
        Err(Error::OutOfRange)
    ));
    assert!(matches!(
        block_on(dev.write(Lba(5), &dst)),
        Err(Error::OutOfRange)
    ));
}

#[test]
fn register_buffers_records_handle() {
    let dev = MockDevice::new(MockDeviceConfig::default());
    let mut buf = vec![0u8; 4096];
    dev.register_buffers(buf.as_mut_ptr(), buf.len()).unwrap();
    assert_eq!(dev.registered_base(), Some(buf.as_mut_ptr()));
    assert_eq!(dev.registered_len(), buf.len());
}

#[test]
fn peek_poke_match_read_write() {
    let dev = MockDevice::new(MockDeviceConfig {
        page_size: 8,
        capacity_pages: 4,
        ..Default::default()
    });
    dev.poke(Lba(1), &[1, 2, 3, 4, 5, 6, 7, 8]);
    let mut buf = [0u8; 8];
    block_on(dev.read(Lba(1), &mut buf)).unwrap();
    assert_eq!(buf, [1, 2, 3, 4, 5, 6, 7, 8]);
    block_on(dev.write(Lba(2), &[9, 9, 9, 9, 9, 9, 9, 9])).unwrap();
    let mut peeked = [0u8; 8];
    dev.peek(Lba(2), &mut peeked);
    assert_eq!(peeked, [9, 9, 9, 9, 9, 9, 9, 9]);
}
