//! Client directory hardening stays confined to the owned, pinned inode.
use super::*;

#[test]
fn startup_hardens_existing_client_directory_without_changing_ancestors() {
    for mode in [0o777, 0o775, 0o757, 0o770, 0o722, 0o2775, 0o1777] {
        let fixture = Fixture::new();
        let path = fixture.socket().parent().unwrap().to_owned();
        fs::create_dir_all(&path).unwrap();
        let cache = path.parent().unwrap();
        let origin = cache.join("origin");
        fs::create_dir(&origin).unwrap();
        for ancestor in [&fixture.root.0, cache, &origin] {
            fs::set_permissions(ancestor, fs::Permissions::from_mode(0o777)).unwrap();
        }
        fs::set_permissions(&path, fs::Permissions::from_mode(mode)).unwrap();
        let before = fs::metadata(&path).unwrap();
        fixture.reconcile(&[definition()]).unwrap();
        let after = fs::metadata(&path).unwrap();
        assert!(same_inode(&before, &after));
        assert_eq!((after.uid(), after.gid()), (before.uid(), before.gid()));
        assert_eq!(after.mode() & 0o7777, mode & !0o022);
        for ancestor in [&fixture.root.0, cache, &origin] {
            assert_eq!(fs::metadata(ancestor).unwrap().mode() & 0o7777, 0o777);
        }
        assert_eq!(
            fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
            0o666
        );
        let mut socket = fixture.connect();
        socket
            .write_all(&request("HEAD", "Connection: close\r\n"))
            .unwrap();
        assert!(
            fixture
                .receive(&mut socket, true)
                .starts_with(b"HTTP/1.1 200")
        );
    }
}

#[test]
fn secure_client_directory_permissions_are_unchanged() {
    for mode in [0o755, 0o750, 0o700, 0o500, 0o2750, 0o1700] {
        let root = Root::new();
        let directory = open_directory(&root.0).unwrap();
        directory
            .set_permissions(fs::Permissions::from_mode(mode))
            .unwrap();
        let before = directory.metadata().unwrap();
        prepare_client_directory(&directory).unwrap();
        prepare_client_directory(&directory).unwrap();
        let after = directory.metadata().unwrap();
        assert!(same_inode(&before, &after));
        assert_eq!((after.uid(), after.gid()), (before.uid(), before.gid()));
        assert_eq!(after.mode() & 0o7777, mode);
        assert_eq!(
            (after.ctime(), after.ctime_nsec()),
            (before.ctime(), before.ctime_nsec())
        );
        directory
            .set_permissions(fs::Permissions::from_mode(0o700))
            .unwrap();
    }
}

#[test]
fn client_directory_preparation_pins_inode_across_path_replacement() {
    for symlink in [false, true] {
        let root = Root::new();
        let parent = open_directory(&root.0).unwrap();
        let directory = child_directory(&parent, b"client").unwrap();
        directory
            .set_permissions(fs::Permissions::from_mode(0o777))
            .unwrap();
        let path = root.0.join("client");
        let moved = root.0.join("moved");
        let target = root.0.join("target");
        fs::create_dir(&target).unwrap();
        fs::set_permissions(&target, fs::Permissions::from_mode(0o777)).unwrap();
        fs::rename(&path, &moved).unwrap();
        if symlink {
            std::os::unix::fs::symlink(&target, &path).unwrap();
            assert!(child_directory(&parent, b"client").is_err());
        } else {
            fs::create_dir(&path).unwrap();
            fs::set_permissions(&path, fs::Permissions::from_mode(0o777)).unwrap();
        }
        prepare_client_directory(&directory).unwrap();
        assert_eq!(fs::metadata(&moved).unwrap().mode() & 0o7777, 0o755);
        assert_eq!(fs::metadata(&path).unwrap().mode() & 0o7777, 0o777);
        assert_eq!(fs::metadata(&target).unwrap().mode() & 0o7777, 0o777);
    }
}

#[test]
fn client_directory_preparation_rejects_non_directories_and_chmod_failure() {
    let root = Root::new();
    let path = root.0.join("file");
    fs::write(&path, b"preserve").unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o666)).unwrap();
    assert_eq!(
        prepare_client_directory(&File::open(&path).unwrap()),
        Err(Error::Io)
    );
    assert_eq!(fs::metadata(&path).unwrap().mode() & 0o7777, 0o666);
    assert_eq!(fs::read(&path).unwrap(), b"preserve");

    fs::set_permissions(&root.0, fs::Permissions::from_mode(0o777)).unwrap();
    // O_PATH supports fstat but not fchmod, deterministically exercising failure.
    let directory = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_PATH | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(&root.0)
        .unwrap();
    assert_eq!(prepare_client_directory(&directory), Err(Error::Io));
    assert_eq!(directory.metadata().unwrap().mode() & 0o7777, 0o777);

    let link = root.0.join("link");
    std::os::unix::fs::symlink(&root.0, &link).unwrap();
    let link = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(link)
        .unwrap();
    assert_eq!(prepare_client_directory(&link), Err(Error::Io));
    assert_eq!(directory.metadata().unwrap().mode() & 0o7777, 0o777);
}

#[test]
fn startup_rejects_foreign_owned_directory_without_chmod() {
    // Root must not use CAP_FOWNER to take over another UID's client directory.
    if unsafe { libc::geteuid() } != 0 {
        return;
    }
    let fixture = Fixture::new();
    let path = fixture.socket().parent().unwrap().to_owned();
    fs::create_dir_all(&path).unwrap();
    let directory = File::open(&path).unwrap();
    assert_eq!(unsafe { libc::fchown(directory.as_raw_fd(), 1, !0) }, 0);
    directory
        .set_permissions(fs::Permissions::from_mode(0o777))
        .unwrap();
    assert_eq!(fixture.reconcile(&[definition()]), Err(Error::Io));
    assert_eq!(directory.metadata().unwrap().uid(), 1);
    assert_eq!(directory.metadata().unwrap().mode() & 0o7777, 0o777);
    assert_eq!(fs::read_dir(&path).unwrap().count(), 0);
}
