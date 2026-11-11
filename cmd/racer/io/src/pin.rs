//! CPU affinity for the calling thread. Topology discovery lives in `racer`.
use crate::{Error, Result};
use std::collections::BTreeSet;

/// Largest logical CPU number accepted by [`set_cpus`].
pub const MAX_CPU: usize = 1_048_575;

/// The calling thread's allowed logical CPUs.
pub fn current_cpus() -> Result<BTreeSet<usize>> {
    let mut mask = vec![0usize; 16];
    loop {
        // SAFETY: the kernel receives the size of the writable, word-aligned mask.
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
        if std::io::Error::last_os_error().raw_os_error() != Some(libc::EINVAL)
            || mask.len() >= 16384
        {
            return Err(Error::last_os_error());
        }
        mask.resize(mask.len() * 2, 0);
    }
    Ok(cpus_of(&mask))
}

/// Restrict the calling thread to `cpus` (nonempty, each at most [`MAX_CPU`]).
pub fn set_cpus(cpus: &BTreeSet<usize>) -> Result<()> {
    let mask = mask_of(cpus)?;
    // SAFETY: the kernel only reads the sized, word-aligned affinity mask.
    if unsafe {
        libc::sched_setaffinity(
            0,
            std::mem::size_of_val(mask.as_slice()),
            mask.as_ptr().cast(),
        )
    } != 0
    {
        return Err(Error::last_os_error());
    }
    Ok(())
}

/// Pin the calling thread to one logical CPU.
pub fn pin_cpu(cpu: usize) -> Result<()> {
    set_cpus(&BTreeSet::from([cpu]))
}

/// Encode a CPU set as a kernel affinity mask.
fn mask_of(cpus: &BTreeSet<usize>) -> Result<Vec<usize>> {
    let max = *cpus.last().ok_or(Error::InvalidConfiguration)?;
    if max > MAX_CPU {
        return Err(Error::InvalidConfiguration);
    }
    let bits = usize::BITS as usize;
    let mut mask = vec![0usize; (max / bits + 1).max(16)];
    for cpu in cpus {
        mask[cpu / bits] |= 1usize << (cpu % bits);
    }
    Ok(mask)
}

/// Decode a kernel affinity mask.
fn cpus_of(mask: &[usize]) -> BTreeSet<usize> {
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn masks_round_trip_and_reject_bad_sets() {
        let cpus = BTreeSet::from([0, 3, 64, 1000]);
        assert_eq!(cpus_of(&mask_of(&cpus).unwrap()), cpus);
        assert_eq!(mask_of(&BTreeSet::new()), Err(Error::InvalidConfiguration));
        assert_eq!(
            mask_of(&BTreeSet::from([MAX_CPU + 1])),
            Err(Error::InvalidConfiguration)
        );
    }

    #[test]
    fn pinning_a_thread_restricts_and_restores_affinity() {
        std::thread::spawn(|| {
            let original = current_cpus().unwrap();
            let first = *original.first().unwrap();
            pin_cpu(first).unwrap();
            assert_eq!(current_cpus().unwrap(), BTreeSet::from([first]));
            set_cpus(&original).unwrap();
            assert_eq!(current_cpus().unwrap(), original);
        })
        .join()
        .unwrap();
    }
}
