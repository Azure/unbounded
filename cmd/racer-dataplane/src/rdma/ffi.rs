// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Native provider ABI. Resource lifetime belongs to the enclosing DMA owner.
use super::*;
#[repr(C)]
#[derive(Clone, Copy)]
pub struct Rail {
    pub name: [c_char; 64],
    pub gid: [u8; 16],
    pub max_read: u32,
    pub lid: u16,
    pub port: u8,
    pub gid_index: u8,
    pub mtu: u8,
    pub ethernet: u8,
    pub windows: u8,
    pub pad: u8,
}
pub(crate) use crate::negotiation::Endpoint;
#[repr(C)]
#[derive(Clone, Copy, Default)]
pub struct Wc {
    pub id: u64,
    pub status: u32,
    pub opcode: u32,
    pub len: u32,
    pub qpn: u32,
}
unsafe extern "C" {
    pub fn racer_discover(out: *mut Rail, capacity: i32) -> i32;
    pub fn racer_open(
        name: *const c_char,
        pool: *mut u8,
        pool_len: usize,
        cqe: i32,
        out: *mut *mut c_void,
    ) -> i32;
    pub fn racer_close(d: *mut c_void) -> i32;
    pub fn racer_fd(d: *mut c_void, asynchronous: i32) -> i32;
    pub fn racer_notify(d: *mut c_void) -> i32;
    pub fn racer_event(d: *mut c_void, asynchronous: i32, qpn: *mut u32) -> i32;
    pub fn racer_poll(d: *mut c_void, out: *mut Wc, capacity: i32) -> i32;
    pub fn racer_qp(
        d: *mut c_void,
        depth: u32,
        rail: *const Rail,
        psn: u32,
        out: *mut Endpoint,
    ) -> *mut c_void;
    pub fn racer_init(qp: *mut c_void, port: u8) -> i32;
    pub fn racer_connect(
        qp: *mut c_void,
        rail: *const Rail,
        peer: *const Endpoint,
        psn: u32,
        reads: u8,
    ) -> i32;
    pub fn racer_destroy_qp(d: *mut c_void, qp: *mut c_void) -> i32;
    pub fn racer_destroy_qps(d: *mut c_void, qps: *mut *mut c_void, count: usize) -> i32;
    pub fn racer_window(d: *mut c_void, key: *mut u32) -> *mut c_void;
    pub fn racer_free_windows(d: *mut c_void, windows: *mut *mut c_void, count: usize) -> i32;
    pub fn racer_post(
        d: *mut c_void,
        qp: *mut c_void,
        op: u32,
        id: u64,
        address: *mut u8,
        len: u32,
        remote: u64,
        key: u32,
        mw: *mut c_void,
    ) -> i32;
}
