// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::ptr;

use proptest::prelude::*;
use unbounded_storage::bufferpool::{BulkRef, PageRef, StripeKey};
use unbounded_storage::fabric::{
    FabricError, MAX_HOPS, MrHandle, RequestHeader, RequestPlan, ensure_launch_fits_registry,
    plan_page_write, required_completion_slots,
};
use unbounded_storage::memory::HUGEPAGE_2MB;

#[derive(Clone, Debug)]
pub struct Workload {
    pub request_id: u32,
    pub remote_key: u64,
    pub src: BulkRef,
    pub local_base: usize,
    pub dest_base: u64,
    pub local_pages: u32,
    pub dest_pages: u32,
    pub page: PageRef,
    pub dest_idx: u32,
    pub available_slots: usize,
    pub dst_count: usize,
}

#[derive(Debug)]
pub struct RunReport {
    pub expected_src: BulkRef,
    pub header: RequestHeader,
    pub plan_result: Result<unbounded_storage::fabric::PageWritePlan, FabricError>,
    pub availability_result: Result<(), FabricError>,
    pub required_slots: Option<usize>,
    pub local_mr: MrHandle,
    pub request_plan: RequestPlan,
    pub page: PageRef,
    pub dest_idx: u32,
    pub available_slots: usize,
    pub dst_count: usize,
}

pub fn workload_strategy() -> impl Strategy<Value = Workload> {
    (
        any::<u32>(),
        any::<u64>(),
        bulk_ref_strategy(),
        0usize..=(usize::MAX / 4),
        0u64..=(u64::MAX / 4),
        1u32..=8,
        0u32..=8,
        page_ref_strategy(),
        0u32..=10,
        prop_oneof![0usize..=16, Just(usize::MAX)],
        prop_oneof![0usize..=16, Just(usize::MAX)],
    )
        .prop_map(
            |(
                request_id,
                remote_key,
                src,
                local_base,
                dest_base,
                local_pages,
                dest_pages,
                page,
                dest_idx,
                available_slots,
                dst_count,
            )| Workload {
                request_id,
                remote_key,
                src,
                local_base,
                dest_base,
                local_pages,
                dest_pages,
                page,
                dest_idx,
                available_slots,
                dst_count,
            },
        )
}

pub fn run_workload(w: Workload) -> RunReport {
    let local_mr = mr(
        w.remote_key,
        w.local_base,
        w.local_pages as usize * HUGEPAGE_2MB,
    );
    let dest_mr = mr(
        w.remote_key,
        w.dest_base as usize,
        w.dest_pages as usize * HUGEPAGE_2MB,
    );
    let header = RequestHeader::new(w.request_id, 0, &dest_mr, w.dest_pages, w.src, MAX_HOPS);
    let request_plan = RequestPlan {
        dest_mr_base: header.dest_mr_base,
        dest_pages: header.dest_pages,
    };

    RunReport {
        expected_src: w.src,
        plan_result: plan_page_write(&local_mr, &request_plan, w.dest_idx, w.page, HUGEPAGE_2MB),
        availability_result: ensure_launch_fits_registry(w.available_slots, w.dst_count),
        required_slots: required_completion_slots(w.dst_count),
        local_mr,
        request_plan,
        header,
        page: w.page,
        dest_idx: w.dest_idx,
        available_slots: w.available_slots,
        dst_count: w.dst_count,
    }
}

fn bulk_ref_strategy() -> impl Strategy<Value = BulkRef> {
    (any::<[u8; 32]>(), any::<u64>(), any::<u32>()).prop_map(|(stripe, offset, len)| BulkRef {
        stripe: StripeKey(stripe),
        offset,
        len,
    })
}

fn page_ref_strategy() -> impl Strategy<Value = PageRef> {
    (
        0u32..=10,
        prop_oneof![
            0u32..=4096,
            (HUGEPAGE_2MB as u32 - 4096)..=HUGEPAGE_2MB as u32
        ],
        prop_oneof![
            0u32..=4096,
            (HUGEPAGE_2MB as u32 - 4096)..=HUGEPAGE_2MB as u32
        ],
    )
        .prop_map(|(page_idx, offset, len)| PageRef {
            page_idx,
            offset,
            len,
        })
}

fn mr(remote_key: u64, base: usize, len: usize) -> MrHandle {
    MrHandle {
        mr: ptr::null_mut(),
        remote_key,
        base,
        remote_base: base as u64,
        len,
    }
}
