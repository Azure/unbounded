//! Parsing of `/proc` cgroup membership, mountinfo and cgroup CPU limits.
//!
//! Everything here is pure string processing so it can be tested without a
//! real cgroup hierarchy (and under Miri).

use std::collections::HashSet;
use std::ffi::OsString;
use std::num::NonZeroU64;
use std::os::unix::ffi::OsStringExt;
use std::path::{Component, Path, PathBuf};

use super::CpuQuota;
use crate::{Error, ErrorKind, Result};

fn invalid(context: &'static str) -> Error {
    Error::new(ErrorKind::InvalidConfiguration).with_context(context)
}

/// A cgroup hierarchy the calling thread belongs to, resolved to a directory.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct Hierarchy {
    /// The thread's cgroup directory in this hierarchy.
    pub(super) leaf: PathBuf,
    /// The mount point; ancestors above it are not visible.
    pub(super) mount: PathBuf,
    /// Whether this is the cgroup v2 unified hierarchy.
    pub(super) unified: bool,
    /// Whether the `cpu` controller (quota) applies.
    pub(super) cpu: bool,
    /// Whether the `cpuset` controller applies.
    pub(super) cpuset: bool,
}

/// Resolve the calling thread's cgroup directories for the `cpu` and `cpuset`
/// controllers.
///
/// `memberships` is the content of `/proc/thread-self/cgroup` and `mounts` of
/// `/proc/self/mountinfo`; both are relative to the caller's namespaces. Only
/// mounts whose root covers the membership path are used: a subtree mount that
/// does not contain the thread's cgroup is not a fallback. If an applicable
/// hierarchy (v2, `cpu` or `cpuset`) is not visible through any mount, this
/// fails closed with [`ErrorKind::InvalidConfiguration`]: an invisible limit is
/// unknown, not unlimited.
pub(super) fn hierarchies(memberships: &str, mounts: &str) -> Result<Vec<Hierarchy>> {
    let memberships = memberships
        .lines()
        .map(|line| {
            let fields: Vec<&str> = line.splitn(3, ':').collect();
            let [_, controllers, path] = fields[..] else {
                return Err(invalid("malformed cgroup membership"));
            };
            let path = Path::new(path);
            if !path.is_absolute() || path.components().any(|c| c == Component::ParentDir) {
                return Err(invalid("malformed cgroup membership path"));
            }
            Ok((controllers, path))
        })
        .collect::<Result<Vec<_>>>()?;

    let mut resolved = Vec::new();
    let mut covered = HashSet::new();
    for line in mounts.lines() {
        let Some((before, after)) = line.split_once(" - ") else {
            continue;
        };
        let before: Vec<&str> = before.split_whitespace().collect();
        let after: Vec<&str> = after.split_whitespace().collect();
        if before.len() < 5 || after.len() < 3 {
            continue;
        }
        let unified = after[0] == "cgroup2";
        if !unified && after[0] != "cgroup" {
            continue;
        }
        let options: HashSet<&str> = after[2].split(',').collect();
        let cpu = unified || options.contains("cpu");
        let cpuset = unified || options.contains("cpuset");
        if !cpu && !cpuset {
            continue;
        }
        for (index, (controllers, membership)) in memberships.iter().enumerate() {
            let applies = if unified {
                controllers.is_empty()
            } else {
                controllers.split(',').any(|c| options.contains(c))
            };
            if !applies {
                continue;
            }
            let mount_root = unescape_mount(before[3]);
            let mount = unescape_mount(before[4]);
            if !mount_root.is_absolute() || !mount.is_absolute() {
                return Err(invalid("malformed mountinfo path"));
            }
            let Ok(relative) = membership.strip_prefix(&mount_root) else {
                continue;
            };
            if unified {
                covered.insert((index, ""));
            }
            if cpu {
                covered.insert((index, "cpu"));
            }
            if cpuset {
                covered.insert((index, "cpuset"));
            }
            resolved.push(Hierarchy {
                leaf: mount.join(relative),
                mount,
                unified,
                cpu,
                cpuset,
            });
        }
    }
    for (index, (controllers, _)) in memberships.iter().enumerate() {
        for controller in controllers.split(',') {
            if matches!(controller, "" | "cpu" | "cpuset")
                && !covered.contains(&(index, controller))
            {
                return Err(invalid("cgroup hierarchy not visible"));
            }
        }
    }
    Ok(resolved)
}

/// Decode the octal escapes (`\040`, `\011`, `\012`, `\134`, ...) that
/// `/proc/self/mountinfo` uses for special bytes, in a single pass.
///
/// A backslash not followed by three octal digits is kept literally.
pub(super) fn unescape_mount(value: &str) -> PathBuf {
    let bytes = value.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index] == b'\\'
            && let Some(digits) = bytes.get(index + 1..index + 4)
            && digits.iter().all(|d| (b'0'..=b'7').contains(d))
        {
            let value = digits
                .iter()
                .fold(0u32, |acc, d| acc * 8 + u32::from(d - b'0'));
            if let Ok(byte) = u8::try_from(value) {
                out.push(byte);
                index += 4;
                continue;
            }
        }
        out.push(bytes[index]);
        index += 1;
    }
    PathBuf::from(OsString::from_vec(out))
}

/// Parse a cgroup v2 `cpu.max` value (`"<quota> <period>"` or `"max <period>"`).
pub(super) fn parse_v2_quota(value: &str) -> Result<Option<CpuQuota>> {
    let mut fields = value.split_whitespace();
    let (Some(quota), Some(period), None) = (fields.next(), fields.next(), fields.next()) else {
        return Err(invalid("cpu.max"));
    };
    parse_v1_quota(if quota == "max" { "-1" } else { quota }, period)
}

/// Parse cgroup v1 `cpu.cfs_quota_us` and `cpu.cfs_period_us` values, where a
/// quota of `-1` means unlimited.
pub(super) fn parse_v1_quota(quota: &str, period: &str) -> Result<Option<CpuQuota>> {
    let period = period
        .trim()
        .parse::<NonZeroU64>()
        .map_err(|_| invalid("cpu quota period"))?;
    if quota.trim() == "-1" {
        return Ok(None);
    }
    let quota = quota
        .trim()
        .parse::<NonZeroU64>()
        .map_err(|_| invalid("cpu quota"))?;
    Ok(Some(CpuQuota { quota, period }))
}

/// Keep the smaller of two quota ratios, compared exactly in 128-bit arithmetic.
pub(super) fn tighten_quota(current: &mut Option<CpuQuota>, candidate: Option<CpuQuota>) {
    if let Some(candidate) = candidate
        && current.is_none_or(|old| {
            u128::from(candidate.quota.get()) * u128::from(old.period.get())
                < u128::from(old.quota.get()) * u128::from(candidate.period.get())
        })
    {
        *current = Some(candidate);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hierarchy(leaf: &str, mount: &str, unified: bool, cpu: bool, cpuset: bool) -> Hierarchy {
        Hierarchy {
            leaf: leaf.into(),
            mount: mount.into(),
            unified,
            cpu,
            cpuset,
        }
    }

    fn pair(quota: Option<CpuQuota>) -> Option<(u64, u64)> {
        quota.map(|q| (q.quota.get(), q.period.get()))
    }

    #[test]
    fn noncovering_mounts_are_skipped_and_hidden_hierarchies_fail_closed() {
        let unrelated = "1 0 0:1 /other /unrelated rw - cgroup2 cgroup rw\n";
        let covering = "2 0 0:1 /tenant /visible\\040group rw - cgroup2 cgroup rw\n";
        let found = hierarchies("0::/tenant/leaf", &format!("{unrelated}{covering}")).unwrap();
        assert_eq!(found.len(), 1);
        assert_eq!(found[0].leaf, PathBuf::from("/visible group/leaf"));
        assert!(hierarchies("0::/tenant/leaf", unrelated).is_err());
        assert!(hierarchies("0::/tenant/leaf", "").is_err());
        assert!(hierarchies("2:cpu:/tenant/leaf", "").is_err());
        assert!(hierarchies("2:cpuset:/tenant/leaf", "").is_err());
        assert!(hierarchies("2:memory:/tenant/leaf", "").unwrap().is_empty());
        let found = hierarchies("0::/", "1 0 0:1 / /visible rw - cgroup2 cgroup rw").unwrap();
        assert_eq!(found[0].leaf, PathBuf::from("/visible"));
    }

    #[test]
    fn v1_and_v2_mounts_are_resolved_exactly() {
        let mounts = "1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n\
            2 0 0:2 /tenant /cpu rw - cgroup cgroup rw,cpu,cpuacct\n\
            3 0 0:3 / /sets rw - cgroup cgroup rw,cpuset\n";
        let found = hierarchies(
            "0::/tenant/leaf\n2:cpu,cpuacct:/tenant/leaf\n3:cpuset:/tenant/leaf\n",
            mounts,
        )
        .unwrap();
        assert_eq!(
            found,
            [
                hierarchy(
                    "/sys/fs/cgroup/tenant/leaf",
                    "/sys/fs/cgroup",
                    true,
                    true,
                    true
                ),
                hierarchy("/cpu/leaf", "/cpu", false, true, false),
                hierarchy("/sets/tenant/leaf", "/sets", false, false, true),
            ]
        );
        assert!(hierarchies("0::/../escape", mounts).is_err());
    }

    #[test]
    fn namespace_paths_and_mount_escapes_are_resolved() {
        let mounts = "malformed\n1 0 0:1 / /ignored rw - tmpfs tmpfs rw\n\
            2 0 0:2 / /memory rw - cgroup cgroup rw,memory\n\
            3 0 0:3 /host\\040root /group\\040mount rw - cgroup2 cgroup rw\n";
        assert_eq!(
            hierarchies("0::/host root/leaf", mounts).unwrap(),
            [hierarchy(
                "/group mount/leaf",
                "/group mount",
                true,
                true,
                true
            )]
        );
        for membership in [
            "0::/leaf",
            "0::/",
            "malformed",
            "0::relative",
            "0::/leaf/../escape",
        ] {
            let error = hierarchies(membership, mounts).unwrap_err();
            assert_eq!(
                error.kind(),
                ErrorKind::InvalidConfiguration,
                "{membership}"
            );
        }
    }

    #[test]
    fn mount_escapes_are_decoded_once() {
        assert_eq!(
            unescape_mount(r"a\040b\011c\012d\134040"),
            PathBuf::from("a b\tc\nd\\040")
        );
        // Incomplete or non-octal escapes stay literal; \777 does not fit a byte.
        assert_eq!(unescape_mount(r"x\04"), PathBuf::from(r"x\04"));
        assert_eq!(unescape_mount(r"x\089"), PathBuf::from(r"x\089"));
        assert_eq!(unescape_mount(r"x\777"), PathBuf::from(r"x\777"));
        assert_eq!(unescape_mount("\\"), PathBuf::from("\\"));
    }

    #[test]
    fn quota_formats_validate_fields_and_unlimited_periods() {
        for (quota, period, expected) in [
            ("150000", "100000", Some((150000, 100000))),
            (" 1\n", " 2\n", Some((1, 2))),
            ("-1", "100000", None),
        ] {
            let v1 = parse_v1_quota(quota, period).unwrap();
            let v2_quota = if quota == "-1" { "max" } else { quota };
            let v2 = parse_v2_quota(&format!("{v2_quota} {period}")).unwrap();
            assert_eq!(pair(v1), expected);
            assert_eq!(pair(v2), expected);
        }
        for value in [
            "",
            "max",
            "1 2 3",
            "0 1",
            "-2 1",
            "1 0",
            "max 0",
            "max nope",
            "1 -1",
            "nope 1",
            "18446744073709551616 1",
            "1 18446744073709551616",
        ] {
            let error = parse_v2_quota(value).unwrap_err();
            assert_eq!(error.kind(), ErrorKind::InvalidConfiguration, "{value}");
        }
        assert!(parse_v1_quota("-2", "100000").is_err());
    }

    #[test]
    fn tightest_ancestor_quota_wins() {
        let mut quota = parse_v2_quota("400000 100000").unwrap();
        tighten_quota(&mut quota, parse_v1_quota("150000", "100000").unwrap());
        tighten_quota(&mut quota, parse_v2_quota("max 100000").unwrap());
        tighten_quota(&mut quota, parse_v2_quota("200000 100000").unwrap());
        assert_eq!(pair(quota), Some((150000, 100000)));
    }

    #[test]
    fn quotas_compare_ratios_without_rounding_or_overflow() {
        let mut quota = None;
        for (candidate, expected) in [
            ("max 10", None),
            ("3 2", Some((3, 2))),
            ("4 3", Some((4, 3))),
            ("8 6", Some((4, 3))),
            ("max 10", Some((4, 3))),
            (
                "18446744073709551615 18446744073709551614",
                Some((u64::MAX, u64::MAX - 1)),
            ),
            (
                "18446744073709551614 18446744073709551615",
                Some((u64::MAX - 1, u64::MAX)),
            ),
            ("2 1", Some((u64::MAX - 1, u64::MAX))),
        ] {
            tighten_quota(&mut quota, parse_v2_quota(candidate).unwrap());
            assert_eq!(pair(quota), expected, "{candidate}");
        }
    }
}
