use std::ffi::CString;
use std::io;
use std::os::unix::ffi::OsStrExt;
use std::path::Path;

use super::{Config, bad};
use crate::runtime::UpdateError;

/// An inotify watch on the *directory* holding the config file: delivery is a `rename(2)`
/// over the path, and a watch on the file would hold the old inode and go deaf after the
/// first push. `IN_MOVED_TO` is the rename landing, `IN_CLOSE_WRITE` an operator editing
/// in place.
pub(super) struct Watch {
    fd: libc::c_int,
    name: Vec<u8>,
}

impl Watch {
    pub(super) fn new(path: &Path) -> io::Result<Watch> {
        let dir = path
            .parent()
            .filter(|d| !d.as_os_str().is_empty())
            .unwrap_or(Path::new("."));
        let name = path
            .file_name()
            .ok_or_else(|| bad("config path has no file name"))?
            .as_bytes()
            .to_vec();
        let fd = unsafe { libc::inotify_init1(libc::IN_CLOEXEC) };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        let w = Watch { fd, name };
        let c = CString::new(dir.as_os_str().as_bytes())?;
        let mask = libc::IN_MOVED_TO | libc::IN_CLOSE_WRITE;
        if unsafe { libc::inotify_add_watch(fd, c.as_ptr(), mask) } < 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(w)
    }

    /// Consume whatever is already queued, without blocking.
    fn drain(&self) -> io::Result<()> {
        let mut buf = [0u64; 512];
        loop {
            let mut p = libc::pollfd {
                fd: self.fd,
                events: libc::POLLIN,
                revents: 0,
            };
            let ready = unsafe { libc::poll(&mut p, 1, 0) };
            let n = match ready {
                0 => return Ok(()),
                r if r > 0 => unsafe {
                    libc::read(
                        self.fd,
                        buf.as_mut_ptr().cast(),
                        std::mem::size_of_val(&buf),
                    )
                },
                _ => -1,
            };
            if n < 0 {
                let e = io::Error::last_os_error();
                if e.kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                return Err(e);
            }
        }
    }

    /// Block until the watched file may have changed. Events for other names in the
    /// directory are consumed and ignored.
    pub(super) fn wait(&self) -> io::Result<()> {
        // u64-aligned so the event headers can be read in place.
        let mut buf = [0u64; 512];
        loop {
            let n = unsafe {
                libc::read(
                    self.fd,
                    buf.as_mut_ptr().cast(),
                    std::mem::size_of_val(&buf),
                )
            };
            if n < 0 {
                let e = io::Error::last_os_error();
                if e.kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                return Err(e);
            }
            let base = buf.as_ptr().cast::<u8>();
            let mut off = 0usize;
            let hdr = std::mem::size_of::<libc::inotify_event>();
            let mut hit = false;
            while off + hdr <= n as usize {
                let ev = unsafe { std::ptr::read(base.add(off).cast::<libc::inotify_event>()) };
                let name =
                    unsafe { std::slice::from_raw_parts(base.add(off + hdr), ev.len as usize) };
                // The name is NUL-padded to an alignment boundary.
                let name = &name[..name.iter().position(|&b| b == 0).unwrap_or(name.len())];
                hit |= name == self.name;
                off += hdr + ev.len as usize;
            }
            // The whole read is consumed before returning: a leftover event is never
            // reported again, and may be the only notice of a write that lands after the
            // caller reloaded.
            if hit {
                return Ok(());
            }
        }
    }
}

impl Drop for Watch {
    fn drop(&mut self) {
        unsafe { libc::close(self.fd) };
    }
}

/// Watch `path` and hand every delivered configuration to `apply`; never returns if healthy.
/// Candidate failures are rejected wholesale and counted. A runtime failure after commit is
/// fatal because continuing could leave file delivery and the dataplane on different states.
pub fn watch(
    path: &Path,
    mut apply: impl FnMut(Config) -> Result<bool, UpdateError>,
) -> io::Result<()> {
    let w = Watch::new(path)?;
    // inotify reports nothing from before the watch existed, and the caller loaded
    // the initial config before this thread ran, so a config published in that window would
    // be lost.
    // Read the file here instead. Draining before every read keeps the two in step: a
    // dropped event can only announce a file this read is about to see, and one this read
    // misses is still queued for the loop below.
    loop {
        w.drain()?;
        let Ok(next) = Config::load(path) else { break };
        match apply(next) {
            Ok(true) => {}
            Ok(false) => break,
            Err(UpdateError::Candidate(e)) => {
                reject(path, e);
                break;
            }
            Err(UpdateError::Runtime(e)) => return Err(e),
        }
    }
    loop {
        w.wait()?;
        let next = match Config::load(path) {
            Ok(c) => c,
            Err(e) => {
                reject(path, e);
                continue;
            }
        };
        match apply(next) {
            Ok(_) => {}
            Err(UpdateError::Candidate(e)) => reject(path, e),
            Err(UpdateError::Runtime(e)) => return Err(e),
        }
    }
}

fn reject(path: &Path, e: io::Error) {
    crate::kernel::add_counter(crate::kernel::Counter::ConfigRejected, 1);
    eprintln!("racer: rejected {}: {e}", path.display());
}
