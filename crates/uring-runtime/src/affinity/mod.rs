//! CPU topology discovery and calling-thread CPU affinity.
//!
//! This module answers two questions and nothing more:
//!
//! - Which logical CPUs may the calling thread use, and how are they laid out
//!   (package, core, die, NUMA node)? See [`Topology`].
//! - How do I restrict the calling thread to a set of CPUs? See
//!   [`current_cpus`], [`set_cpus`] and [`pin_cpu`].
//!
//! It deliberately contains no placement policy: deciding how many lanes to
//! run, which CPUs to give them, or whether to share SMT siblings is up to the
//! application. [`Topology`] provides the inputs such a policy needs.
//!
//! Affinity functions affect only the calling thread, never the whole process.
//!
//! ```
//! use uring_runtime::affinity::{CpuSet, current_cpus, pin_cpu, set_cpus};
//!
//! # fn main() -> uring_runtime::Result<()> {
//! std::thread::spawn(|| -> uring_runtime::Result<()> {
//!     let allowed = current_cpus()?;
//!     let first = allowed.first().expect("at least one allowed CPU");
//!     pin_cpu(first)?;
//!     assert_eq!(current_cpus()?, CpuSet::from_iter([first]));
//!     set_cpus(&allowed)?;
//!     Ok(())
//! })
//! .join()
//! .unwrap()?;
//! # Ok(())
//! # }
//! ```

mod cgroup;
mod cpuset;
mod topology;

pub use cpuset::CpuSet;
pub use topology::{CpuLocation, CpuQuota, DeviceLocality, Topology};

use crate::{Error, ErrorKind, Result};

/// Initial `sched_getaffinity` mask size in machine words (1024 CPUs on 64-bit).
const INITIAL_MASK_WORDS: usize = 16;

/// Largest mask `current_cpus` will try before giving up, in machine words.
const MAX_MASK_WORDS: usize = (CpuSet::MAX_CPU + 1) / usize::BITS as usize;

/// Return the set of logical CPUs the calling thread may run on.
///
/// # Errors
///
/// Returns the `sched_getaffinity` errno if the kernel rejects every mask size
/// up to [`CpuSet::MAX_CPU`].
pub fn current_cpus() -> Result<CpuSet> {
    let mut mask = vec![0usize; INITIAL_MASK_WORDS];
    loop {
        // SAFETY: the kernel writes at most `size_of_val(mask)` bytes into the
        // writable, word-aligned buffer, which outlives the call.
        let result = unsafe {
            libc::sched_getaffinity(
                0,
                std::mem::size_of_val(mask.as_slice()),
                mask.as_mut_ptr().cast(),
            )
        };
        if result == 0 {
            break;
        }
        let error = Error::last_os_error();
        // EINVAL means the mask is smaller than the kernel's CPU mask.
        if error.raw_os_error() != Some(libc::EINVAL) || mask.len() >= MAX_MASK_WORDS {
            return Err(error.with_context("sched_getaffinity"));
        }
        mask.resize(mask.len() * 2, 0);
    }
    Ok(CpuSet::from_mask(&mask))
}

/// Restrict the calling thread to the given logical CPUs.
///
/// Other threads, including ones spawned earlier, are not affected. Threads
/// spawned afterwards by the calling thread inherit the new mask.
///
/// # Errors
///
/// - [`ErrorKind::InvalidInput`] if `cpus` is empty or contains a CPU above
///   [`CpuSet::MAX_CPU`]. The affinity is left unchanged.
/// - The `sched_setaffinity` errno otherwise, for example `EINVAL` when none
///   of the CPUs is online and allowed by the thread's cpuset. The affinity is
///   left unchanged.
pub fn set_cpus(cpus: &CpuSet) -> Result<()> {
    let Some(max) = cpus.last() else {
        return Err(Error::new(ErrorKind::InvalidInput).with_context("empty CPU set"));
    };
    if max > CpuSet::MAX_CPU {
        return Err(Error::new(ErrorKind::InvalidInput).with_context("CPU number too large"));
    }
    let mask = cpus.to_mask(INITIAL_MASK_WORDS);
    // SAFETY: the kernel only reads `size_of_val(mask)` bytes from the
    // word-aligned buffer, which outlives the call.
    let result = unsafe {
        libc::sched_setaffinity(
            0,
            std::mem::size_of_val(mask.as_slice()),
            mask.as_ptr().cast(),
        )
    };
    if result != 0 {
        return Err(Error::last_os_error().with_context("sched_setaffinity"));
    }
    Ok(())
}

/// Restrict the calling thread to a single logical CPU.
///
/// Equivalent to [`set_cpus`] with a one-element set.
///
/// # Errors
///
/// See [`set_cpus`].
pub fn pin_cpu(cpu: usize) -> Result<()> {
    set_cpus(&CpuSet::from_iter([cpu]))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    #[cfg_attr(miri, ignore = "affinity syscalls are not supported by Miri")]
    fn pin_and_restore_on_the_calling_thread() {
        let outer = current_cpus().unwrap();
        std::thread::spawn(|| {
            let allowed = current_cpus().unwrap();
            let cpu = allowed.first().unwrap();
            pin_cpu(cpu).unwrap();
            assert_eq!(current_cpus().unwrap(), CpuSet::from_iter([cpu]));
            set_cpus(&allowed).unwrap();
            assert_eq!(current_cpus().unwrap(), allowed);
        })
        .join()
        .unwrap();
        // The spawning thread is never affected.
        assert_eq!(current_cpus().unwrap(), outer);
    }

    #[test]
    #[cfg_attr(miri, ignore = "affinity syscalls are not supported by Miri")]
    fn invalid_masks_fail_without_changing_affinity() {
        let allowed = current_cpus().unwrap();
        let error = set_cpus(&CpuSet::new()).unwrap_err();
        assert_eq!(error.kind(), ErrorKind::InvalidInput);
        let error = pin_cpu(CpuSet::MAX_CPU + 1).unwrap_err();
        assert_eq!(error.kind(), ErrorKind::InvalidInput);
        assert_eq!(current_cpus().unwrap(), allowed);
    }
}
