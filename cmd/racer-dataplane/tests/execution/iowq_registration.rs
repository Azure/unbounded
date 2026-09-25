// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::workers::CpuId;

#[test]
fn iowq_mask_uses_native_words_and_byte_count_for_sparse_cpus() {
    register_iowq_affinity(&[CpuId(4), CpuId(7), CpuId(4097)], |mask, bytes| {
        assert_eq!(bytes as usize, size_of_val(mask));
        let selected: Vec<_> = mask
            .iter()
            .enumerate()
            .flat_map(|(word, &value)| {
                (0..libc::c_ulong::BITS as usize).filter_map(move |bit| {
                    (value & (1 << bit) != 0).then_some(word * libc::c_ulong::BITS as usize + bit)
                })
            })
            .collect();
        assert_eq!(selected, [4, 7, 4097]);
        Ok(())
    })
    .unwrap();
}

#[test]
fn iowq_empty_and_oversized_masks_never_register() {
    for cpus in [vec![], vec![CpuId(usize::MAX)]] {
        let error =
            register_iowq_affinity(&cpus, |_, _| panic!("invalid mask registered")).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
    }
}

#[test]
fn iowq_registration_errors_are_fatal_and_identify_the_operation() {
    for errno in [
        libc::EINVAL,
        libc::EOPNOTSUPP,
        libc::EPERM,
        libc::ENOMEM,
        libc::EFAULT,
    ] {
        let error =
            register_iowq_affinity(&[CpuId(4)], |_, _| Err(io::Error::from_raw_os_error(errno)))
                .unwrap_err();
        assert_eq!(error.kind(), io::Error::from_raw_os_error(errno).kind());
        let message = error.to_string();
        assert!(message.contains("IORING_REGISTER_IOWQ_AFF"), "{message}");
        assert!(message.contains(&format!("os error {errno}")), "{message}");
    }
}

#[test]
fn iowq_real_registration_rejects_zero_byte_count() {
    let raw = match KernelRing::new(8) {
        Ok(raw) => raw,
        Err(error) if matches!(error.raw_os_error(), Some(libc::EPERM | libc::ENOSYS)) => {
            assert!(
                std::env::var_os("RACER_REQUIRE_URING").is_none(),
                "io_uring required: {error}"
            );
            eprintln!("SKIP io_wq registration: {error}");
            return;
        }
        Err(error) => panic!("ring: {error}"),
    };
    let mask = [0 as libc::c_ulong; 1];
    let error = raw.register(17, mask.as_ptr().cast(), 0).unwrap_err();
    assert_eq!(error.raw_os_error(), Some(libc::EINVAL));
    // Some kernels accept a nonzero-size all-zero mask. Reject empty CPU
    // selections ourselves rather than relying on kernel validation.
    let error = raw.register_iowq_affinity(&[]).unwrap_err();
    assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
    assert!(error.to_string().contains("outside reactor physical cores"));
}
