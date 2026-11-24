//! Node-local origin endpoint directory: /run/racer/<cache name>/origin.
//!
//! The privileged dataplane creates this directory and assigns it to the
//! unprivileged origin user so origins never need root or `mkdir` rights in
//! /run/racer. The origin binds its socket here (pkg/racersdk ServeOrigin).
use std::fs::File;
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::fs::MetadataExt;

/// UID that Racer origins run as. Keep in sync with the nonroot user of
/// images/racer-loadgen/Containerfile and with the origin directory checks in
/// cmd/racer-loadgen/origin_uds.go (checkOriginDirectory).
pub const ORIGIN_UID: u32 = 65532;
/// GID that Racer origins run as. See [`ORIGIN_UID`].
pub const ORIGIN_GID: u32 = 65532;
/// Only the origin user may list, create, or remove origin endpoint entries.
pub const ORIGIN_MODE: u32 = 0o700;

/// Target owner of the origin directory.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Owner {
    pub uid: u32,
    pub gid: u32,
}

impl Owner {
    /// Production origin identity.
    pub const ORIGIN: Self = Self {
        uid: ORIGIN_UID,
        gid: ORIGIN_GID,
    };
}

impl std::fmt::Display for Owner {
    /// `uid=<uid> gid=<gid>`, the form diagnostics log.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "uid={} gid={}", self.uid, self.gid)
    }
}

/// What [`prepare`] changed. Exposed for tests and diagnostics.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Outcome {
    /// The directory owner or group was changed.
    pub chowned: bool,
    /// The directory mode was changed.
    pub chmodded: bool,
    /// Ownership could not be assigned because the dataplane lacks privilege.
    pub unprivileged: bool,
}

/// Changes made by [`prepare`]. A seam so tests can check the order.
trait Ops {
    fn chmod(&mut self, directory: &File, mode: u32) -> io::Result<()>;
    fn chown(&mut self, directory: &File, owner: Owner) -> io::Result<()>;
}

struct System;

impl Ops for System {
    fn chmod(&mut self, directory: &File, mode: u32) -> io::Result<()> {
        // SAFETY: directory retains its descriptor throughout fchmod.
        check(unsafe { libc::fchmod(directory.as_raw_fd(), mode) })
    }

    fn chown(&mut self, directory: &File, owner: Owner) -> io::Result<()> {
        // SAFETY: directory retains its descriptor throughout fchown.
        check(unsafe { libc::fchown(directory.as_raw_fd(), owner.uid, owner.gid) })
    }
}

fn check(result: libc::c_int) -> io::Result<()> {
    if result == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

/// Create or repair `<cache>/origin` so it is a real directory owned by
/// `owner` with mode [`ORIGIN_MODE`].
///
/// The directory is opened with `O_DIRECTORY | O_NOFOLLOW`, so a symlink or
/// non-directory at that name is rejected, and every change is applied
/// through the pinned descriptor.
///
/// The mode is set before the handoff, so the origin user gets no control
/// until the directory is already private. If a run stops before the handoff,
/// the directory keeps its old owner, and the next run repeats the remaining
/// steps. That retry also recovers a first start interrupted between creation
/// and handoff, which is the only recovery a directory created by this version
/// needs.
///
/// # Errors
/// Returns the open/mkdir/stat/chmod error, or a chown error other than an
/// unprivileged `EPERM`.
pub fn prepare(cache: &File, owner: Owner) -> io::Result<Outcome> {
    let privileged = effective_uid() == 0;
    prepare_with(cache, owner, privileged, &mut System)
}

/// `privileged` says whether the dataplane can assign any owner. It is a
/// parameter so tests can run the root path without root.
fn prepare_with(
    cache: &File,
    owner: Owner,
    privileged: bool,
    ops: &mut impl Ops,
) -> io::Result<Outcome> {
    let directory = crate::uds_endpoint::child_directory(cache, b"origin", ORIGIN_MODE)?;
    let before = directory.metadata()?;
    if !before.is_dir() {
        return Err(io::Error::from_raw_os_error(libc::ENOTDIR));
    }
    let mut outcome = Outcome::default();
    if before.mode() & 0o7777 != ORIGIN_MODE {
        ops.chmod(&directory, ORIGIN_MODE)?;
        outcome.chmodded = true;
    }
    if before.uid() != owner.uid || before.gid() != owner.gid {
        match ops.chown(&directory, owner) {
            Ok(()) => outcome.chowned = true,
            // The dataplane runs as root in production. Without root (for
            // example local tests), keep the directory usable by an origin
            // running as the dataplane's own user instead of failing.
            Err(error) if error.raw_os_error() == Some(libc::EPERM) && !privileged => {
                eprintln!(
                    "racer: stage=origin-directory warning=unprivileged \
                     {owner} error={error}"
                );
                outcome.unprivileged = true;
            }
            Err(error) => return Err(error),
        }
    }
    Ok(outcome)
}

fn effective_uid() -> u32 {
    // SAFETY: geteuid has no preconditions.
    unsafe { libc::geteuid() }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::os::unix::fs::PermissionsExt;

    struct Scratch(std::path::PathBuf);
    impl Scratch {
        fn new() -> Self {
            // Stay inside the worktree and keep socket paths under SUN_LEN.
            static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
            let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(format!(
                ".odt-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
            ));
            fs::create_dir(&path).unwrap();
            Self(path)
        }
        fn open(&self) -> File {
            File::open(&self.0).unwrap()
        }
    }
    impl Drop for Scratch {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn me() -> Owner {
        // SAFETY: no preconditions.
        Owner {
            uid: unsafe { libc::geteuid() },
            gid: unsafe { libc::getegid() },
        }
    }

    #[test]
    fn owner_displays_as_log_fields() {
        assert_eq!(Owner { uid: 4242, gid: 7 }.to_string(), "uid=4242 gid=7");
    }

    #[test]
    fn creates_private_directory() {
        let scratch = Scratch::new();
        let outcome = prepare(&scratch.open(), me()).unwrap();
        let meta = fs::symlink_metadata(scratch.0.join("origin")).unwrap();
        assert!(meta.is_dir());
        assert_eq!(meta.mode() & 0o7777, ORIGIN_MODE);
        assert_eq!((meta.uid(), meta.gid()), (me().uid, me().gid));
        assert!(!outcome.chowned && !outcome.chmodded);
        // Idempotent: second call changes nothing.
        assert_eq!(prepare(&scratch.open(), me()).unwrap(), Outcome::default());
    }

    #[test]
    fn repairs_mode_and_keeps_entries() {
        for mode in [0o755, 0o777, 0o1777, 0o2700, 0o500] {
            let scratch = Scratch::new();
            let origin = scratch.0.join("origin");
            fs::create_dir(&origin).unwrap();
            fs::write(origin.join("socket"), b"keep").unwrap();
            fs::set_permissions(&origin, fs::Permissions::from_mode(mode)).unwrap();
            let outcome = prepare(&scratch.open(), me()).unwrap();
            assert!(outcome.chmodded);
            assert_eq!(fs::metadata(&origin).unwrap().mode() & 0o7777, ORIGIN_MODE);
            assert_eq!(fs::read(origin.join("socket")).unwrap(), b"keep");
        }
    }

    #[test]
    fn rejects_symlink_and_non_directory() {
        let scratch = Scratch::new();
        let target = scratch.0.join("target");
        fs::create_dir(&target).unwrap();
        fs::set_permissions(&target, fs::Permissions::from_mode(0o777)).unwrap();
        std::os::unix::fs::symlink(&target, scratch.0.join("origin")).unwrap();
        assert!(prepare(&scratch.open(), me()).is_err());
        assert_eq!(fs::metadata(&target).unwrap().mode() & 0o7777, 0o777);

        let scratch = Scratch::new();
        fs::write(scratch.0.join("origin"), b"file").unwrap();
        assert!(prepare(&scratch.open(), me()).is_err());
        assert_eq!(fs::read(scratch.0.join("origin")).unwrap(), b"file");
    }

    #[test]
    fn unprivileged_chown_is_tolerated() {
        if effective_uid() == 0 {
            return;
        }
        let scratch = Scratch::new();
        let outcome = prepare(&scratch.open(), Owner::ORIGIN).unwrap();
        assert!(outcome.unprivileged && !outcome.chowned);
        let meta = fs::metadata(scratch.0.join("origin")).unwrap();
        assert_eq!(meta.mode() & 0o7777, ORIGIN_MODE);
        assert_eq!(meta.uid(), effective_uid());
    }

    /// Records each change and the directory state seen at that moment.
    /// Chmod is applied so later steps see the new mode. Chown calls are only
    /// recorded, so these tests do not need root.
    #[derive(Debug, PartialEq, Eq)]
    enum Event {
        Chmod(u32, u32),
        Chown(u32, u32),
    }

    struct Recorder {
        events: Vec<Event>,
        chown_error: Option<i32>,
    }

    impl Recorder {
        fn new() -> Self {
            Self {
                events: Vec::new(),
                chown_error: None,
            }
        }
    }

    fn state(directory: &File) -> (u32, u32) {
        let meta = directory.metadata().unwrap();
        (meta.uid(), meta.mode() & 0o7777)
    }

    impl Ops for Recorder {
        fn chmod(&mut self, directory: &File, mode: u32) -> io::Result<()> {
            let (uid, before) = state(directory);
            self.events.push(Event::Chmod(uid, before));
            System.chmod(directory, mode)
        }

        fn chown(&mut self, directory: &File, _: Owner) -> io::Result<()> {
            let (uid, mode) = state(directory);
            self.events.push(Event::Chown(uid, mode));
            match self.chown_error {
                Some(code) => Err(io::Error::from_raw_os_error(code)),
                None => Ok(()),
            }
        }
    }

    const TARGET: Owner = Owner {
        uid: 4242,
        gid: 4242,
    };

    fn existing(scratch: &Scratch, mode: u32) -> std::path::PathBuf {
        let origin = scratch.0.join("origin");
        fs::create_dir(&origin).unwrap();
        fs::set_permissions(&origin, fs::Permissions::from_mode(mode)).unwrap();
        origin
    }

    #[test]
    fn narrows_mode_before_handoff() {
        let scratch = Scratch::new();
        existing(&scratch, 0o755);
        let euid = effective_uid();
        let mut ops = Recorder::new();
        let outcome = prepare_with(&scratch.open(), TARGET, true, &mut ops).unwrap();
        assert_eq!(
            ops.events,
            vec![Event::Chmod(euid, 0o755), Event::Chown(euid, ORIGIN_MODE)]
        );
        assert_eq!(
            outcome,
            Outcome {
                chowned: true,
                chmodded: true,
                unprivileged: false,
            }
        );
    }

    #[test]
    fn privileged_eperm_is_fatal() {
        let scratch = Scratch::new();
        existing(&scratch, 0o700);
        let mut ops = Recorder::new();
        ops.chown_error = Some(libc::EPERM);
        assert!(prepare_with(&scratch.open(), TARGET, true, &mut ops).is_err());
    }
}
