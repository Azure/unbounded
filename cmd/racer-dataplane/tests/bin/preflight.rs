// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn quota_is_not_affinity() {
        for text in ["300000 100000", "max 100000", "600000 200000"] {
            quota(text).unwrap();
        }
        for text in [
            "299999 100000",
            "100000 100000",
            "max 0",
            "max",
            "-1 100000",
        ] {
            assert!(quota(text).is_err(), "{text}");
        }
    }
    #[test]
    fn sparse_and_full_slab_headroom() {
        assert_eq!(
            storage_needed(10 * 1024 * MIB, 4096).unwrap(),
            12 * 1024 * MIB - 4096
        );
        assert_eq!(
            storage_needed(10 * 1024 * MIB, 10 * 1024 * MIB).unwrap(),
            HEADROOM
        );
        assert!(storage_needed(u64::MAX, 0).is_err());
    }
    #[test]
    fn reject_unbounded_or_changed_profile() {
        profile(1, 1, 1, 8, 10 * 1024 * MIB).unwrap();
        profile(1, 1, 1, 8, 64 * MIB).unwrap();
        for args in [
            (32, 1, 1, 8, 64 * MIB),
            (1, 2, 1, 8, 64 * MIB),
            (1, 1, 2, 8, 64 * MIB),
            (1, 1, 1, 32, 64 * MIB),
            (1, 1, 1, 8, 11 * 1024 * MIB),
            (1, 1, 1, 8, 64 * MIB + 1),
        ] {
            assert!(profile(args.0, args.1, args.2, args.3, args.4).is_err());
        }
    }
    #[test]
    fn cgroup_namespace_is_fail_closed() {
        assert_eq!(
            cgroup_path("0::/pod/container\n").unwrap(),
            Path::new("/sys/fs/cgroup/pod/container")
        );
        assert_eq!(cgroup_path("0::/\n").unwrap(), Path::new("/sys/fs/cgroup"));
        for text in ["1:cpu:/pod", "0::/../../host", "0::relative"] {
            assert!(cgroup_path(text).is_err());
        }
    }
}
