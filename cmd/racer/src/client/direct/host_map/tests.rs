use super::*;
use std::io::Write;
use std::os::fd::FromRawFd;

fn memfd(size: usize, seals: libc::c_int) -> std::fs::File {
    let fd = unsafe {
        libc::memfd_create(
            c"racer-host-map".as_ptr(),
            libc::MFD_CLOEXEC | libc::MFD_ALLOW_SEALING,
        )
    };
    assert!(fd >= 0);
    let mut file = unsafe { std::fs::File::from_raw_fd(fd) };
    file.write_all(&vec![0; size]).unwrap();
    if seals != 0 {
        assert_eq!(unsafe { libc::fcntl(fd, libc::F_ADD_SEALS, seals) }, 0);
    }
    file
}

fn window(offset: u64, length: u64) -> Destination {
    Destination { offset, length }
}

#[test]
fn maps_only_shrink_sealed_read_write_memfds_that_cover_the_window() {
    let sealed = memfd(10_000, libc::F_SEAL_SHRINK);
    assert!(HostMapping::new(&sealed, window(0, 10_000)).is_some());
    assert!(HostMapping::new(&sealed, window(4097, 5000)).is_some());
    assert!(
        HostMapping::new(&sealed, window(5000, 5001)).is_none(),
        "past the end"
    );
    assert!(HostMapping::new(&sealed, window(0, 0)).is_none(), "empty");

    let unsealed = memfd(10_000, 0);
    assert!(HostMapping::new(&unsealed, window(0, 100)).is_none());

    let write_sealed = memfd(10_000, libc::F_SEAL_SHRINK | libc::F_SEAL_WRITE);
    assert!(HostMapping::new(&write_sealed, window(0, 100)).is_none());

    let future_write = memfd(10_000, libc::F_SEAL_SHRINK | libc::F_SEAL_FUTURE_WRITE);
    assert!(HostMapping::new(&future_write, window(0, 100)).is_none());

    let path = format!("/proc/self/fd/{}", sealed.as_raw_fd());
    let write_only = std::fs::OpenOptions::new().write(true).open(&path).unwrap();
    assert!(HostMapping::new(&write_only, window(0, 100)).is_none());
}

#[test]
fn does_not_map_files_outside_tmpfs() {
    // The crate directory is on a disk filesystem, never tmpfs.
    let file = std::fs::File::open(env!("CARGO_MANIFEST_DIR").to_string() + "/Cargo.toml").unwrap();
    assert!(HostMapping::new(&file, window(0, 1)).is_none());
}

#[test]
fn bytes_stay_inside_the_window_and_reach_the_file() {
    use std::os::unix::fs::FileExt;
    let file = memfd(3 * 4096, libc::F_SEAL_SHRINK);
    let mapping = HostMapping::new(&file, window(5000, 3000)).unwrap();
    unsafe {
        assert!(mapping.bytes_mut(4999, 1).is_err());
        assert!(mapping.bytes_mut(7999, 2).is_err());
        assert!(mapping.bytes_mut(u64::MAX, 2).is_err());
        let bytes = mapping.bytes_mut(5000, 3000).unwrap();
        populate(bytes).unwrap();
        bytes.fill(7);
    }
    drop(mapping);
    let mut contents = vec![0; 3 * 4096];
    file.read_exact_at(&mut contents, 0).unwrap();
    assert!(contents[..5000].iter().all(|&b| b == 0));
    assert!(contents[5000..8000].iter().all(|&b| b == 7));
    assert!(contents[8000..].iter().all(|&b| b == 0));
}
