// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use proptest::prelude::*;
use unbounded_storage::memory::HUGEPAGE_2MB;

use crate::fabric::workload::{RunReport, run_workload, workload_strategy};

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 256,
        ..ProptestConfig::default()
    })]

    /// Invariant: fabric request headers preserve the full source range.
    #[test]
    fn invariant_request_header_preserves_source_range(w in workload_strategy()) {
        let report = run_workload(w);
        assert_header_preserves_source_range(&report)?;
    }

    /// Invariant: server page-write planning maps source pages to destination ordinals.
    #[test]
    fn invariant_page_write_planning(w in workload_strategy()) {
        let report = run_workload(w);
        assert_page_write_planning(&report)?;
    }

    /// Invariant: launch availability accounting admits exactly requests that fit.
    #[test]
    fn invariant_launch_availability_accounting(w in workload_strategy()) {
        let report = run_workload(w);
        assert_launch_availability_accounting(&report)?;
    }
}

fn assert_header_preserves_source_range(report: &RunReport) -> Result<(), TestCaseError> {
    prop_assert_eq!(report.header.source(), report.expected_src);
    prop_assert_eq!(report.header.dest_mr_base, report.request_plan.dest_mr_base);
    prop_assert_eq!(report.header.dest_pages, report.request_plan.dest_pages);
    Ok(())
}

fn assert_page_write_planning(report: &RunReport) -> Result<(), TestCaseError> {
    let page_offset = report.page.offset as usize;
    let len = report.page.len as usize;
    let source_start = (report.page.page_idx as usize)
        .checked_mul(HUGEPAGE_2MB)
        .and_then(|n| n.checked_add(page_offset));
    let local_ok = page_offset
        .checked_add(len)
        .is_some_and(|end| end <= HUGEPAGE_2MB)
        && source_start
            .and_then(|n| n.checked_add(len))
            .is_some_and(|end| end <= report.local_mr.len)
        && source_start
            .and_then(|n| report.local_mr.base.checked_add(n))
            .is_some();
    let dest_start = (report.dest_idx as usize)
        .checked_mul(HUGEPAGE_2MB)
        .and_then(|n| n.checked_add(page_offset));
    let dest_pages_len = (report.request_plan.dest_pages as usize).checked_mul(HUGEPAGE_2MB);
    let dest_ok = report.dest_idx < report.request_plan.dest_pages
        && dest_start
            .and_then(|n| n.checked_add(len))
            .zip(dest_pages_len)
            .is_some_and(|(end, limit)| end <= limit)
        && dest_start
            .and_then(|n| report.request_plan.dest_mr_base.checked_add(n as u64))
            .is_some();

    match &report.plan_result {
        Ok(plan) => {
            prop_assert!(
                local_ok && dest_ok,
                "planning succeeded for invalid range: {report:?}"
            );
            let source_start = source_start.expect("valid source start");
            let dest_start = dest_start.expect("valid destination start");
            prop_assert_eq!(plan.src_addr, report.local_mr.base + source_start);
            prop_assert_eq!(
                plan.dest_addr,
                report.request_plan.dest_mr_base + dest_start as u64
            );
            prop_assert_eq!(plan.len, len);
            prop_assert_eq!(plan.ack_page_idx, report.dest_idx);
        }
        Err(_) => {
            prop_assert!(
                !(local_ok && dest_ok),
                "planning failed for valid range: {report:?}"
            );
        }
    }

    Ok(())
}

fn assert_launch_availability_accounting(report: &RunReport) -> Result<(), TestCaseError> {
    // Under the FI_EP_MSG protocol the client posts no recvs of its own;
    // the per-connection RecvPool receives all replies. A bulk_get therefore
    // costs exactly one completion slot (the request send), independent of
    // dst_count, which is retained only for API stability.
    let _ = report.dst_count;
    let expected_required = Some(1);
    prop_assert_eq!(report.required_slots, expected_required);
    let should_fit = expected_required.is_some_and(|required| required <= report.available_slots);
    prop_assert_eq!(report.availability_result.is_ok(), should_fit);
    Ok(())
}
