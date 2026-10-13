use std::ffi::CString;
use std::fs::{self, File};
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::atomic::{AtomicU64, Ordering};
use uds_endpoint::publication::{Publication, Replacement};
use uds_endpoint::{
    BoundSocket, EndpointOwner, Layout, file_path, open_directory, restrict_directory,
};

struct Root(PathBuf);

impl Root {
    fn new() -> Self {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let root = Self(PathBuf::from(env!("CARGO_TARGET_TMPDIR")).join(format!(
            "boundary-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        )));
        fs::create_dir_all(&root.0).unwrap();
        fs::set_permissions(&root.0, fs::Permissions::from_mode(0o700)).unwrap();
        root
    }
}

impl Drop for Root {
    fn drop(&mut self) {
        if let Err(error) = fs::remove_dir_all(&self.0) {
            if std::thread::panicking() {
                eprintln!("fixture cleanup failed: {error}");
            } else {
                panic!("fixture cleanup failed: {error}");
            }
        }
    }
}

fn layout() -> Layout {
    Layout::new("lock", "socket", "owned-", |n| n.starts_with("pending-")).unwrap()
}

fn acl(uid: u32, owner: u16, mask: u16) -> Vec<u8> {
    // Linux POSIX ACL xattr version 2, followed by little-endian tag/perm/id entries.
    let mut bytes = 2_u32.to_le_bytes().to_vec();
    for (tag, permissions, id) in [
        (0x01_u16, owner, u32::MAX), // ACL_USER_OBJ
        (0x02, 0o6, uid),            // ACL_USER
        (0x04, 0, u32::MAX),         // ACL_GROUP_OBJ
        (0x10, mask, u32::MAX),      // ACL_MASK
        (0x20, 0, u32::MAX),         // ACL_OTHER
    ] {
        bytes.extend_from_slice(&tag.to_le_bytes());
        bytes.extend_from_slice(&permissions.to_le_bytes());
        bytes.extend_from_slice(&id.to_le_bytes());
    }
    bytes
}

fn read_acl(path: &std::path::Path, name: &std::ffi::CStr) -> Vec<u8> {
    let path = CString::new(path.as_os_str().as_encoded_bytes()).unwrap();
    let mut bytes = vec![0_u8; 256];
    // SAFETY: both strings are terminated; bytes is writable for its supplied length.
    let size = unsafe {
        libc::getxattr(
            path.as_ptr(),
            name.as_ptr(),
            bytes.as_mut_ptr().cast(),
            bytes.len(),
        )
    };
    assert!(size >= 0, "getxattr: {}", io::Error::last_os_error());
    bytes.truncate(size as usize);
    bytes
}

#[test]
fn default_acl_survives_hardening_and_socket_chmod_reopens_its_mask() {
    let root = Root::new();
    let directory = open_directory(&root.0, 0o700).unwrap();
    let uid = if directory.metadata().unwrap().uid() == 65534 {
        65533
    } else {
        65534
    };
    let default = acl(uid, 0o7, 0o7);
    // SAFETY: the directory is open; name and value remain valid for the call.
    let result = unsafe {
        libc::fsetxattr(
            directory.as_raw_fd(),
            c"system.posix_acl_default".as_ptr(),
            default.as_ptr().cast(),
            default.len(),
            0,
        )
    };
    if result != 0 {
        let error = io::Error::last_os_error();
        if error.raw_os_error() == Some(libc::EOPNOTSUPP) {
            eprintln!("skipping ACL regression: fixture filesystem lacks POSIX ACLs");
            return;
        }
        panic!("fsetxattr: {error}");
    }
    directory
        .set_permissions(fs::Permissions::from_mode(0o777))
        .unwrap();
    restrict_directory(&directory, 0o022).unwrap();
    assert_eq!(directory.metadata().unwrap().mode() & 0o777, 0o755);
    assert_eq!(read_acl(&root.0, c"system.posix_acl_default"), default);

    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    let bound = BoundSocket::bind(owner, "pending-a").unwrap();
    let socket = file_path(&directory).join("pending-a");
    for (mode, mask) in [(0o600, 0), (0o660, 0o6), (0o600, 0)] {
        bound.set_mode(mode).unwrap();
        assert_eq!(fs::metadata(&socket).unwrap().mode() & 0o777, mode);
        // The named user's rw entry persists; chmod changes its effective mask.
        assert_eq!(
            read_acl(&socket, c"system.posix_acl_access"),
            acl(uid, 0o6, mask)
        );
    }
    assert_eq!(read_acl(&root.0, c"system.posix_acl_default"), default);
}

#[test]
fn writable_ancestor_can_redirect_clients_but_not_pinned_operations() {
    let root = Root::new();
    let ancestor = File::open(&root.0).unwrap();
    ancestor
        .set_permissions(fs::Permissions::from_mode(0o777))
        .unwrap();
    let directory = open_directory(&root.0.join("child"), 0o755).unwrap();
    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    let bound = Rc::new(BoundSocket::bind(owner.clone(), "pending-a").unwrap());
    let mut publication = Publication::default();
    publication
        .publish(
            Replacement::prepare(bound.clone(), None, "pending-a".into(), "socket".into()).unwrap(),
        )
        .unwrap();
    publication.commit();
    drop(publication);

    fs::rename(root.0.join("child"), root.0.join("moved")).unwrap();
    fs::create_dir(root.0.join("child")).unwrap();
    // Pin only the fixture ancestor to keep sockaddr_un short. The client still
    // resolves the replaceable child/socket pathname, not the owned directory fd.
    let client_path = file_path(&ancestor).join("child/socket");
    let replacement = UnixListener::bind(&client_path).unwrap();
    replacement.set_nonblocking(true).unwrap();
    fs::set_permissions(&client_path, fs::Permissions::from_mode(0o600)).unwrap();
    owner.validate(&directory).unwrap();
    let _redirected = UnixStream::connect(&client_path).unwrap();
    replacement.accept().unwrap();
    assert_eq!(
        bound.accept().unwrap_err().kind(),
        io::ErrorKind::WouldBlock
    );

    let _original = UnixStream::connect(file_path(&directory).join("socket")).unwrap();
    bound.accept().unwrap();
    bound.set_mode(0o660).unwrap();
    assert_eq!(fs::metadata(&client_path).unwrap().mode() & 0o777, 0o600);
    assert_eq!(
        fs::metadata(file_path(&directory).join("socket"))
            .unwrap()
            .mode()
            & 0o777,
        0o660
    );
    drop(bound);
    assert!(!root.0.join("moved/socket").exists());
    assert!(!root.0.join("moved/owned-pending-a").exists());
    let _still_redirected = UnixStream::connect(&client_path).unwrap();
    replacement.accept().unwrap();
}
