//! Synchronous publication shares containment, permission, and write policy across
//! host and simulation. Only the descriptor/syscall boundary differs.
use super::*;
use std::{
    io,
    path::{Component, Path, PathBuf},
};

fn invalid() -> io::Error {
    io::Error::from_raw_os_error(libc::EINVAL)
}

fn validate_directory(path: &Path) -> io::Result<()> {
    secure::path_name(path.as_os_str(), PATH_BYTES).map_err(|_| invalid())?;
    if path
        .components()
        .any(|p| matches!(p, Component::ParentDir | Component::Prefix(_)))
    {
        return Err(invalid());
    }
    Ok(())
}

fn child(directory: &Path, path: &Path) -> io::Result<CString> {
    // Accept exactly directory.join(component), not nested or escaping paths.
    let relative = path.strip_prefix(directory).map_err(|_| invalid())?;
    secure::component(relative.as_os_str(), PATH_BYTES).map_err(|_| invalid())
}

struct Directory {
    fd: Descriptor,
    #[cfg(feature = "simulation")]
    simulated: Option<(simulation::Simulation, PathBuf)>,
}
impl Directory {
    fn open(path: &Path) -> io::Result<Self> {
        #[cfg(feature = "simulation")]
        if let Some(sim) = simulation::Simulation::current() {
            let mut current = if path.is_absolute() {
                PathBuf::from("/")
            } else {
                PathBuf::from(".")
            };
            let mut fd = sim.open(
                None,
                &current,
                libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW,
            )?;
            for part in path.components() {
                if let Component::Normal(part) = part {
                    current.push(part);
                    // No yielding occurs in this synchronous adapter. Existing
                    // symlinks fail O_NOFOLLOW before any traversal through them.
                    match sim.metadata(&current) {
                        Ok(_) => (),
                        Err(error) if error.kind() == io::ErrorKind::NotFound => {
                            sim.create_dir_all(&current)?;
                            sim.chmod(&current, 0o700)?;
                        }
                        Err(error) => return Err(error),
                    }
                    fd = sim.open(
                        Some(&fd),
                        Path::new(part),
                        libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW,
                    )?;
                }
            }
            return Ok(Self {
                fd,
                simulated: Some((sim, current)),
            });
        }
        let mut fd = host_open(
            libc::AT_FDCWD,
            if path.is_absolute() { c"/" } else { c"." },
            libc::O_RDONLY | libc::O_DIRECTORY,
            0,
        )?;
        for part in path.components() {
            if let Component::Normal(part) = part {
                let name = secure::component(part, PATH_BYTES).map_err(|_| invalid())?;
                // SAFETY: live pinned parent and NUL-terminated name.
                if unsafe { libc::mkdirat(fd.as_raw_fd(), name.as_ptr(), 0o700) } < 0 {
                    let error = io::Error::last_os_error();
                    if error.raw_os_error() != Some(libc::EEXIST) {
                        return Err(error);
                    }
                }
                fd = host_open(fd.as_raw_fd(), &name, libc::O_RDONLY | libc::O_DIRECTORY, 0)?;
            }
        }
        Ok(Self {
            fd,
            #[cfg(feature = "simulation")]
            simulated: None,
        })
    }
    fn create(&self, name: &CString) -> io::Result<Descriptor> {
        let flags =
            libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL | libc::O_CLOEXEC | libc::O_NOFOLLOW;
        #[cfg(feature = "simulation")]
        if let Some((sim, _)) = &self.simulated {
            return sim.open(
                Some(&self.fd),
                Path::new(std::ffi::OsStr::from_bytes(name.as_bytes())),
                flags,
            );
        }
        host_open(self.fd.as_raw_fd(), name, flags, 0o600)
    }
    fn rename(&self, from: &CString, to: &CString) -> io::Result<()> {
        #[cfg(feature = "simulation")]
        if let Some((sim, directory)) = &self.simulated {
            return sim.rename(
                &directory.join(std::ffi::OsStr::from_bytes(from.as_bytes())),
                &directory.join(std::ffi::OsStr::from_bytes(to.as_bytes())),
                0,
            );
        }
        // SAFETY: names and pinned parent stay live for this syscall.
        status(unsafe {
            libc::renameat(
                self.fd.as_raw_fd(),
                from.as_ptr(),
                self.fd.as_raw_fd(),
                to.as_ptr(),
            )
        })
    }
    fn unlink(&self, name: &CString) -> io::Result<()> {
        #[cfg(feature = "simulation")]
        if let Some((sim, directory)) = &self.simulated {
            return sim.unlink(&directory.join(std::ffi::OsStr::from_bytes(name.as_bytes())));
        }
        // SAFETY: name and pinned parent stay live for this syscall.
        status(unsafe { libc::unlinkat(self.fd.as_raw_fd(), name.as_ptr(), 0) })
    }
}

fn status(value: i32) -> io::Result<()> {
    if value < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(())
    }
}
fn host_open(
    dir: i32,
    name: &std::ffi::CStr,
    flags: i32,
    mode: libc::mode_t,
) -> io::Result<Descriptor> {
    // SAFETY: name is NUL-terminated and the successful FD is immediately owned.
    let fd = unsafe {
        libc::openat(
            dir,
            name.as_ptr(),
            flags | libc::O_CLOEXEC | libc::O_NOFOLLOW,
            mode,
        )
    };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(unsafe { Descriptor::from_raw_fd(fd) })
}
fn write(fd: &Descriptor, offset: usize, bytes: &[u8]) -> io::Result<usize> {
    #[cfg(feature = "simulation")]
    if let Some(handle) = fd.as_sim() {
        return handle.file_write(offset as u64, bytes);
    }
    let offset = libc::off_t::try_from(offset).map_err(|_| invalid())?;
    // SAFETY: the descriptor and read-only slice remain live across pwrite.
    let n = unsafe { libc::pwrite(fd.as_raw_fd(), bytes.as_ptr().cast(), bytes.len(), offset) };
    if n < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(n as usize)
    }
}

/// Blocking namespace-only replacement: do not call on latency-sensitive workers.
/// No fsync is issued. New directories use 0700 and stages 0600, both reduced by
/// the host umask (simulation models umask 0). Existing permissions are unchanged.
/// Target/candidates must be direct children of `directory`. Parent symlinks and
/// `..` are rejected. The caller supplies a finite candidate iterator and exclusively
/// controls the directory throughout publication and best-effort failure cleanup.
/// Pinned descriptor-relative host syscalls prevent parent-path substitution; no
/// claim is made against a writer allowed to replace entries in the pinned directory.
pub fn publish_new(
    directory: &Path,
    target: &Path,
    bytes: &[u8],
    candidates: impl IntoIterator<Item = PathBuf>,
) -> io::Result<()> {
    validate_directory(directory)?;
    let target = child(directory, target)?;
    // Validate all candidates before creating a directory or stage. The bound on
    // this collection is caller policy, as is the finite candidate iterator.
    let candidates: Vec<CString> = candidates
        .into_iter()
        .map(|p| {
            let name = child(directory, &p)?;
            if name == target {
                return Err(invalid());
            }
            Ok(name)
        })
        .collect::<io::Result<_>>()?;
    let directory = Directory::open(directory)?;
    for temporary in candidates {
        let file = match directory.create(&temporary) {
            Ok(file) => file,
            Err(error) if error.kind() == io::ErrorKind::AlreadyExists => continue,
            Err(error) => return Err(error),
        };
        let result = (|| {
            let mut offset = 0;
            while offset < bytes.len() {
                match write(&file, offset, &bytes[offset..]) {
                    Ok(0) => {
                        return Err(io::Error::new(io::ErrorKind::WriteZero, "zero stage write"));
                    }
                    Ok(n) => offset += n,
                    Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                    Err(e) => return Err(e),
                }
            }
            directory.rename(&temporary, &target)
        })();
        if result.is_err() {
            let _ = directory.unlink(&temporary);
        }
        return result;
    }
    Err(io::Error::new(
        io::ErrorKind::AlreadyExists,
        "stage candidates exhausted",
    ))
}
