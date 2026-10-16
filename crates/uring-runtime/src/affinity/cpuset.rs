//! A set of logical CPU numbers.

use std::collections::BTreeSet;
use std::fmt;
use std::str::FromStr;

use crate::{Error, ErrorKind};

/// A set of logical CPU numbers, as used by Linux affinity masks and cpusets.
///
/// Iteration is in ascending order. [`Display`](fmt::Display) and
/// [`FromStr`] use the Linux CPU list format found in sysfs and cgroup files,
/// for example `0-3,8,10-11`.
///
/// ```
/// use uring_runtime::affinity::CpuSet;
///
/// let set: CpuSet = "0-2,8".parse().unwrap();
/// assert_eq!(set.len(), 4);
/// assert!(set.contains(8));
/// assert_eq!(set.to_string(), "0-2,8");
/// ```
#[derive(Clone, Debug, Default, Eq, PartialEq, Hash)]
pub struct CpuSet {
    cpus: BTreeSet<usize>,
}

impl CpuSet {
    /// The largest CPU number accepted by parsing and by
    /// [`set_cpus`](super::set_cpus). This is far above any `CONFIG_NR_CPUS`
    /// the kernel supports and bounds the size of affinity masks.
    pub const MAX_CPU: usize = (1 << 20) - 1;

    /// Create an empty set.
    pub const fn new() -> Self {
        Self {
            cpus: BTreeSet::new(),
        }
    }

    /// Add a CPU. Returns `false` if it was already present.
    pub fn insert(&mut self, cpu: usize) -> bool {
        self.cpus.insert(cpu)
    }

    /// Remove a CPU. Returns `false` if it was not present.
    pub fn remove(&mut self, cpu: usize) -> bool {
        self.cpus.remove(&cpu)
    }

    /// Whether the set contains `cpu`.
    pub fn contains(&self, cpu: usize) -> bool {
        self.cpus.contains(&cpu)
    }

    /// The number of CPUs in the set.
    pub fn len(&self) -> usize {
        self.cpus.len()
    }

    /// Whether the set is empty.
    pub fn is_empty(&self) -> bool {
        self.cpus.is_empty()
    }

    /// The lowest CPU number in the set.
    pub fn first(&self) -> Option<usize> {
        self.cpus.first().copied()
    }

    /// The highest CPU number in the set.
    pub fn last(&self) -> Option<usize> {
        self.cpus.last().copied()
    }

    /// Iterate over the CPUs in ascending order.
    pub fn iter(&self) -> impl DoubleEndedIterator<Item = usize> + ExactSizeIterator + '_ {
        self.cpus.iter().copied()
    }

    /// Keep only the CPUs for which `keep` returns `true`.
    pub fn retain(&mut self, mut keep: impl FnMut(usize) -> bool) {
        self.cpus.retain(|cpu| keep(*cpu));
    }

    /// The CPUs present in both `self` and `other`.
    #[must_use]
    pub fn intersection(&self, other: &CpuSet) -> CpuSet {
        self.cpus.intersection(&other.cpus).copied().collect()
    }

    /// Whether every CPU in `self` is also in `other`.
    pub fn is_subset(&self, other: &CpuSet) -> bool {
        self.cpus.is_subset(&other.cpus)
    }

    /// Decode a native-endian kernel affinity mask.
    pub(crate) fn from_mask(mask: &[usize]) -> Self {
        let bits = usize::BITS as usize;
        mask.iter()
            .enumerate()
            .flat_map(|(word, value)| {
                (0..bits)
                    .filter(move |bit| value & (1usize << bit) != 0)
                    .map(move |bit| word * bits + bit)
            })
            .collect()
    }

    /// Encode as a kernel affinity mask of at least `min_words` words.
    pub(crate) fn to_mask(&self, min_words: usize) -> Vec<usize> {
        let bits = usize::BITS as usize;
        let words = self.last().map_or(0, |max| max / bits + 1).max(min_words);
        let mut mask = vec![0usize; words];
        for cpu in self.iter() {
            mask[cpu / bits] |= 1usize << (cpu % bits);
        }
        mask
    }
}

impl FromIterator<usize> for CpuSet {
    fn from_iter<I: IntoIterator<Item = usize>>(iter: I) -> Self {
        Self {
            cpus: iter.into_iter().collect(),
        }
    }
}

impl Extend<usize> for CpuSet {
    fn extend<I: IntoIterator<Item = usize>>(&mut self, iter: I) {
        self.cpus.extend(iter);
    }
}

impl<'a> IntoIterator for &'a CpuSet {
    type Item = usize;
    type IntoIter = std::iter::Copied<std::collections::btree_set::Iter<'a, usize>>;

    fn into_iter(self) -> Self::IntoIter {
        self.cpus.iter().copied()
    }
}

impl IntoIterator for CpuSet {
    type Item = usize;
    type IntoIter = std::collections::btree_set::IntoIter<usize>;

    fn into_iter(self) -> Self::IntoIter {
        self.cpus.into_iter()
    }
}

impl fmt::Display for CpuSet {
    /// Format as a Linux CPU list (`0-3,8`). The empty set formats as `""`.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let mut cpus = self.iter().peekable();
        let mut first = true;
        while let Some(start) = cpus.next() {
            let mut end = start;
            while cpus.peek() == Some(&(end + 1)) {
                end += 1;
                cpus.next();
            }
            if !first {
                f.write_str(",")?;
            }
            first = false;
            if start == end {
                write!(f, "{start}")?;
            } else {
                write!(f, "{start}-{end}")?;
            }
        }
        Ok(())
    }
}

impl FromStr for CpuSet {
    type Err = Error;

    /// Parse a Linux CPU list such as `0-3,8,10-11`.
    ///
    /// Surrounding whitespace is ignored and an empty string is the empty
    /// set. Reversed ranges, empty elements and CPU numbers above
    /// [`CpuSet::MAX_CPU`] fail with [`ErrorKind::InvalidConfiguration`].
    fn from_str(value: &str) -> Result<Self, Error> {
        let invalid = || Error::new(ErrorKind::InvalidConfiguration).with_context("CPU list");
        let value = value.trim();
        let mut set = CpuSet::new();
        if value.is_empty() {
            return Ok(set);
        }
        for part in value.split(',') {
            let (start, end) = part.split_once('-').unwrap_or((part, part));
            let start = start.parse::<usize>().map_err(|_| invalid())?;
            let end = end.parse::<usize>().map_err(|_| invalid())?;
            if start > end || end > Self::MAX_CPU {
                return Err(invalid());
            }
            set.extend(start..=end);
        }
        Ok(set)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn set(cpus: &[usize]) -> CpuSet {
        cpus.iter().copied().collect()
    }

    #[test]
    fn parses_linux_cpu_lists() {
        for value in ["", " \n"] {
            assert_eq!(value.parse::<CpuSet>().unwrap(), CpuSet::new());
        }
        assert_eq!(
            " 3,1-3,2,0,1048575\n".parse::<CpuSet>().unwrap(),
            set(&[0, 1, 2, 3, 1_048_575])
        );
        assert_eq!(
            "1-3,8,10-11\n".parse::<CpuSet>().unwrap(),
            set(&[1, 2, 3, 8, 10, 11])
        );
        for value in [
            "4-1",
            "-1",
            "a",
            "1-2-3",
            "1048576",
            "999999999",
            "1,,2",
            "1,",
        ] {
            let error = value.parse::<CpuSet>().unwrap_err();
            assert_eq!(error.kind(), ErrorKind::InvalidConfiguration, "{value}");
        }
    }

    #[test]
    fn display_round_trips() {
        for (cpus, text) in [
            (set(&[]), ""),
            (set(&[5]), "5"),
            (set(&[0, 1, 2, 3, 8, 10, 11]), "0-3,8,10-11"),
            (set(&[1, 3, 5]), "1,3,5"),
        ] {
            assert_eq!(cpus.to_string(), text);
            assert_eq!(text.parse::<CpuSet>().unwrap(), cpus);
        }
    }

    #[test]
    fn masks_round_trip() {
        let cpus = set(&[0, 1, 63, 64, 130]);
        let mask = cpus.to_mask(1);
        assert_eq!(mask.len(), 130 / usize::BITS as usize + 1);
        assert_eq!(CpuSet::from_mask(&mask), cpus);
        assert_eq!(CpuSet::new().to_mask(4), vec![0; 4]);
    }

    #[test]
    fn set_operations() {
        let mut a = set(&[1, 2, 3]);
        let b = set(&[2, 3, 4]);
        assert_eq!(a.intersection(&b), set(&[2, 3]));
        assert!(set(&[2]).is_subset(&a));
        assert!(a.insert(9));
        assert!(!a.insert(9));
        assert!(a.remove(1));
        a.retain(|cpu| cpu != 2);
        assert_eq!(a.iter().collect::<Vec<_>>(), [3, 9]);
        assert_eq!((a.first(), a.last(), a.len()), (Some(3), Some(9), 2));
        assert_eq!((&a).into_iter().rev().collect::<Vec<_>>(), [9, 3]);
    }
}
