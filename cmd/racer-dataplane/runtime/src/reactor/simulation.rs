//! Deterministic OS boundary for the production reactor and synchronous syscalls.
//! Operations keep only borrowed pointers: the production Entry owns all backing
//! until this driver has emitted the original and cancellation completions.
use super::Descriptor;
use super::{BufferOperation, CANCEL_BIT, KernelResult, SocketAddress};
use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet, VecDeque},
    ffi::CString,
    io,
    path::{Path, PathBuf},
    rc::Rc,
};

thread_local! { static CURRENT: RefCell<Option<Simulation>> = const { RefCell::new(None) }; }

mod disk {
    //! Sparse crash disk used by the simulated OS, not a parallel storage service.
    //!
    //! File fsync commits inode data/length/metadata. Directory fsync commits that
    //! directory's immediate name-to-inode bindings, independently of file data and
    //! ancestor bindings. Unsynced state never persists implicitly. This is one
    //! deterministic, conservative outcome allowed by the durability contract.
    use super::*;
    use std::ffi::OsString;

    #[derive(Clone, Debug)]
    pub struct CrashDisk(pub(super) Simulation);

    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum DiskState {
        Volatile,
        Durable,
        Both,
    }

    #[derive(Clone, Debug)]
    struct Image {
        mode: u16,
        length: u64,
        pages: BTreeMap<u64, Rc<[u8; 4096]>>,
        symlink: Option<PathBuf>,
    }
    impl Image {
        fn capture(node: &Node) -> Self {
            Self {
                mode: node.mode,
                length: node.length,
                pages: node.pages.clone(),
                symlink: node.symlink.clone(),
            }
        }
        fn initial(node: &Node) -> Self {
            Self {
                mode: node.mode,
                length: 0,
                pages: BTreeMap::new(),
                symlink: node.symlink.clone(),
            }
        }
        fn restore(&self, inode: u64) -> Node {
            Node {
                inode,
                mode: self.mode,
                length: self.length,
                pages: self.pages.clone(),
                locked: false,
                symlink: self.symlink.clone(),
            }
        }
    }

    #[derive(Default, Debug)]
    pub(super) struct State {
        inodes: BTreeMap<u64, Image>,
        directories: BTreeMap<u64, BTreeMap<OsString, u64>>,
        root: Option<u64>,
        pub(super) generation: u64,
        crashes: Vec<(u64, PathBuf)>,
    }
    impl State {
        pub(super) fn crashed_since(&self, generation: u64, paths: &[PathBuf]) -> bool {
            self.crashes.iter().any(|(at, root)| {
                *at > generation && paths.iter().any(|path| path.starts_with(root))
            })
        }
        fn paths(&self) -> BTreeMap<PathBuf, u64> {
            let mut paths = BTreeMap::new();
            let Some(root) = self.root else {
                return paths;
            };
            let mut pending = vec![(PathBuf::from("/"), root, BTreeSet::new())];
            while let Some((path, inode, mut ancestors)) = pending.pop() {
                if !ancestors.insert(inode) {
                    continue;
                }
                paths.insert(path.clone(), inode);
                if let Some(entries) = self.directories.get(&inode) {
                    for (name, child) in entries {
                        pending.push((path.join(name), *child, ancestors.clone()));
                    }
                }
            }
            paths
        }
    }

    impl World {
        fn sync_inode(&mut self, node: &Rc<RefCell<Node>>) -> io::Result<()> {
            let n = node.borrow();
            let inode = n.inode;
            let kind = n.mode as u32 & libc::S_IFMT;
            if kind != libc::S_IFREG && kind != libc::S_IFDIR {
                return Err(errno(libc::EINVAL));
            }
            self.disk.inodes.insert(inode, Image::capture(&n));
            if kind == libc::S_IFDIR {
                // Fsync an unlinked directory still persists its inode, but it cannot
                // recreate its removed parent binding.
                if let Some(path) = self
                    .paths
                    .iter()
                    .find(|(_, candidate)| Rc::ptr_eq(candidate, node))
                    .map(|(p, _)| p.clone())
                {
                    let mut entries = BTreeMap::new();
                    for (child, node) in &self.paths {
                        if child != &path && child.parent() == Some(path.as_path()) {
                            let n = node.borrow();
                            if n.mode as u32 & libc::S_IFMT == libc::S_IFSOCK {
                                continue;
                            }
                            entries.insert(child.file_name().unwrap().to_owned(), n.inode);
                            self.disk
                                .inodes
                                .entry(n.inode)
                                .or_insert_with(|| Image::initial(&n));
                        }
                    }
                    self.disk.directories.insert(inode, entries);
                    if path == Path::new("/") {
                        self.disk.root = Some(inode);
                    }
                }
            }
            self.record(
                if kind == libc::S_IFDIR {
                    "sync:directory"
                } else {
                    "sync:file"
                },
                inode,
                0,
            );
            Ok(())
        }
    }

    impl Handle {
        /// Same durable transition as the reactor Fsync operation. Injection failures
        /// leave the durable image untouched; completion ownership stays in Reactor.
        pub fn sync(&self) -> io::Result<()> {
            let (node, _) = self.node()?;
            let mut w = self.sim.0.borrow_mut();
            if let Some(Fault::Errno(n)) = w.fault("fsync") {
                return Err(errno(n));
            }
            w.sync_inode(&node)
        }
    }

    impl CrashDisk {
        /// Fsync one existing file or directory. Does not sync its parent or children.
        pub fn sync(&self, path: &Path) -> io::Result<()> {
            let fd = self.0.open(None, path, libc::O_RDONLY)?;
            let Some(handle) = fd.as_sim() else {
                unreachable!()
            };
            handle.sync()
        }

        /// Explicit fixture provisioning/barrier: persist every current inode and
        /// directory binding. Ordinary write_file/create_dir_all remain volatile.
        pub fn sync_all(&self) -> io::Result<()> {
            let paths: Vec<_> = self
                .0
                .0
                .borrow()
                .paths
                .iter()
                .filter(|(_, node)| {
                    matches!(
                        node.borrow().mode as u32 & libc::S_IFMT,
                        libc::S_IFREG | libc::S_IFDIR
                    )
                })
                .map(|(path, _)| path.clone())
                .collect();
            for path in paths {
                self.sync(&path)?;
            }
            Ok(())
        }

        /// Power loss for all storage. Socket/pipe resources are independent: the app
        /// harness tears down process/network owners separately. Pending disk SQEs
        /// complete EIO via the production reactor; crash never drops Entry owners.
        pub fn crash(&self) -> io::Result<()> {
            self.crash_under(Path::new("/"))
        }

        /// Crash one node's disk subtree in a multi-node Simulation. Other subtrees,
        /// their open file descriptions, and their queued disk operations survive.
        /// The root must be absolute and normal; use /dst/node-N for app DST.
        pub fn crash_under(&self, root: &Path) -> io::Result<()> {
            if !root.is_absolute() {
                return Err(errno(libc::EINVAL));
            }
            let root = normalize(root)?;
            let mut w = self.0.0.borrow_mut();
            w.disk.generation += 1;
            let generation = w.disk.generation;
            w.disk.crashes.push((generation, root.clone()));
            let lost: BTreeSet<_> = w
                .paths
                .iter()
                .filter(|(path, _)| path.starts_with(&root))
                .map(|(_, n)| n.borrow().inode)
                .collect();
            w.resources.retain(|_, resource| {
                !matches!(resource, Resource::File { node, opened_path, .. }
                if opened_path.starts_with(&root) || lost.contains(&node.borrow().inode))
            });
            w.paths.retain(|path, _| !path.starts_with(&root));
            let durable = w.disk.paths();
            let mut restored = BTreeMap::<u64, Rc<RefCell<Node>>>::new();
            for (path, inode) in durable {
                if !path.starts_with(&root) {
                    continue;
                }
                let image = w
                    .disk
                    .inodes
                    .get(&inode)
                    .expect("durable binding owns inode");
                let node = restored
                    .entry(inode)
                    .or_insert_with(|| Rc::new(RefCell::new(image.restore(inode))))
                    .clone();
                w.paths.insert(path, node);
            }
            w.record("disk:crash", generation, lost.len() as i64);
            Ok(())
        }

        /// Read a bounded range from either image without allocating a sparse file's
        /// logical size. Both is a mutation selector, not a read selector.
        pub fn read(
            &self,
            path: &Path,
            offset: u64,
            length: usize,
            state: DiskState,
        ) -> io::Result<Vec<u8>> {
            let path = normalize(path)?;
            let w = self.0.0.borrow();
            let image = match state {
                DiskState::Volatile => Image::capture(
                    &w.paths
                        .get(&path)
                        .ok_or_else(|| errno(libc::ENOENT))?
                        .borrow(),
                ),
                DiskState::Durable => {
                    let inode = *w
                        .disk
                        .paths()
                        .get(&path)
                        .ok_or_else(|| errno(libc::ENOENT))?;
                    w.disk.inodes[&inode].clone()
                }
                DiskState::Both => return Err(errno(libc::EINVAL)),
            };
            if image.mode as u32 & libc::S_IFMT != libc::S_IFREG {
                return Err(errno(libc::EISDIR));
            }
            let count = length
                .min(usize::try_from(image.length.saturating_sub(offset)).unwrap_or(usize::MAX));
            let mut bytes = vec![0; count];
            for (index, byte) in bytes.iter_mut().enumerate() {
                let pos = offset + index as u64;
                *byte = image
                    .pages
                    .get(&(pos / 4096))
                    .map_or(0, |page| page[(pos % 4096) as usize]);
            }
            Ok(bytes)
        }

        /// Overwrite existing bytes without changing length or checksum. Durable-only
        /// corruption becomes visible to production reads after crash. Both requires
        /// the same inode at the volatile and durable pathname, avoiding accidental
        /// corruption of two different replacement generations. Validation is atomic.
        pub fn corrupt(
            &self,
            path: &Path,
            offset: u64,
            bytes: &[u8],
            state: DiskState,
        ) -> io::Result<()> {
            let path = normalize(path)?;
            let end = offset
                .checked_add(bytes.len() as u64)
                .ok_or_else(|| errno(libc::EFBIG))?;
            let mut w = self.0.0.borrow_mut();
            let volatile = if state != DiskState::Durable {
                Some(
                    w.paths
                        .get(&path)
                        .ok_or_else(|| errno(libc::ENOENT))?
                        .clone(),
                )
            } else {
                None
            };
            let durable = if state != DiskState::Volatile {
                Some(
                    *w.disk
                        .paths()
                        .get(&path)
                        .ok_or_else(|| errno(libc::ENOENT))?,
                )
            } else {
                None
            };
            if let Some(node) = &volatile {
                let n = node.borrow();
                if n.mode as u32 & libc::S_IFMT != libc::S_IFREG || end > n.length {
                    return Err(errno(libc::EINVAL));
                }
                if durable.is_some_and(|id| id != n.inode) {
                    return Err(errno(libc::ESTALE));
                }
            }
            if let Some(inode) = durable {
                let image = &w.disk.inodes[&inode];
                if image.mode as u32 & libc::S_IFMT != libc::S_IFREG || end > image.length {
                    return Err(errno(libc::EINVAL));
                }
            }
            if let Some(node) = &volatile {
                overwrite(&mut node.borrow_mut().pages, offset, bytes);
            }
            if let Some(inode) = durable {
                overwrite(
                    &mut w.disk.inodes.get_mut(&inode).unwrap().pages,
                    offset,
                    bytes,
                );
            }
            let inode = durable.unwrap_or_else(|| volatile.as_ref().unwrap().borrow().inode);
            w.record(
                match state {
                    DiskState::Volatile => "corrupt:volatile",
                    DiskState::Durable => "corrupt:durable",
                    DiskState::Both => "corrupt:both",
                },
                inode,
                bytes.len() as i64,
            );
            Ok(())
        }
    }

    fn overwrite(pages: &mut BTreeMap<u64, Rc<[u8; 4096]>>, offset: u64, bytes: &[u8]) {
        for (index, byte) in bytes.iter().enumerate() {
            let pos = offset + index as u64;
            Rc::make_mut(
                pages
                    .entry(pos / 4096)
                    .or_insert_with(|| Rc::new([0; 4096])),
            )[(pos % 4096) as usize] = *byte;
        }
    }
}
pub use disk::{CrashDisk, DiskState};
#[cfg(test)]
mod disk_tests {
    use super::io_tests::{reactor, scope};
    use super::*;

    #[test]
    fn delayed_completion_and_fault_trace_replay_exactly() {
        fn run() -> Vec<Event> {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let scope = scope();
            sim.write_file(Path::new("/file"), b"abc").unwrap();
            sim.inject("open", Fault::Delay(2));
            let fd = drive(
                &r,
                r.file_open(
                    None,
                    CString::new("/file").unwrap(),
                    libc::O_RDONLY,
                    0,
                    &scope,
                ),
            )
            .unwrap();
            sim.inject("read", Fault::Errno(libc::EIO));
            assert!(matches!(
                drive(&r, r.read_at(fd, 0, r.file_buffer(3).unwrap(), (), &scope)),
                Err(Error::Io)
            ));
            sim.trace()
        }
        assert_eq!(run(), run());
    }

    #[test]
    fn sparse_page_copies_preserve_boundaries_holes_and_snapshots() {
        for offset in [0, 1, 4095, 4096, 4097] {
            for length in [0, 1, 4095, 4096, 4097, 8193] {
                let sim = Simulation::new();
                let file = sim
                    .open(None, Path::new("/file"), libc::O_CREAT | libc::O_RDWR)
                    .unwrap()
                    .into_sim()
                    .unwrap();
                // Leave a full sparse page before an unaligned multi-page write.
                let offset = 8192 + offset;
                let original: Vec<_> = (0..length).map(|i| (i % 251) as u8).collect();
                assert_eq!(file.file_write(offset, &original).unwrap(), length);
                let mut expected = vec![0; offset as usize];
                expected.extend_from_slice(&original);
                let (node, _) = file.node().unwrap();
                let snapshot = node.borrow().pages.clone();
                let replacement = vec![0xa5; length + 1];
                sim.inject("write", Fault::Short(length / 2));
                assert_eq!(file.file_write(offset, &replacement).unwrap(), length / 2);
                expected[offset as usize..offset as usize + length / 2].fill(0xa5);
                for (index, byte) in original.iter().enumerate() {
                    let pos = offset as usize + index;
                    assert_eq!(snapshot[&(pos as u64 / 4096)][pos % 4096], *byte);
                }
                let mut output = vec![0xcc; expected.len() + 8];
                assert_eq!(file.file_read(0, &mut output).unwrap(), expected.len());
                assert_eq!(&output[..expected.len()], expected);
                assert_eq!(&output[expected.len()..], &[0xcc; 8]);
                let mut partial = vec![0xcc; length + 8];
                sim.inject("read", Fault::Short(length / 2));
                assert_eq!(file.file_read(offset, &mut partial).unwrap(), length / 2);
                assert_eq!(
                    &partial[..length / 2],
                    &expected[offset as usize..offset as usize + length / 2]
                );
                assert!(partial[length / 2..].iter().all(|b| *b == 0xcc));
                sim.inject("write", Fault::Errno(libc::EIO));
                assert_eq!(
                    file.file_write(offset, b"bad").unwrap_err().raw_os_error(),
                    Some(libc::EIO)
                );
                assert_eq!(sim.read_file(Path::new("/file")).unwrap(), expected);
            }
        }
    }

    #[test]
    fn sparse_files_partial_io_faults_rename_unlink_and_open_inode_ownership() {
        let (sim, _environment, r, scope) = setup();
        sim.create_dir_all(Path::new("/data")).unwrap();
        let dir = drive(
            &r,
            r.file_open(
                None,
                CString::new("/data").unwrap(),
                libc::O_RDONLY | libc::O_DIRECTORY,
                0,
                &scope,
            ),
        )
        .unwrap();
        let file = drive(
            &r,
            r.file_open(
                Some(dir.clone()),
                CString::new("a").unwrap(),
                libc::O_CREAT | libc::O_RDWR | libc::O_EXCL,
                0,
                &scope,
            ),
        )
        .unwrap();
        sim.inject("write", Fault::Short(2));
        let write = drive(
            &r,
            r.write_at(
                file.clone(),
                1 << 30,
                r.file_bytes(b"abcdef").unwrap(),
                (),
                &scope,
            ),
        )
        .unwrap();
        assert_eq!(write.bytes, 2);
        drop(write);
        let stat = drive(&r, r.file_stat(file.clone(), &scope)).unwrap();
        assert_eq!(stat.stx_size, (1 << 30) + 2);
        sim.inject("fsync", Fault::Errno(libc::EIO));
        assert_eq!(drive(&r, r.file_sync(file.clone(), &scope)), Err(Error::Io));
        drive(&r, r.file_sync(file.clone(), &scope)).unwrap();
        drive(
            &r,
            r.file_rename(
                dir.clone(),
                CString::new("a").unwrap(),
                CString::new("b").unwrap(),
                &scope,
            ),
        )
        .unwrap();
        drive(
            &r,
            r.file_unlink(dir.clone(), CString::new("b").unwrap(), &scope),
        )
        .unwrap();
        let read = drive(
            &r,
            r.read_at(
                file.clone(),
                (1 << 30) - 2,
                r.file_buffer(8).unwrap(),
                (),
                &scope,
            ),
        )
        .unwrap();
        assert_eq!(read.bytes, 4);
        assert_eq!(read.buffer.prefix(4).unwrap(), b"\0\0ab");
        assert!(matches!(
            drive(
                &r,
                r.file_open(
                    Some(dir.clone()),
                    CString::new("b").unwrap(),
                    libc::O_RDONLY,
                    0,
                    &scope
                )
            ),
            Err(Error::NotFound)
        ));
        drop((read, file, dir));
        assert_eq!(sim.live_handles(), 0);
        assert!(
            sim.trace()
                .iter()
                .any(|e| e.operation == "complete:fsync" && e.result == -(libc::EIO as i64))
        );
    }

    #[test]
    fn partition_stalls_established_streams_and_connects_then_heals_without_loss() {
        let (sim, _environment, r, scope) = setup();
        let a = SocketAddress::Inet("127.0.0.1:101".parse().unwrap());
        let b = SocketAddress::Inet("127.0.0.1:102".parse().unwrap());
        let listener = sim.listen(b.clone()).unwrap();
        let _node = sim.enter_endpoint(a.clone());
        let client = Rc::new(sim.connect(b.clone()).unwrap());
        let listener = listener.into_sim().unwrap();
        let server = Rc::new(listener.accept().unwrap());
        drive(
            &r,
            r.send(client.clone(), r.file_bytes(b"before").unwrap(), (), &scope),
        )
        .unwrap();
        sim.partition(a.clone(), b.clone());
        let read = drive(
            &r,
            r.recv(server.clone(), r.file_buffer(32).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(read.buffer.prefix(read.bytes).unwrap(), b"before");
        drop(read);
        let mut send = r.send(client.clone(), r.file_bytes(b"after").unwrap(), (), &scope);
        let mut reverse = r.send(
            server.clone(),
            r.file_bytes(b"reverse").unwrap(),
            (),
            &scope,
        );
        let fresh = Rc::new(Descriptor::socket(libc::AF_INET).unwrap());
        let mut connect = r.connect(fresh.clone(), b.clone(), &scope);
        let mut ready = r.readiness(client.clone(), libc::POLLOUT as u32, &scope);
        for _ in 0..5 {
            assert!(poll(&mut send).is_pending());
            assert!(poll(&mut reverse).is_pending());
            assert!(poll(&mut connect).is_pending());
            assert!(poll(&mut ready).is_pending());
            r.poll_budgeted(8).unwrap();
        }
        let (x, y) = sim.socket_pair();
        x.try_send(b"ok").unwrap();
        let mut bytes = [0; 8];
        assert_eq!(y.try_recv(&mut bytes).unwrap(), 2);
        sim.heal(b.clone(), a.clone());
        assert_eq!(drive(&r, send).unwrap().bytes, 5);
        assert_eq!(drive(&r, reverse).unwrap().bytes, 7);
        drive(&r, connect).unwrap();
        drive(&r, ready).unwrap();
        assert_eq!(server.try_recv(&mut bytes).unwrap(), 5);
        assert_eq!(&bytes[..5], b"after");
        assert_eq!(client.try_recv(&mut bytes).unwrap(), 7);
        assert_eq!(&bytes[..7], b"reverse");
        sim.partition(a, b);
        let weak = Rc::downgrade(&client);
        let mut pending = r.send(client, r.file_bytes(b"blocked").unwrap(), (), &scope);
        assert!(poll(&mut pending).is_pending());
        drop(pending);
        r.poll_budgeted(1).unwrap();
        assert!(weak.upgrade().is_some());
        drive(&r, r.drain()).unwrap();
        assert!(weak.upgrade().is_none());
    }
    use super::super::tests::fixtures::{Reactor, RequestScope, ResourceClass};
    use crate::Error;
    use std::time::{Duration, Instant};

    fn setup() -> (Simulation, Environment, Reactor, RequestScope) {
        let sim = Simulation::new();
        let environment = sim.enter();
        let r = reactor();
        let scope = RequestScope::new((), Instant::now() + Duration::from_secs(30)).unwrap();
        (sim, environment, r, scope)
    }
    use super::io_tests::{drive, poll};
    fn open(sim: &Simulation, path: &str) -> Rc<Descriptor> {
        Rc::new(sim.open(None, Path::new(path), libc::O_RDWR).unwrap())
    }
    fn read(sim: &Simulation, path: &str) -> Vec<u8> {
        sim.disk()
            .read(Path::new(path), 0, 64, DiskState::Volatile)
            .unwrap()
    }

    #[test]
    fn file_sync_and_namespace_sync_are_independent() {
        for (file_sync, dir_sync) in [(false, false), (true, false), (false, true), (true, true)] {
            let (sim, _environment, r, scope) = setup();
            sim.create_dir_all(Path::new("/disk")).unwrap();
            sim.disk().sync_all().unwrap();
            sim.write_file(Path::new("/disk/new"), b"contents").unwrap();
            if file_sync {
                drive(&r, r.file_sync(open(&sim, "/disk/new"), &scope)).unwrap();
            }
            if dir_sync {
                let dir = Rc::new(
                    sim.open(None, Path::new("/disk"), libc::O_RDONLY | libc::O_DIRECTORY)
                        .unwrap(),
                );
                drive(&r, r.file_sync(dir, &scope)).unwrap();
            }
            sim.disk().crash().unwrap();
            if !dir_sync {
                assert_eq!(
                    sim.read_file(Path::new("/disk/new"))
                        .unwrap_err()
                        .raw_os_error(),
                    Some(libc::ENOENT)
                );
            } else {
                assert_eq!(
                    read(&sim, "/disk/new"),
                    if file_sync {
                        b"contents".as_slice()
                    } else {
                        b""
                    }
                );
            }
        }
    }

    #[test]
    fn replacement_unlink_and_directory_ancestors_require_namespace_fences() {
        let (sim, _environment, _, _) = setup();
        sim.write_file(Path::new("/disk/current"), b"old").unwrap();
        sim.disk().sync_all().unwrap();
        sim.write_file(Path::new("/disk/stage"), b"new").unwrap();
        sim.disk().sync(Path::new("/disk/stage")).unwrap();
        sim.rename(Path::new("/disk/stage"), Path::new("/disk/current"), 0)
            .unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/disk/current"), b"old");
        sim.write_file(Path::new("/disk/stage"), b"new").unwrap();
        sim.disk().sync(Path::new("/disk/stage")).unwrap();
        sim.rename(Path::new("/disk/stage"), Path::new("/disk/current"), 0)
            .unwrap();
        sim.disk().sync(Path::new("/disk")).unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/disk/current"), b"new");
        sim.unlink(Path::new("/disk/current")).unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/disk/current"), b"new");
        sim.unlink(Path::new("/disk/current")).unwrap();
        sim.disk().sync(Path::new("/disk")).unwrap();
        sim.disk().crash().unwrap();
        assert!(sim.read_file(Path::new("/disk/current")).is_err());
        sim.write_file(Path::new("/disk/child/file"), b"hidden")
            .unwrap();
        sim.disk().sync(Path::new("/disk/child/file")).unwrap();
        sim.disk().sync(Path::new("/disk/child")).unwrap();
        sim.disk().crash().unwrap();
        assert!(sim.metadata(Path::new("/disk/child")).is_err());
    }

    #[test]
    fn failed_sync_and_crash_during_pending_write_preserve_fences_and_other_disks() {
        let (sim, _environment, r, scope) = setup();
        for path in ["/a/file", "/b/file"] {
            sim.write_file(Path::new(path), b"old").unwrap();
        }
        sim.disk().sync_all().unwrap();
        let a = open(&sim, "/a/file");
        let b = open(&sim, "/b/file");
        drive(
            &r,
            r.write_at(a.clone(), 0, r.file_bytes(b"bad").unwrap(), (), &scope),
        )
        .unwrap();
        sim.inject("fsync", Fault::Errno(libc::EIO));
        assert_eq!(drive(&r, r.file_sync(a.clone(), &scope)), Err(Error::Io));
        sim.inject("write", Fault::Delay(3));
        let weak = Rc::downgrade(&a);
        let lease = r
            .admission
            .reserve(None, ResourceClass::Connection, 1)
            .unwrap();
        let mut write = r.write_at(a, 0, r.file_bytes(b"late").unwrap(), lease, &scope);
        assert!(poll(&mut write).is_pending());
        sim.disk().crash_under(Path::new("/a")).unwrap();
        assert!(weak.upgrade().is_some());
        assert_eq!(r.in_flight(), 1);
        assert_eq!(read(&sim, "/a/file"), b"old");
        drive(
            &r,
            r.write_at(b, 0, r.file_bytes(b"new").unwrap(), (), &scope),
        )
        .unwrap();
        assert!(matches!(drive(&r, write), Err(Error::Io)));
        assert!(weak.upgrade().is_none());
        assert_eq!(read(&sim, "/a/file"), b"old");
        assert_eq!(read(&sim, "/b/file"), b"new");
        assert_eq!(r.admission.used(ResourceClass::Connection), 0);
        // A path-only open queued before crash cannot recreate lost names afterwards.
        sim.inject("open", Fault::Delay(2));
        let mut op = r.file_open(
            None,
            CString::new("/a/late").unwrap(),
            libc::O_CREAT | libc::O_RDWR,
            0,
            &scope,
        );
        assert!(poll(&mut op).is_pending());
        sim.disk().crash_under(Path::new("/a")).unwrap();
        assert!(matches!(drive(&r, op), Err(Error::Io)));
        assert!(sim.metadata(Path::new("/a/late")).is_err());
    }

    #[test]
    fn sparse_corruption_and_truncate_do_not_mutate_durable_shared_pages() {
        let (sim, _environment, r, scope) = setup();
        sim.write_file(Path::new("/file"), b"abcdef").unwrap();
        sim.disk().sync_all().unwrap();
        let fd = open(&sim, "/file");
        let Some(h) = fd.as_sim() else { unreachable!() };
        h.set_len(2).unwrap();
        h.set_len(6).unwrap();
        assert_eq!(read(&sim, "/file"), b"ab\0\0\0\0");
        assert_eq!(
            sim.disk()
                .read(Path::new("/file"), 0, 8, DiskState::Durable)
                .unwrap(),
            b"abcdef"
        );
        sim.disk()
            .corrupt(Path::new("/file"), 1, b"X", DiskState::Volatile)
            .unwrap();
        sim.disk()
            .corrupt(Path::new("/file"), 4, b"Y", DiskState::Durable)
            .unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/file"), b"abcdYf");
        let fd = open(&sim, "/file");
        drive(
            &r,
            r.write_at(
                fd.clone(),
                1 << 40,
                r.file_bytes(b"sparse").unwrap(),
                (),
                &scope,
            ),
        )
        .unwrap();
        drive(&r, r.file_sync(fd, &scope)).unwrap();
        assert_eq!(
            sim.disk()
                .read(Path::new("/file"), (1 << 40) - 2, 10, DiskState::Durable)
                .unwrap(),
            b"\0\0sparse"
        );
        assert!(
            sim.disk()
                .corrupt(Path::new("/file"), u64::MAX, b"bad", DiskState::Both)
                .is_err()
        );
        sim.disk().crash().unwrap();
        assert_eq!(
            sim.disk()
                .read(Path::new("/file"), 1 << 40, 6, DiskState::Volatile)
                .unwrap(),
            b"sparse"
        );
    }

    #[test]
    fn crash_after_sync_issue_before_cqe_and_abandoned_sync_keep_real_fences() {
        let (sim, _environment, r, scope) = setup();
        sim.write_file(Path::new("/file"), b"old").unwrap();
        sim.disk().sync_all().unwrap();
        let fd = open(&sim, "/file");
        drive(
            &r,
            r.write_at(fd.clone(), 0, r.file_bytes(b"new").unwrap(), (), &scope),
        )
        .unwrap();
        let mut sync = r.file_sync(fd.clone(), &scope);
        assert!(poll(&mut sync).is_pending());
        // Submit fsync, but leave its successful original CQE unconsumed.
        r.poll_budgeted(1).unwrap();
        assert_eq!(r.in_flight(), 1);
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/file"), b"new");
        assert!(matches!(drive(&r, sync), Ok(())));
        assert!(matches!(drive(&r, r.file_stat(fd, &scope)), Err(Error::Io)));
        let fd = open(&sim, "/file");
        drive(
            &r,
            r.write_at(fd.clone(), 0, r.file_bytes(b"bad").unwrap(), (), &scope),
        )
        .unwrap();
        let weak = Rc::downgrade(&fd);
        let mut sync = r.file_sync(fd, &scope);
        assert!(poll(&mut sync).is_pending());
        drop(sync);
        r.poll_budgeted(1).unwrap();
        assert!(weak.upgrade().is_some());
        r.poll_budgeted(1).unwrap();
        assert!(weak.upgrade().is_some());
        r.poll_budgeted(1).unwrap();
        assert!(weak.upgrade().is_none());
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/file"), b"new");
    }

    #[test]
    fn renamed_directory_fsync_uses_inode_and_durable_corruption_targets_exact_generation() {
        let (sim, _environment, _, _) = setup();
        sim.write_file(Path::new("/old/file"), b"before").unwrap();
        sim.disk().sync_all().unwrap();
        let directory = sim
            .open(None, Path::new("/old"), libc::O_RDONLY | libc::O_DIRECTORY)
            .unwrap();
        sim.rename(Path::new("/old"), Path::new("/new"), 0).unwrap();
        sim.write_file(Path::new("/new/extra"), b"extra").unwrap();
        sim.disk().sync(Path::new("/new/extra")).unwrap();
        let directory = directory.into_sim().unwrap();
        directory.sync().unwrap();
        sim.disk().sync(Path::new("/")).unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/new/file"), b"before");
        assert_eq!(read(&sim, "/new/extra"), b"extra");
        assert!(sim.metadata(Path::new("/old")).is_err());
        sim.write_file(Path::new("/stage"), b"replace").unwrap();
        sim.rename(Path::new("/stage"), Path::new("/new/file"), 0)
            .unwrap();
        assert_eq!(
            sim.disk()
                .corrupt(Path::new("/new/file"), 0, b"X", DiskState::Both)
                .unwrap_err()
                .raw_os_error(),
            Some(libc::ESTALE)
        );
        assert_eq!(read(&sim, "/new/file"), b"replace");
        assert_eq!(
            sim.disk()
                .read(Path::new("/new/file"), 0, 64, DiskState::Durable)
                .unwrap(),
            b"before"
        );
    }

    // Migrated from test_support::disk. Persistence faults mutate the integrated disk;
    // submissions, readback, and crash-invalidated handles use the production reactor.
    #[test]
    fn crashprefix_covers_missing_torn_and_reordered_overwrites() {
        let offset = (1 << 40) - 4;
        for (prefix, expected) in [b"base", b"bBBe", b"AABe"].into_iter().enumerate() {
            let (sim, _environment, r, scope) = setup();
            let path = Path::new("/sparse");
            sim.write_file(path, b"").unwrap();
            let fd = open(&sim, "/sparse");
            drive(
                &r,
                r.write_at(
                    fd.clone(),
                    offset,
                    r.file_bytes(b"base").unwrap(),
                    (),
                    &scope,
                ),
            )
            .unwrap();
            sim.disk().sync_all().unwrap();
            for (at, bytes) in [(offset, b"AAAA".as_slice()), (offset + 1, b"BB".as_slice())] {
                drive(
                    &r,
                    r.write_at(fd.clone(), at, r.file_bytes(bytes).unwrap(), (), &scope),
                )
                .unwrap();
            }
            assert_eq!(
                sim.disk()
                    .read(path, offset, 4, DiskState::Volatile)
                    .unwrap(),
                b"ABBA"
            );
            // Explicit durable-only fault prefixes preserve the independent volatile image.
            if prefix >= 1 {
                sim.disk()
                    .corrupt(path, offset + 1, b"BB", DiskState::Durable)
                    .unwrap();
            }
            if prefix >= 2 {
                sim.disk()
                    .corrupt(path, offset, b"AA", DiskState::Durable)
                    .unwrap();
            }
            assert_eq!(
                sim.disk()
                    .read(path, offset, 4, DiskState::Volatile)
                    .unwrap(),
                b"ABBA"
            );
            sim.disk().crash().unwrap();
            assert!(matches!(
                drive(
                    &r,
                    r.write_at(fd, offset, r.file_bytes(b"late").unwrap(), (), &scope)
                ),
                Err(Error::Io)
            ));
            let result = drive(
                &r,
                r.read_at(
                    open(&sim, "/sparse"),
                    offset,
                    r.file_buffer(4).unwrap(),
                    (),
                    &scope,
                ),
            )
            .unwrap();
            assert_eq!(result.bytes, 4);
            assert_eq!(result.buffer.prefix(4).unwrap(), expected);
            drop(result);
            assert_eq!(r.in_flight(), 0);
            assert_eq!(sim.live_handles(), 0);
        }
    }

    #[test]
    fn invalid_fault_plans_and_extents_are_atomic_and_holes_are_zero() {
        let (sim, _environment, r, scope) = setup();
        let path = Path::new("/file");
        sim.write_file(path, b"\0\0\0\0data").unwrap();
        sim.disk().sync_all().unwrap();
        for (offset, bytes) in [(4, b"large".as_slice()), (u64::MAX, b"x".as_slice())] {
            assert!(
                sim.disk()
                    .corrupt(path, offset, bytes, DiskState::Both)
                    .is_err()
            );
            for state in [DiskState::Volatile, DiskState::Durable] {
                assert_eq!(sim.disk().read(path, 0, 8, state).unwrap(), b"\0\0\0\0data");
            }
        }
        let old = open(&sim, "/file");
        assert!(matches!(
            drive(
                &r,
                r.write_at(
                    old.clone(),
                    u64::MAX,
                    r.file_bytes(b"x").unwrap(),
                    (),
                    &scope
                )
            ),
            Err(Error::Io)
        ));
        assert_eq!(read(&sim, "/file"), b"\0\0\0\0data");
        let holes = drive(
            &r,
            r.read_at(old.clone(), 0, r.file_buffer(4).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(holes.buffer.prefix(4).unwrap(), &[0; 4]);
        drop(holes);
        // Files have EOF rather than the removed fixture's arbitrary capacity bound.
        assert_eq!(
            drive(
                &r,
                r.read_at(old.clone(), 15, r.file_buffer(2).unwrap(), (), &scope)
            )
            .unwrap()
            .bytes,
            0
        );
        sim.disk().crash().unwrap();
        let fresh = open(&sim, "/file");
        let (Some(a), Some(b)) = (old.as_sim(), fresh.as_sim()) else {
            unreachable!()
        };
        assert_ne!(a.id(), b.id());
        assert!(matches!(
            drive(&r, r.file_stat(old, &scope)),
            Err(Error::Io)
        ));
        drive(
            &r,
            r.write_at(fresh, 0, r.file_bytes(b"new").unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(read(&sim, "/file"), b"new\0data");
        assert_eq!(r.in_flight(), 0);
    }
}
#[cfg(test)]
mod io_tests {
    //! Assertions migrated from test_support::io onto production Entry ownership.
    use super::super::tests::fixtures::{Admission, Limits, Reactor, RequestScope, ResourceClass};
    use super::*;
    use crate::{Error, Operation, Result};
    use std::{
        cell::Cell,
        task::{Context, Poll},
        time::{Duration, Instant},
    };

    pub(super) fn reactor() -> Reactor {
        Reactor::new(Rc::new(Admission::new(Limits {
            queue_entries: std::num::NonZeroUsize::new(64).unwrap(),
        })))
    }
    pub(super) fn scope() -> RequestScope {
        RequestScope::new((), Instant::now() + Duration::from_secs(30)).unwrap()
    }
    pub(super) fn poll<T>(op: &mut Operation<'_, T>) -> Poll<Result<T>> {
        op.as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }
    pub(super) fn drive<T>(r: &Reactor, mut op: Operation<'_, T>) -> Result<T> {
        for _ in 0..1000 {
            if let Poll::Ready(result) = poll(&mut op) {
                return result;
            }
            r.poll_budgeted(8)?;
            r.wait(Duration::ZERO)?;
        }
        panic!("simulation did not progress")
    }

    #[test]
    fn real_reactor_stream_backpressure_eof_and_completion_fences() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        r.init().unwrap();
        assert!(r.state.borrow().ring.is_none());
        assert!(r.state.borrow().wake.is_none());
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let scope = scope();
        let address = SocketAddress::Unix("/stream".into());
        let listener = Rc::new(sim.listen(address.clone()).unwrap());
        let client = Rc::new(Descriptor::socket(libc::AF_UNIX).unwrap());
        drive(&r, r.connect(client.clone(), address, &scope)).unwrap();
        let server = Rc::new(drive(&r, r.accept(listener.clone(), &scope)).unwrap());
        sim.set_stream_capacity(3);
        let mut sent = drive(
            &r,
            r.send(client.clone(), r.file_bytes(b"abcdef").unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(sent.bytes, 3);
        sent.buffer.advance(sent.bytes).unwrap();
        assert_eq!(sent.buffer.remaining(), 3);
        let mut writable = r.readiness(client.clone(), libc::POLLOUT as u32, &scope);
        assert!(poll(&mut writable).is_pending());
        r.poll_budgeted(8).unwrap();
        assert!(poll(&mut writable).is_pending());
        let read = drive(
            &r,
            r.recv(server.clone(), r.file_buffer(8).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(read.bytes, 3);
        assert_eq!(read.buffer.prefix(3).unwrap(), b"abc");
        drop(read);
        assert_eq!(drive(&r, writable).unwrap(), libc::POLLOUT as u32);
        // Reuse the returned owner to finish the request, then send a reply.
        let mut sent = drive(&r, r.send(client.clone(), sent.buffer, (), &scope)).unwrap();
        assert_eq!(sent.bytes, 3);
        sent.buffer.advance(sent.bytes).unwrap();
        assert_eq!(sent.buffer.remaining(), 0);
        drop(sent);
        let read = drive(
            &r,
            r.recv(server.clone(), r.file_buffer(8).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(read.buffer.prefix(read.bytes).unwrap(), b"def");
        drop(read);
        let sent = drive(
            &r,
            r.send(server.clone(), r.file_bytes(b"ok").unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(sent.bytes, 2);
        drop(sent);
        let reply = drive(
            &r,
            r.recv(client.clone(), r.file_buffer(8).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(reply.buffer.prefix(reply.bytes).unwrap(), b"ok");
        drop(reply);
        drop(client);
        assert_eq!(
            drive(
                &r,
                r.recv(server.clone(), r.file_buffer(8).unwrap(), (), &scope)
            )
            .unwrap()
            .bytes,
            0
        );
        assert!(matches!(
            drive(
                &r,
                r.send(server.clone(), r.file_bytes(b"late").unwrap(), (), &scope)
            ),
            Err(Error::Io)
        ));
        drive(&r, r.drain()).unwrap();
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.init(), Err(Error::Unavailable));
        drop((server, listener));
        assert_eq!(sim.live_handles(), 0);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
    }

    #[test]
    fn scoped_selection_and_listener_pending_close_are_isolated() {
        let sim = Simulation::new();
        let other = Simulation::new();
        assert!(Simulation::current().is_none());
        {
            let _scope = sim.enter();
            {
                let _nested = other.enter();
                assert!(Rc::ptr_eq(&Simulation::current().unwrap().0, &other.0));
            }
            assert!(Rc::ptr_eq(&Simulation::current().unwrap().0, &sim.0));
        }
        assert!(Simulation::current().is_none());
        let address = SocketAddress::Inet("127.0.0.1:1234".parse().unwrap());
        let listener = sim.listen(address.clone()).unwrap();
        let client = sim.connect(address).unwrap();
        assert_eq!(sim.live_handles(), 3);
        drop(listener);
        assert_eq!(sim.live_handles(), 1);
        let client = client.into_sim().unwrap();
        assert_eq!(
            client.send(b"x").unwrap_err().raw_os_error(),
            Some(libc::EPIPE)
        );
        drop(client);
        assert_eq!(sim.live_handles(), 0);
    }

    #[test]
    fn wrapped_stream_and_pipe_copies_preserve_short_io_and_errors() {
        let sim = Simulation::new();
        let (writer, reader) = sim.socket_pair();
        let (writer, reader) = (writer.into_sim().unwrap(), reader.into_sim().unwrap());
        let (pipe_reader, pipe_writer) = sim.pipe(16);
        let (pipe_reader, pipe_writer) = (
            pipe_reader.into_sim().unwrap(),
            pipe_writer.into_sim().unwrap(),
        );
        // Install wrapped queues explicitly so this does not depend on allocator growth.
        let wrapped = || {
            let mut queue = VecDeque::with_capacity(16);
            queue.extend(0..16);
            queue.drain(..12);
            queue.extend(16..24);
            assert!(!queue.as_slices().1.is_empty());
            queue
        };
        {
            let mut world = sim.0.borrow_mut();
            let Resource::Socket { bytes, .. } = world.resources.get_mut(&reader.id).unwrap()
            else {
                unreachable!()
            };
            *bytes = wrapped();
            let Resource::Pipe { bytes, .. } = world.resources.get(&pipe_reader.id).unwrap() else {
                unreachable!()
            };
            *bytes.borrow_mut() = wrapped();
        }
        let mut output = [0xcc; 16];
        sim.inject("recv", Fault::Short(7));
        assert_eq!(reader.recv(&mut output).unwrap(), 7);
        assert_eq!(&output[..7], &[12, 13, 14, 15, 16, 17, 18]);
        assert_eq!(&output[7..], &[0xcc; 9]);
        assert_eq!(reader.recv(&mut output).unwrap(), 5);
        assert_eq!(&output[..5], &[19, 20, 21, 22, 23]);
        sim.inject("pipe_read", Fault::Short(7));
        assert_eq!(pipe_reader.pipe_read(&mut output).unwrap(), 7);
        assert_eq!(&output[..7], &[12, 13, 14, 15, 16, 17, 18]);
        assert_eq!(pipe_writer.pipe_write(&[24, 25, 26, 27]).unwrap(), 4);
        sim.inject("send", Fault::Errno(libc::EPIPE));
        assert_eq!(
            pipe_reader.splice(&writer, 9).unwrap_err().raw_os_error(),
            Some(libc::EPIPE)
        );
        sim.inject("splice", Fault::Short(6));
        assert_eq!(pipe_reader.splice(&writer, 9).unwrap(), 6);
        assert_eq!(reader.recv(&mut output).unwrap(), 6);
        assert_eq!(&output[..6], &[19, 20, 21, 22, 23, 24]);
        assert_eq!(pipe_reader.pipe_read(&mut output).unwrap(), 3);
        assert_eq!(&output[..3], &[25, 26, 27]);
        assert_eq!(
            pipe_reader.pipe_read(&mut output).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(
            reader.recv(&mut output).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        drop(writer);
        assert_eq!(reader.recv(&mut output).unwrap(), 0);
    }

    #[test]
    fn datagrams_preserve_packet_boundaries_and_source_addresses() {
        let sim = Simulation::new();
        let server_address = "127.0.0.1:53".parse().unwrap();
        let server = sim.bind_datagram(server_address).unwrap();
        let client = sim.bind_datagram("127.0.0.1:0".parse().unwrap()).unwrap();
        let (server, client) = (server.into_sim().unwrap(), client.into_sim().unwrap());
        client.connect_datagram(server_address).unwrap();
        client.send_datagram(b"query").unwrap();
        let mut bytes = [0; 32];
        let (count, source) = server.recv_from(&mut bytes).unwrap();
        assert_eq!(&bytes[..count], b"query");
        server.send_to(b"reply", source).unwrap();
        let (count, source) = client.recv_from(&mut bytes).unwrap();
        assert_eq!(source, server_address);
        assert_eq!(&bytes[..count], b"reply");
    }

    struct Probe(Rc<Cell<usize>>);
    impl Drop for Probe {
        fn drop(&mut self) {
            self.0.set(self.0.get() + 1);
        }
    }

    #[test]
    fn abandoned_resources_wait_for_both_fences_in_either_order() {
        for cancel_first in [false, true] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            r.init().unwrap();
            let baseline = r.admission.used(ResourceClass::RequestContext);
            let scope = scope();
            let (fd, peer) = sim.socket_pair();
            let fd = Rc::new(fd);
            let weak = Rc::downgrade(&fd);
            let drops = Rc::new(Cell::new(0));
            let lease = r
                .admission
                .reserve(None, ResourceClass::Connection, 1)
                .unwrap();
            let mut recv = r.recv(
                fd,
                r.file_buffer(8).unwrap(),
                (Probe(drops.clone()), lease),
                &scope,
            );
            assert!(poll(&mut recv).is_pending());
            let id = *r.state.borrow().entries.keys().next().unwrap();
            // An unsolicited cancellation CQE must not mutate the live entry.
            assert!(matches!(
                r.state.borrow_mut().complete(id.0 | CANCEL_BIT, 0),
                Err(Error::Io)
            ));
            drop(recv);
            assert_eq!(r.poll_budgeted(1), Ok(0));
            if !cancel_first {
                // Reorder the two actual driver CQEs, leaving production fence logic intact.
                r.state
                    .borrow_mut()
                    .simulation
                    .as_mut()
                    .unwrap()
                    .completed
                    .borrow_mut()
                    .swap(0, 1);
            }
            assert_eq!(r.poll_budgeted(1), Ok(1));
            assert_eq!(drops.get(), 0);
            assert!(weak.upgrade().is_some());
            assert_eq!(r.in_flight(), 1);
            assert_eq!(r.admission.used(ResourceClass::Connection), 1);
            assert!(r.admission.used(ResourceClass::RequestContext) > baseline);
            assert_eq!(r.poll_budgeted(1), Ok(1));
            assert_eq!(drops.get(), 1);
            assert!(weak.upgrade().is_none());
            assert_eq!(r.in_flight(), 0);
            assert_eq!(r.admission.used(ResourceClass::Connection), 0);
            assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
            assert!(matches!(
                r.state.borrow_mut().complete(id.0, 8),
                Err(Error::Io)
            ));
            drop(peer);
            assert_eq!(sim.live_handles(), 0);
        }
    }

    #[test]
    fn scheduled_short_io_disconnect_and_budget_preserve_resource_ownership() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        r.init().unwrap();
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let scope = scope();
        let (fd, peer) = sim.socket_pair();
        let fd = Rc::new(fd);
        let drops = Rc::new(Cell::new(0));
        sim.inject("send", Fault::Delay(2));
        sim.set_max_chunk(3);
        let mut first = r.send(
            fd.clone(),
            r.file_bytes(&[1; 8]).unwrap(),
            Probe(drops.clone()),
            &scope,
        );
        assert!(poll(&mut first).is_pending());
        let first_id = *r.state.borrow().entries.keys().next().unwrap();
        sim.inject("send", Fault::Errno(libc::ECONNRESET));
        let mut second = r.send(
            fd.clone(),
            r.file_bytes(&[2; 8]).unwrap(),
            Probe(drops.clone()),
            &scope,
        );
        assert!(poll(&mut second).is_pending());
        assert_eq!(r.poll_budgeted(0), Ok(0));
        assert_eq!(r.poll_budgeted(1), Ok(0));
        assert_eq!(r.poll_budgeted(1), Ok(1));
        assert!(poll(&mut first).is_pending());
        assert!(matches!(
            poll(&mut second),
            std::task::Poll::Ready(Err(Error::Io))
        ));
        assert_eq!(
            drops.get(),
            1,
            "failed I/O releases its owned resources after the CQE"
        );
        drop(second);
        assert_eq!(r.poll_budgeted(1), Ok(0));
        assert_eq!(r.poll_budgeted(0), Ok(0));
        assert!(poll(&mut first).is_pending());
        assert_eq!(r.poll_budgeted(1), Ok(1));
        let std::task::Poll::Ready(Ok(mut completed)) = poll(&mut first) else {
            panic!("short send did not complete")
        };
        drop(first);
        assert_eq!(completed.bytes, 3);
        assert_eq!(completed.buffer.prefix(8).unwrap(), &[1; 8]);
        assert_eq!(drops.get(), 1);
        assert_eq!(completed.buffer.advance(9), Err(Error::Io));
        assert_eq!(completed.buffer.remaining(), 8);
        completed.buffer.advance(3).unwrap();
        let mut remainder = r.send(fd.clone(), completed.buffer, completed.lease, &scope);
        assert!(poll(&mut remainder).is_pending());
        let next_id = *r.state.borrow().entries.keys().next().unwrap();
        assert!(next_id > first_id);
        assert!(matches!(
            r.state.borrow_mut().complete(first_id.0, 3),
            Err(Error::Io)
        ));
        assert_eq!(
            r.in_flight(),
            1,
            "stale CQE cannot retire the new submission"
        );
        sim.disconnect(&fd).unwrap();
        assert!(matches!(drive(&r, remainder), Err(Error::Io)));
        assert_eq!(drops.get(), 2);
        let mut bytes = [0; 8];
        assert_eq!(peer.try_recv(&mut bytes).unwrap(), 3);
        assert_eq!(&bytes[..3], &[1; 3]);
        let eof = drive(
            &r,
            r.recv(Rc::new(peer), r.file_buffer(8).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(eof.bytes, 0);
        drop((eof, fd));
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(sim.live_handles(), 0);
        assert!(
            sim.trace()
                .iter()
                .any(|e| e.operation == "submit:send" && e.resource == next_id.0)
        );
    }
}

/// Labels new outbound streams with their owning node's listening endpoint.
/// Enter this scope when polling that node; established sockets retain the label.
pub struct EndpointEnvironment {
    sim: Simulation,
    previous: Option<SocketAddress>,
}
impl Drop for EndpointEnvironment {
    fn drop(&mut self) {
        self.sim.0.borrow_mut().endpoint = self.previous.take();
    }
}

#[derive(Clone, Debug)]
pub struct Simulation(Rc<RefCell<World>>);
pub struct Environment {
    previous: Option<Simulation>,
}
impl Drop for Environment {
    fn drop(&mut self) {
        CURRENT.with(|s| *s.borrow_mut() = self.previous.take());
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Event {
    pub sequence: u64,
    pub operation: String,
    pub resource: u64,
    pub result: i64,
}
/// Queued faults match the next submitted operation with this name (or `*`).
/// `Delay` counts driver submission turns; `Short` caps a single byte transfer.
/// Synchronous syscalls support Errno/Short; use Delay on reactor submissions.
#[derive(Clone, Debug)]
pub enum Fault {
    Errno(i32),
    Short(usize),
    Delay(usize),
    /// Execute normally, then retain the actual result for this many driver turns.
    /// Bytes are visible to the peer while production CQE owners remain pinned.
    HoldCompletion(usize),
}

#[derive(Debug)]
struct World {
    cancel_first: bool,
    next: u64,
    resources: BTreeMap<u64, Resource>,
    paths: BTreeMap<PathBuf, Rc<RefCell<Node>>>,
    listeners: BTreeMap<SocketAddress, u64>,
    datagrams: BTreeMap<std::net::SocketAddr, u64>,
    faults: VecDeque<(String, Fault)>,
    trace: Vec<Event>,
    next_event: u64,
    stream_capacity: usize,
    max_chunk: usize,
    executing: bool,
    endpoint: Option<SocketAddress>,
    partitions: BTreeSet<(SocketAddress, SocketAddress)>,
    disk: disk::State,
}
#[derive(Debug)]
enum Resource {
    Socket {
        peer: Option<u64>,
        bytes: VecDeque<u8>,
        connected: bool,
        local: Option<SocketAddress>,
        remote: Option<SocketAddress>,
    },
    Datagram {
        address: std::net::SocketAddr,
        peer: Option<std::net::SocketAddr>,
        packets: VecDeque<(std::net::SocketAddr, Vec<u8>)>,
    },
    Listener {
        pending: VecDeque<u64>,
    },
    File {
        node: Rc<RefCell<Node>>,
        flags: i32,
        lock_owner: bool,
        opened_path: PathBuf,
    },
    Pipe {
        bytes: Rc<RefCell<VecDeque<u8>>>,
        write: bool,
        capacity: usize,
    },
}
#[derive(Debug)]
struct Node {
    inode: u64,
    mode: u16,
    length: u64,
    pages: BTreeMap<u64, Rc<[u8; 4096]>>,
    locked: bool,
    symlink: Option<PathBuf>,
}

#[derive(Debug)]
pub struct Handle {
    sim: Simulation,
    id: u64,
}
impl Drop for Handle {
    fn drop(&mut self) {
        let mut w = self.sim.0.borrow_mut();
        match w.resources.remove(&self.id) {
            Some(Resource::File {
                node,
                lock_owner: true,
                ..
            }) => node.borrow_mut().locked = false,
            Some(Resource::Listener { pending }) => {
                for id in pending {
                    w.resources.remove(&id);
                    w.record("close", id, 0);
                }
            }
            _ => (),
        }
        w.listeners.retain(|_, id| *id != self.id);
        w.datagrams.retain(|_, id| *id != self.id);
        w.record("close", self.id, 0);
    }
}

fn errno(n: i32) -> io::Error {
    io::Error::from_raw_os_error(n)
}
impl World {
    fn partitioned(&self, a: &SocketAddress, b: &SocketAddress) -> bool {
        self.partitions
            .contains(&endpoint_pair(a.clone(), b.clone()))
    }
    fn stream_partitioned(&self, id: u64) -> bool {
        matches!(self.resources.get(&id), Some(Resource::Socket { local: Some(a), remote: Some(b), .. }) if self.partitioned(a,b))
    }
    fn id(&mut self) -> u64 {
        let id = self.next;
        self.next += 1;
        id
    }
    fn record(&mut self, op: &str, resource: u64, result: i64) {
        self.trace.push(Event {
            sequence: self.next_event,
            operation: op.into(),
            resource,
            result,
        });
        self.next_event += 1;
    }
    fn fault(&mut self, op: &str) -> Option<Fault> {
        if self.executing {
            return None;
        }
        let index = self
            .faults
            .iter()
            .position(|(name, _)| name == op || name == "*")?;
        let (_, fault) = self.faults.remove(index)?;
        self.record(
            &format!("fault:{op}"),
            0,
            match fault {
                Fault::Errno(n) => -(n as i64),
                Fault::Short(n) | Fault::Delay(n) | Fault::HoldCompletion(n) => n as i64,
            },
        );
        Some(fault)
    }
    fn path(&self, dir: Option<&Descriptor>, name: &Path) -> io::Result<PathBuf> {
        if name.is_absolute() {
            return normalize(name);
        }
        let base = match dir {
            None => PathBuf::from("/"),
            Some(fd) if fd.as_sim().is_some() => {
                let h = fd.as_sim().unwrap();
                let Some(Resource::File { node, .. }) = self.resources.get(&h.id) else {
                    return Err(errno(libc::ENOTDIR));
                };
                if node.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
                    return Err(errno(libc::ENOTDIR));
                }
                self.paths
                    .iter()
                    .find(|(_, n)| Rc::ptr_eq(n, node))
                    .map(|(p, _)| p.clone())
                    .ok_or_else(|| errno(libc::ENOENT))?
            }
            _ => return Err(errno(libc::EBADF)),
        };
        normalize(&base.join(name))
    }
    fn node(&mut self, mode: u16) -> Rc<RefCell<Node>> {
        Rc::new(RefCell::new(Node {
            inode: self.id(),
            mode,
            length: 0,
            pages: BTreeMap::new(),
            locked: false,
            symlink: None,
        }))
    }
    fn resolve(
        &self,
        path: PathBuf,
        boundary: Option<&Path>,
        no_symlinks: bool,
    ) -> io::Result<PathBuf> {
        let mut path = path;
        for _ in 0..40 {
            let mut prefix = PathBuf::from("/");
            let parts: Vec<_> = path.components().collect();
            let mut replacement = None;
            for (index, component) in parts.iter().enumerate() {
                if let std::path::Component::Normal(name) = component {
                    prefix.push(name);
                }
                if let Some(target) = self
                    .paths
                    .get(&prefix)
                    .and_then(|n| n.borrow().symlink.clone())
                {
                    if no_symlinks {
                        return Err(errno(libc::ELOOP));
                    }
                    if target.is_absolute() && boundary.is_some() {
                        return Err(errno(libc::EXDEV));
                    }
                    let mut next = if target.is_absolute() {
                        target
                    } else {
                        prefix.parent().unwrap_or(Path::new("/")).join(target)
                    };
                    for component in &parts[index + 1..] {
                        next.push(component.as_os_str());
                    }
                    let next = normalize(&next)?;
                    if boundary.is_some_and(|b| !next.starts_with(b)) {
                        return Err(errno(libc::EXDEV));
                    }
                    replacement = Some(next);
                    break;
                }
            }
            match replacement {
                Some(next) => path = next,
                None => return Ok(path),
            }
        }
        Err(errno(libc::ELOOP))
    }
}
fn endpoint_pair(a: SocketAddress, b: SocketAddress) -> (SocketAddress, SocketAddress) {
    if a <= b { (a, b) } else { (b, a) }
}
fn normalize(path: &Path) -> io::Result<PathBuf> {
    let mut out = PathBuf::from("/");
    for component in path.components() {
        match component {
            std::path::Component::RootDir | std::path::Component::CurDir => (),
            std::path::Component::Normal(name) => out.push(name),
            _ => return Err(errno(libc::EXDEV)),
        }
    }
    Ok(out)
}

impl Simulation {
    pub fn new() -> Self {
        let sim = Self(Rc::new(RefCell::new(World {
            cancel_first: true,
            next: 1,
            resources: BTreeMap::new(),
            paths: BTreeMap::new(),
            listeners: BTreeMap::new(),
            datagrams: BTreeMap::new(),
            faults: VecDeque::new(),
            trace: Vec::new(),
            next_event: 0,
            stream_capacity: 64 * 1024,
            max_chunk: usize::MAX,
            executing: false,
            endpoint: None,
            partitions: BTreeSet::new(),
            disk: disk::State::default(),
        })));
        sim.create_dir_all(Path::new("/")).unwrap();
        sim.disk().sync_all().unwrap();
        sim
    }
    /// Selection is worker-thread scoped. Reactors and descriptors retain their
    /// environment independently, including while a nested environment is active.
    pub fn enter(&self) -> Environment {
        Environment {
            previous: CURRENT.with(|s| s.borrow_mut().replace(self.clone())),
        }
    }
    pub fn current() -> Option<Self> {
        CURRENT.with(|s| s.borrow().clone())
    }
    pub fn disk(&self) -> CrashDisk {
        CrashDisk(self.clone())
    }
    pub fn enter_endpoint(&self, endpoint: SocketAddress) -> EndpointEnvironment {
        let previous = self.0.borrow_mut().endpoint.replace(endpoint);
        EndpointEnvironment {
            sim: self.clone(),
            previous,
        }
    }
    /// Symmetric blackhole: existing streams and future connects stall until heal
    /// or their normal production deadlines/cancellation. Received bytes remain
    /// readable; a partition does not manufacture EOF or discard accepted bytes.
    pub fn partition(&self, a: SocketAddress, b: SocketAddress) {
        let mut w = self.0.borrow_mut();
        w.partitions.insert(endpoint_pair(a, b));
        w.record("partition", 0, 0);
    }
    pub fn heal(&self, a: SocketAddress, b: SocketAddress) {
        let mut w = self.0.borrow_mut();
        w.partitions.remove(&endpoint_pair(a, b));
        w.record("heal", 0, 0);
    }
    /// Assign labels to a preexisting socket pair or a socket created outside a
    /// node scope. Updates the opposite endpoint too, including pending accepts.
    pub fn label_stream(
        &self,
        fd: &Descriptor,
        local: SocketAddress,
        remote: SocketAddress,
    ) -> io::Result<()> {
        let Some(h) = fd.as_sim() else {
            return Err(errno(libc::EXDEV));
        };
        if !Rc::ptr_eq(&self.0, &h.sim.0) {
            return Err(errno(libc::EXDEV));
        }
        let mut w = self.0.borrow_mut();
        let Some(Resource::Socket {
            peer,
            local: a,
            remote: b,
            ..
        }) = w.resources.get_mut(&h.id)
        else {
            return Err(errno(libc::ENOTSOCK));
        };
        *a = Some(local.clone());
        *b = Some(remote.clone());
        let peer = *peer;
        if let Some(Resource::Socket {
            local: a,
            remote: b,
            ..
        }) = peer.and_then(|id| w.resources.get_mut(&id))
        {
            *a = Some(remote);
            *b = Some(local);
        }
        Ok(())
    }
    pub fn inject(&self, operation: &str, fault: Fault) {
        if let Fault::Errno(errno) = &fault {
            assert!(*errno > 0, "fault errno must be positive");
        }
        self.0
            .borrow_mut()
            .faults
            .push_back((operation.into(), fault));
    }
    pub fn trace(&self) -> Vec<Event> {
        self.0.borrow().trace.clone()
    }
    pub fn take_trace(&self) -> Vec<Event> {
        std::mem::take(&mut self.0.borrow_mut().trace)
    }
    pub fn live_handles(&self) -> usize {
        self.0.borrow().resources.len()
    }
    pub fn next_sequence(&self) -> u64 {
        self.0.borrow_mut().id()
    }
    /// Disconnect an established stream even while production owns both handles.
    /// Already received bytes remain readable, then reads return EOF.
    pub fn disconnect(&self, descriptor: &Descriptor) -> io::Result<()> {
        let Some(handle) = descriptor.as_sim() else {
            return Err(errno(libc::EXDEV));
        };
        if !Rc::ptr_eq(&self.0, &handle.sim.0) {
            return Err(errno(libc::EXDEV));
        }
        let mut w = self.0.borrow_mut();
        let Some(Resource::Socket { peer, .. }) = w.resources.get_mut(&handle.id) else {
            return Err(errno(libc::ENOTSOCK));
        };
        let other = peer.take();
        if let Some(Resource::Socket { peer, .. }) = other.and_then(|id| w.resources.get_mut(&id)) {
            *peer = None;
        }
        w.record("disconnect", handle.id, 0);
        Ok(())
    }
    pub fn set_stream_capacity(&self, capacity: usize) {
        assert!(capacity > 0);
        self.0.borrow_mut().stream_capacity = capacity;
    }
    /// Select cancellation CQE ordering in the simulated kernel.
    pub fn set_cancel_first(&self, cancel_first: bool) {
        self.0.borrow_mut().cancel_first = cancel_first;
    }
    pub fn set_max_chunk(&self, bytes: usize) {
        assert!(bytes > 0);
        self.0.borrow_mut().max_chunk = bytes;
    }
    fn insert(&self, resource: Resource) -> Descriptor {
        let mut w = self.0.borrow_mut();
        let id = w.id();
        w.resources.insert(id, resource);
        w.record("create", id, 0);
        Descriptor::from(Handle {
            sim: self.clone(),
            id,
        })
    }
    pub fn socket(&self, domain: i32) -> io::Result<Descriptor> {
        if ![libc::AF_INET, libc::AF_INET6, libc::AF_UNIX].contains(&domain) {
            return Err(errno(libc::EAFNOSUPPORT));
        }
        let local = self.0.borrow().endpoint.clone();
        Ok(self.insert(Resource::Socket {
            peer: None,
            bytes: VecDeque::new(),
            connected: false,
            local,
            remote: None,
        }))
    }
    pub fn bind_datagram(&self, mut address: std::net::SocketAddr) -> io::Result<Descriptor> {
        if address.port() == 0 {
            let w = self.0.borrow();
            let port = (20000..=65535)
                .find(|port| {
                    address.set_port(*port);
                    !w.datagrams.contains_key(&address)
                })
                .ok_or_else(|| errno(libc::EADDRINUSE))?;
            address.set_port(port);
        }
        if self.0.borrow().datagrams.contains_key(&address) {
            return Err(errno(libc::EADDRINUSE));
        }
        let fd = self.insert(Resource::Datagram {
            address,
            peer: None,
            packets: VecDeque::new(),
        });
        let Some(h) = fd.as_sim() else { unreachable!() };
        self.0.borrow_mut().datagrams.insert(address, h.id);
        Ok(fd)
    }
    pub fn listen(&self, address: SocketAddress) -> io::Result<Descriptor> {
        if self.0.borrow().listeners.contains_key(&address) {
            return Err(errno(libc::EADDRINUSE));
        }
        let fd = self.insert(Resource::Listener {
            pending: VecDeque::new(),
        });
        let Some(h) = fd.as_sim() else { unreachable!() };
        self.0.borrow_mut().listeners.insert(address.clone(), h.id);
        if let SocketAddress::Unix(path) = address {
            let mut w = self.0.borrow_mut();
            if w.paths.contains_key(&path) {
                drop(w);
                drop(fd);
                return Err(errno(libc::EADDRINUSE));
            }
            let node = w.node(libc::S_IFSOCK as u16 | 0o660);
            w.paths.insert(path, node);
        }
        Ok(fd)
    }
    pub fn connect(&self, address: SocketAddress) -> io::Result<Descriptor> {
        let fd = self.socket(libc::AF_UNIX)?;
        let Some(h) = fd.as_sim() else { unreachable!() };
        h.connect(&address)?;
        Ok(fd)
    }
    pub fn socket_pair(&self) -> (Descriptor, Descriptor) {
        let a = self.socket(libc::AF_UNIX).unwrap();
        let b = self.socket(libc::AF_UNIX).unwrap();
        let (Some(ah), Some(bh)) = (a.as_sim(), b.as_sim()) else {
            unreachable!()
        };
        let mut w = self.0.borrow_mut();
        for (id, peer) in [(ah.id, bh.id), (bh.id, ah.id)] {
            if let Resource::Socket {
                peer: p, connected, ..
            } = w.resources.get_mut(&id).unwrap()
            {
                *p = Some(peer);
                *connected = true;
            }
        }
        (a, b)
    }
    pub fn pipe(&self, capacity: usize) -> (Descriptor, Descriptor) {
        let bytes = Rc::new(RefCell::new(VecDeque::new()));
        (
            self.insert(Resource::Pipe {
                bytes: bytes.clone(),
                write: false,
                capacity,
            }),
            self.insert(Resource::Pipe {
                bytes,
                write: true,
                capacity,
            }),
        )
    }
    pub fn create_dir_all(&self, path: &Path) -> io::Result<()> {
        let path = normalize(path)?;
        let mut w = self.0.borrow_mut();
        let mut paths: Vec<_> = path.ancestors().collect();
        paths.reverse();
        for path in paths {
            if let Some(node) = w.paths.get(path) {
                if node.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
                    return Err(errno(libc::ENOTDIR));
                }
            } else {
                let node = w.node(libc::S_IFDIR as u16 | 0o755);
                w.paths.insert(path.into(), node);
            }
        }
        Ok(())
    }
    pub fn open(
        &self,
        dir: Option<&Descriptor>,
        path: &Path,
        flags: i32,
    ) -> io::Result<Descriptor> {
        self.open_resolved(dir, path, flags, 0)
    }
    pub fn symlink(&self, target: &Path, path: &Path) -> io::Result<()> {
        let path = normalize(path)?;
        let mut w = self.0.borrow_mut();
        if w.paths.contains_key(&path) {
            return Err(errno(libc::EEXIST));
        }
        if !w
            .paths
            .contains_key(path.parent().unwrap_or(Path::new("/")))
        {
            return Err(errno(libc::ENOENT));
        }
        let node = w.node(libc::S_IFLNK as u16 | 0o777);
        node.borrow_mut().symlink = Some(target.into());
        w.paths.insert(path, node);
        w.record("symlink", 0, 0);
        Ok(())
    }
    fn open_resolved(
        &self,
        dir: Option<&Descriptor>,
        path: &Path,
        flags: i32,
        resolve: u64,
    ) -> io::Result<Descriptor> {
        if let Some(dir) = dir {
            match dir.as_sim() {
                Some(handle) if Rc::ptr_eq(&self.0, &handle.sim.0) => (),
                _ => return Err(errno(libc::EXDEV)),
            }
        }
        let (node, opened_path) = {
            let mut w = self.0.borrow_mut();
            if let Some(Fault::Errno(n)) = w.fault("open") {
                return Err(errno(n));
            }
            if resolve & 0x08 != 0 && path.is_absolute() {
                return Err(errno(libc::EXDEV));
            }
            let boundary = if resolve & 0x08 != 0 {
                Some(w.path(dir, Path::new("."))?)
            } else {
                None
            };
            let path = w.path(dir, path)?;
            if flags & libc::O_NOFOLLOW != 0
                && w.paths
                    .get(&path)
                    .is_some_and(|n| n.borrow().symlink.is_some())
            {
                return Err(errno(libc::ELOOP));
            }
            let path = w.resolve(path, boundary.as_deref(), resolve & 0x04 != 0)?;
            if w.paths.contains_key(&path)
                && flags & (libc::O_CREAT | libc::O_EXCL) == (libc::O_CREAT | libc::O_EXCL)
            {
                return Err(errno(libc::EEXIST));
            }
            if !w.paths.contains_key(&path) {
                if flags & libc::O_CREAT == 0 {
                    return Err(errno(libc::ENOENT));
                }
                let parent = w
                    .paths
                    .get(path.parent().ok_or_else(|| errno(libc::ENOENT))?)
                    .ok_or_else(|| errno(libc::ENOENT))?;
                if parent.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
                    return Err(errno(libc::ENOTDIR));
                }
                let node = w.node(libc::S_IFREG as u16 | 0o600);
                w.paths.insert(path.clone(), node);
            }
            let node = w.paths[&path].clone();
            if flags & libc::O_DIRECTORY != 0
                && node.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR
            {
                return Err(errno(libc::ENOTDIR));
            }
            if flags & libc::O_TRUNC != 0 {
                let mut n = node.borrow_mut();
                if n.mode as u32 & libc::S_IFMT != libc::S_IFREG {
                    return Err(errno(libc::EISDIR));
                }
                if flags & libc::O_ACCMODE == libc::O_RDONLY {
                    return Err(errno(libc::EINVAL));
                }
                n.pages.clear();
                n.length = 0;
            }
            (node, path)
        };
        Ok(self.insert(Resource::File {
            node,
            flags,
            lock_owner: false,
            opened_path,
        }))
    }
    pub fn write_file(&self, path: &Path, bytes: &[u8]) -> io::Result<()> {
        if let Some(parent) = path.parent() {
            self.create_dir_all(parent)?;
        }
        let fd = self.open(None, path, libc::O_CREAT | libc::O_RDWR | libc::O_TRUNC)?;
        let Some(h) = fd.as_sim() else { unreachable!() };
        let mut offset = 0;
        while offset < bytes.len() {
            let written = h.file_write(offset as u64, &bytes[offset..])?;
            if written == 0 {
                return Err(errno(libc::EIO));
            }
            offset += written;
        }
        Ok(())
    }
    pub fn read_file(&self, path: &Path) -> io::Result<Vec<u8>> {
        let fd = self.open(None, path, libc::O_RDONLY)?;
        let Some(h) = fd.as_sim() else { unreachable!() };
        let mut bytes =
            vec![0; usize::try_from(h.stat()?.stx_size).map_err(|_| errno(libc::EFBIG))?];
        let mut offset = 0;
        while offset < bytes.len() {
            let count = h.file_read(offset as u64, &mut bytes[offset..])?;
            if count == 0 {
                bytes.truncate(offset);
                break;
            }
            offset += count;
        }
        Ok(bytes)
    }
    pub fn metadata(&self, path: &Path) -> io::Result<(u64, u16)> {
        let w = self.0.borrow();
        let node = w
            .paths
            .get(&normalize(path)?)
            .ok_or_else(|| errno(libc::ENOENT))?
            .borrow();
        Ok((node.inode, node.mode))
    }
    pub fn chmod(&self, path: &Path, mode: u32) -> io::Result<()> {
        let mut w = self.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("chmod") {
            return Err(errno(n));
        }
        let mut node = w
            .paths
            .get(&normalize(path)?)
            .ok_or_else(|| errno(libc::ENOENT))?
            .borrow_mut();
        node.mode = (node.mode & libc::S_IFMT as u16) | (mode as u16 & 0o7777);
        Ok(())
    }
    pub fn rename(&self, from: &Path, to: &Path, flags: u32) -> io::Result<()> {
        let mut w = self.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("rename") {
            return Err(errno(n));
        }
        let from = normalize(from)?;
        let to = normalize(to)?;
        if flags & !(libc::RENAME_NOREPLACE | libc::RENAME_EXCHANGE) != 0
            || flags == (libc::RENAME_NOREPLACE | libc::RENAME_EXCHANGE)
        {
            return Err(errno(libc::EINVAL));
        }
        if !w.paths.contains_key(&from) {
            return Err(errno(libc::ENOENT));
        }
        if from == to {
            return Ok(());
        }
        if from == Path::new("/") || to == Path::new("/") {
            return Err(errno(libc::EBUSY));
        }
        let parent = w
            .paths
            .get(to.parent().unwrap())
            .ok_or_else(|| errno(libc::ENOENT))?;
        if parent.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
            return Err(errno(libc::ENOTDIR));
        }
        if to.starts_with(&from) || from.starts_with(&to) {
            return Err(errno(libc::EINVAL));
        }
        if flags & libc::RENAME_NOREPLACE != 0 && w.paths.contains_key(&to) {
            return Err(errno(libc::EEXIST));
        }
        if flags & libc::RENAME_EXCHANGE != 0 && !w.paths.contains_key(&to) {
            return Err(errno(libc::ENOENT));
        }
        if flags & libc::RENAME_EXCHANGE == 0
            && let Some(target) = w.paths.get(&to)
        {
            let source_dir = w.paths[&from].borrow().mode as u32 & libc::S_IFMT == libc::S_IFDIR;
            let target_dir = target.borrow().mode as u32 & libc::S_IFMT == libc::S_IFDIR;
            if source_dir != target_dir {
                return Err(errno(if source_dir {
                    libc::ENOTDIR
                } else {
                    libc::EISDIR
                }));
            }
            if target_dir && w.paths.keys().any(|p| p != &to && p.starts_with(&to)) {
                return Err(errno(libc::ENOTEMPTY));
            }
        }
        let descendants: Vec<_> = w
            .paths
            .iter()
            .filter_map(|(path, node)| {
                if path != &from && path.starts_with(&from) {
                    Some((
                        path.clone(),
                        to.join(path.strip_prefix(&from).unwrap()),
                        node.clone(),
                    ))
                } else if flags & libc::RENAME_EXCHANGE != 0 && path != &to && path.starts_with(&to)
                {
                    Some((
                        path.clone(),
                        from.join(path.strip_prefix(&to).unwrap()),
                        node.clone(),
                    ))
                } else {
                    None
                }
            })
            .collect();
        for (path, _, _) in &descendants {
            w.paths.remove(path);
        }
        for (_, path, node) in descendants {
            w.paths.insert(path, node);
        }
        let source = w.paths.remove(&from).unwrap();
        let target = w.paths.insert(to.clone(), source);
        if flags & libc::RENAME_EXCHANGE != 0 {
            w.paths.insert(from.clone(), target.unwrap());
        }
        // Keep open-description crash ownership attached to the moved inode.
        let locations: BTreeMap<_, _> = w
            .paths
            .iter()
            .map(|(path, node)| (node.borrow().inode, path.clone()))
            .collect();
        for resource in w.resources.values_mut() {
            if let Resource::File {
                node, opened_path, ..
            } = resource
                && let Some(path) = locations.get(&node.borrow().inode)
            {
                *opened_path = path.clone();
            }
        }
        let a = w.listeners.remove(&SocketAddress::Unix(from.clone()));
        let b = w.listeners.remove(&SocketAddress::Unix(to.clone()));
        if let Some(a) = a {
            w.listeners.insert(SocketAddress::Unix(to), a);
        }
        if flags & libc::RENAME_EXCHANGE != 0
            && let Some(b) = b
        {
            w.listeners.insert(SocketAddress::Unix(from), b);
        }
        w.record("rename", 0, 0);
        Ok(())
    }
    pub fn unlink(&self, path: &Path) -> io::Result<()> {
        let mut w = self.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("unlink") {
            return Err(errno(n));
        }
        let path = normalize(path)?;
        if w.paths
            .get(&path)
            .is_some_and(|n| n.borrow().mode as u32 & libc::S_IFMT == libc::S_IFDIR)
        {
            return Err(errno(libc::EISDIR));
        }
        w.paths.remove(&path).ok_or_else(|| errno(libc::ENOENT))?;
        w.listeners.remove(&SocketAddress::Unix(path));
        w.record("unlink", 0, 0);
        Ok(())
    }
}

impl Default for Simulation {
    fn default() -> Self {
        Self::new()
    }
}
impl Handle {
    pub fn simulation(&self) -> Simulation {
        self.sim.clone()
    }
    pub fn id(&self) -> u64 {
        self.id
    }
    pub fn connect_datagram(&self, peer: std::net::SocketAddr) -> io::Result<()> {
        let mut w = self.sim.0.borrow_mut();
        let Some(Resource::Datagram { peer: target, .. }) = w.resources.get_mut(&self.id) else {
            return Err(errno(libc::ENOTSOCK));
        };
        *target = Some(peer);
        Ok(())
    }
    pub fn send_to(&self, bytes: &[u8], target: std::net::SocketAddr) -> io::Result<usize> {
        let mut w = self.sim.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("send_datagram") {
            return Err(errno(n));
        }
        let Some(Resource::Datagram { address, .. }) = w.resources.get(&self.id) else {
            return Err(errno(libc::ENOTSOCK));
        };
        let address = *address;
        let target_id = *w
            .datagrams
            .get(&target)
            .ok_or_else(|| errno(libc::ECONNREFUSED))?;
        let Some(Resource::Datagram { packets, peer, .. }) = w.resources.get_mut(&target_id) else {
            unreachable!()
        };
        if peer.is_none_or(|peer| peer == address) {
            if packets.len() >= 64 {
                return Err(errno(libc::EAGAIN));
            }
            packets.push_back((address, bytes.to_vec()));
        }
        w.record("send_datagram", self.id, bytes.len() as i64);
        Ok(bytes.len())
    }
    pub fn recv_from(&self, bytes: &mut [u8]) -> io::Result<(usize, std::net::SocketAddr)> {
        let mut w = self.sim.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("recv_datagram") {
            return Err(errno(n));
        }
        let Some(Resource::Datagram { packets, .. }) = w.resources.get_mut(&self.id) else {
            return Err(errno(libc::ENOTSOCK));
        };
        let (address, packet) = packets.pop_front().ok_or_else(|| errno(libc::EAGAIN))?;
        let count = bytes.len().min(packet.len());
        bytes[..count].copy_from_slice(&packet[..count]);
        w.record("recv_datagram", self.id, count as i64);
        Ok((count, address))
    }
    pub fn send_datagram(&self, bytes: &[u8]) -> io::Result<usize> {
        let peer = match self.sim.0.borrow().resources.get(&self.id) {
            Some(Resource::Datagram {
                peer: Some(peer), ..
            }) => *peer,
            _ => return Err(errno(libc::ENOTCONN)),
        };
        self.send_to(bytes, peer)
    }
    pub fn validate_socket(&self) -> io::Result<()> {
        if matches!(
            self.sim.0.borrow().resources.get(&self.id),
            Some(Resource::Socket { .. })
        ) {
            Ok(())
        } else {
            Err(errno(libc::ENOTSOCK))
        }
    }
    pub fn idle_healthy(&self) -> bool {
        let w = self.sim.0.borrow();
        matches!(w.resources.get(&self.id), Some(Resource::Socket { peer: Some(peer), bytes, .. }) if bytes.is_empty() && w.resources.contains_key(peer))
    }
    pub fn peer_disconnected(&self) -> bool {
        let w = self.sim.0.borrow();
        !matches!(w.resources.get(&self.id), Some(Resource::Socket { peer: Some(peer), .. }) if w.resources.contains_key(peer))
    }
    pub fn connect(&self, address: &SocketAddress) -> io::Result<()> {
        let mut w = self.sim.0.borrow_mut();
        let local = match w.resources.get(&self.id) {
            Some(Resource::Socket { local, .. }) => local.clone(),
            _ => return Err(errno(libc::ENOTSOCK)),
        };
        if local.as_ref().is_some_and(|a| w.partitioned(a, address)) {
            w.record("blocked:connect", self.id, 0);
            return Err(errno(libc::EAGAIN));
        }
        let listener = *w
            .listeners
            .get(address)
            .ok_or_else(|| errno(libc::ECONNREFUSED))?;
        if !matches!(
            w.resources.get(&self.id),
            Some(Resource::Socket {
                connected: false,
                ..
            })
        ) {
            return Err(errno(libc::EISCONN));
        }
        let peer = w.id();
        w.resources.insert(
            peer,
            Resource::Socket {
                peer: Some(self.id),
                bytes: VecDeque::new(),
                connected: true,
                local: Some(address.clone()),
                remote: local,
            },
        );
        if let Resource::Socket {
            peer: p,
            connected,
            remote,
            ..
        } = w.resources.get_mut(&self.id).unwrap()
        {
            *p = Some(peer);
            *connected = true;
            *remote = Some(address.clone());
        }
        let Some(Resource::Listener { pending }) = w.resources.get_mut(&listener) else {
            return Err(errno(libc::ECONNREFUSED));
        };
        pending.push_back(peer);
        w.record("connect", self.id, 0);
        Ok(())
    }
    pub fn accept(&self) -> io::Result<Descriptor> {
        let mut w = self.sim.0.borrow_mut();
        let Some(Resource::Listener { pending }) = w.resources.get_mut(&self.id) else {
            return Err(errno(libc::EINVAL));
        };
        let id = pending.pop_front().ok_or_else(|| errno(libc::EAGAIN))?;
        w.record("accept", self.id, id as i64);
        Ok(Descriptor::from(Handle {
            sim: self.sim.clone(),
            id,
        }))
    }
    pub fn send(&self, bytes: &[u8]) -> io::Result<usize> {
        let mut w = self.sim.0.borrow_mut();
        let limit = match w.fault("send") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let capacity = w.stream_capacity;
        let max_chunk = w.max_chunk;
        let Some(Resource::Socket {
            peer, connected, ..
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::ENOTCONN));
        };
        let peer = peer.ok_or_else(|| {
            errno(if *connected {
                libc::EPIPE
            } else {
                libc::ENOTCONN
            })
        })?;
        if !bytes.is_empty() && w.stream_partitioned(self.id) {
            w.record("blocked:send", self.id, bytes.len() as i64);
            return Err(errno(libc::EAGAIN));
        }
        let Some(Resource::Socket { bytes: output, .. }) = w.resources.get_mut(&peer) else {
            return Err(errno(libc::EPIPE));
        };
        let count = bytes
            .len()
            .min(capacity.saturating_sub(output.len()))
            .min(max_chunk)
            .min(limit);
        if count == 0 && !bytes.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        output.extend(&bytes[..count]);
        w.record("send", self.id, count as i64);
        Ok(count)
    }
    pub fn recv(&self, bytes: &mut [u8]) -> io::Result<usize> {
        let mut w = self.sim.0.borrow_mut();
        let limit = match w.fault("recv") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let max_chunk = w.max_chunk;
        let Some(Resource::Socket {
            peer, connected, ..
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::ENOTSOCK));
        };
        if !connected {
            return Err(errno(libc::ENOTCONN));
        }
        let closed = peer.is_none_or(|id| !w.resources.contains_key(&id));
        let Some(Resource::Socket { bytes: input, .. }) = w.resources.get_mut(&self.id) else {
            unreachable!()
        };
        if input.is_empty() && !closed && !bytes.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        let count = bytes.len().min(input.len()).min(max_chunk).min(limit);
        // VecDeque's reader copies contiguous slices, including a wrapped tail.
        std::io::Read::read_exact(input, &mut bytes[..count])?;
        w.record("recv", self.id, count as i64);
        Ok(count)
    }
    fn ready(&self, interest: u32) -> io::Result<i32> {
        let w = self.sim.0.borrow();
        let flags = match w.resources.get(&self.id) {
            Some(Resource::Datagram { packets, .. }) => {
                libc::POLLOUT | if packets.is_empty() { 0 } else { libc::POLLIN }
            }
            Some(Resource::Listener { pending }) => {
                if pending.is_empty() {
                    0
                } else {
                    libc::POLLIN
                }
            }
            Some(Resource::Socket { peer, bytes, .. }) => {
                let remote = peer.and_then(|id| w.resources.get(&id));
                let mut flags = if bytes.is_empty() { 0 } else { libc::POLLIN };
                if let Some(Resource::Socket { bytes, .. }) = remote {
                    if bytes.len() < w.stream_capacity && !w.stream_partitioned(self.id) {
                        flags |= libc::POLLOUT;
                    }
                } else {
                    flags |= libc::POLLHUP | libc::POLLIN | libc::POLLOUT;
                }
                flags
            }
            _ => return Err(errno(libc::EINVAL)),
        };
        let flags = flags as u32 & (interest | libc::POLLHUP as u32);
        if flags == 0 {
            Err(errno(libc::EAGAIN))
        } else {
            Ok(flags as i32)
        }
    }
    fn node(&self) -> io::Result<(Rc<RefCell<Node>>, i32)> {
        match self.sim.0.borrow().resources.get(&self.id) {
            Some(Resource::File { node, flags, .. }) => Ok((node.clone(), *flags)),
            _ => Err(errno(libc::EBADF)),
        }
    }
    pub fn stat(&self) -> io::Result<libc::statx> {
        let (node, _) = self.node()?;
        let node = node.borrow();
        let mut stat: libc::statx = unsafe { std::mem::zeroed() };
        stat.stx_mask = libc::STATX_BASIC_STATS | libc::STATX_DIOALIGN;
        stat.stx_mode = node.mode;
        stat.stx_ino = node.inode;
        stat.stx_size = node.length;
        stat.stx_nlink = 1;
        stat.stx_dio_mem_align = 4096;
        stat.stx_dio_offset_align = 4096;
        stat.stx_uid = 0;
        stat.stx_gid = 0;
        Ok(stat)
    }
    pub fn set_len(&self, length: u64) -> io::Result<()> {
        let (node, flags) = self.node()?;
        if flags & libc::O_ACCMODE == libc::O_RDONLY {
            return Err(errno(libc::EBADF));
        }
        let mut node = node.borrow_mut();
        if node.mode as u32 & libc::S_IFMT != libc::S_IFREG {
            return Err(errno(libc::EINVAL));
        }
        node.pages.retain(|page, _| *page < length.div_ceil(4096));
        if !length.is_multiple_of(4096)
            && let Some(page) = node.pages.get_mut(&(length / 4096))
        {
            Rc::make_mut(page)[(length % 4096) as usize..].fill(0);
        }
        node.length = length;
        Ok(())
    }
    pub fn lock(&self) -> io::Result<()> {
        let (node, _) = self.node()?;
        let mut n = node.borrow_mut();
        if n.locked {
            return Err(errno(libc::EWOULDBLOCK));
        }
        n.locked = true;
        if let Some(Resource::File { lock_owner, .. }) =
            self.sim.0.borrow_mut().resources.get_mut(&self.id)
        {
            *lock_owner = true;
        }
        Ok(())
    }
    pub fn file_read(&self, offset: u64, bytes: &mut [u8]) -> io::Result<usize> {
        let limit = match self.sim.0.borrow_mut().fault("read") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let (node, flags) = self.node()?;
        if !self.sim.0.borrow().executing {
            check_direct(flags, offset, bytes.as_ptr(), bytes.len())?;
        }
        if flags & libc::O_ACCMODE == libc::O_WRONLY {
            return Err(errno(libc::EBADF));
        }
        let node = node.borrow();
        if node.mode as u32 & libc::S_IFMT != libc::S_IFREG {
            return Err(errno(libc::EISDIR));
        }
        let len = bytes
            .len()
            .min(node.length.saturating_sub(offset) as usize)
            .min(limit);
        // Copy one sparse page at a time, including unaligned first/last pages.
        let mut index = 0;
        while index < len {
            let pos = offset + index as u64;
            let start = (pos % 4096) as usize;
            let count = (4096 - start).min(len - index);
            let output = &mut bytes[index..index + count];
            if let Some(page) = node.pages.get(&(pos / 4096)) {
                output.copy_from_slice(&page[start..start + count]);
            } else {
                output.fill(0);
            }
            index += count;
        }
        Ok(len)
    }
    fn file_write(&self, offset: u64, bytes: &[u8]) -> io::Result<usize> {
        let (node, flags) = self.node()?;
        if !self.sim.0.borrow().executing {
            check_direct(flags, offset, bytes.as_ptr(), bytes.len())?;
        }
        let limit = match self.sim.0.borrow_mut().fault("write") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let bytes = &bytes[..bytes.len().min(limit)];
        if flags & libc::O_ACCMODE == libc::O_RDONLY {
            return Err(errno(libc::EBADF));
        }
        let end = offset
            .checked_add(bytes.len() as u64)
            .ok_or_else(|| errno(libc::EFBIG))?;
        let mut node = node.borrow_mut();
        if node.mode as u32 & libc::S_IFMT != libc::S_IFREG {
            return Err(errno(libc::EISDIR));
        }
        let mut index = 0;
        while index < bytes.len() {
            let pos = offset + index as u64;
            let start = (pos % 4096) as usize;
            let count = (4096 - start).min(bytes.len() - index);
            let page = Rc::make_mut(
                node.pages
                    .entry(pos / 4096)
                    .or_insert_with(|| Rc::new([0; 4096])),
            );
            page[start..start + count].copy_from_slice(&bytes[index..index + count]);
            index += count;
        }
        node.length = node.length.max(end);
        Ok(bytes.len())
    }
    pub fn pipe_write(&self, bytes: &[u8]) -> io::Result<usize> {
        let limit = match self.sim.0.borrow_mut().fault("pipe_write") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let w = self.sim.0.borrow();
        let Some(Resource::Pipe {
            bytes: output,
            write: true,
            capacity,
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::EBADF));
        };
        let mut output = output.borrow_mut();
        let count = bytes
            .len()
            .min(capacity.saturating_sub(output.len()))
            .min(limit);
        if count == 0 && !bytes.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        output.extend(&bytes[..count]);
        Ok(count)
    }
    pub fn pipe_read(&self, bytes: &mut [u8]) -> io::Result<usize> {
        let limit = match self.sim.0.borrow_mut().fault("pipe_read") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let w = self.sim.0.borrow();
        let Some(Resource::Pipe {
            bytes: input,
            write: false,
            ..
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::EBADF));
        };
        let mut input = input.borrow_mut();
        let count = bytes.len().min(input.len()).min(limit);
        if count == 0 && !bytes.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        std::io::Read::read_exact(&mut *input, &mut bytes[..count])?;
        Ok(count)
    }
    pub fn splice(&self, socket: &Handle, count: usize) -> io::Result<usize> {
        let count = match self.sim.0.borrow_mut().fault("splice") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => count.min(n),
            _ => count,
        };
        if !Rc::ptr_eq(&self.sim.0, &socket.sim.0) {
            return Err(errno(libc::EXDEV));
        }
        let bytes = {
            let w = self.sim.0.borrow();
            let Some(Resource::Pipe {
                bytes,
                write: false,
                ..
            }) = w.resources.get(&self.id)
            else {
                return Err(errno(libc::EBADF));
            };
            bytes.clone()
        };
        let mut input = bytes.borrow_mut();
        let count = count.min(input.len());
        let sent = socket.send(&input.make_contiguous()[..count])?;
        input.drain(..sent);
        Ok(sent)
    }
}

fn check_direct(flags: i32, offset: u64, ptr: *const u8, length: usize) -> io::Result<()> {
    if flags & libc::O_DIRECT != 0
        && (!offset.is_multiple_of(4096)
            || !(ptr as usize).is_multiple_of(4096)
            || !length.is_multiple_of(4096))
    {
        return Err(errno(libc::EINVAL));
    }
    Ok(())
}

pub(super) enum Op {
    Buffer {
        fd: Rc<Descriptor>,
        operation: BufferOperation,
        ptr: *mut u8,
        len: usize,
    },
    Poll {
        fd: Rc<Descriptor>,
        interest: u32,
    },
    Accept(Rc<Descriptor>),
    Connect {
        fd: Rc<Descriptor>,
        address: SocketAddress,
    },
    Open {
        dir: Option<Rc<Descriptor>>,
        path: CString,
        flags: i32,
        resolve: u64,
    },
    Stat {
        fd: Rc<Descriptor>,
        ptr: *mut libc::statx,
    },
    Sync(Rc<Descriptor>),
    Mkdir {
        dir: Rc<Descriptor>,
        name: CString,
    },
    Rename {
        dir: Rc<Descriptor>,
        from: CString,
        to: CString,
    },
    Unlink {
        dir: Rc<Descriptor>,
        name: CString,
    },
}
impl Op {
    fn disk_paths(&self, sim: &Simulation) -> Vec<PathBuf> {
        let w = sim.0.borrow();
        let file_path = |fd: &Descriptor| match fd {
            fd if fd.as_sim().is_some() => match w.resources.get(&fd.as_sim().unwrap().id) {
                Some(Resource::File { opened_path, .. }) => Some(opened_path.clone()),
                _ => None,
            },
            _ => None,
        };
        let path = |dir: Option<&Descriptor>, name: &CString| {
            use std::os::unix::ffi::OsStrExt;
            w.path(dir, Path::new(std::ffi::OsStr::from_bytes(name.as_bytes())))
                .ok()
        };
        match self {
            Self::Buffer {
                fd,
                operation: BufferOperation::Read(_) | BufferOperation::Write(_),
                ..
            }
            | Self::Stat { fd, .. }
            | Self::Sync(fd) => file_path(fd).into_iter().collect(),
            Self::Open {
                dir, path: name, ..
            } => path(dir.as_deref(), name).into_iter().collect(),
            Self::Mkdir { dir, name } | Self::Unlink { dir, name } => {
                path(Some(dir), name).into_iter().collect()
            }
            Self::Rename { dir, from, to } => [path(Some(dir), from), path(Some(dir), to)]
                .into_iter()
                .flatten()
                .collect(),
            _ => Vec::new(),
        }
    }
    fn name(&self) -> &'static str {
        match self {
            Self::Buffer { operation, .. } => match operation {
                BufferOperation::Read(_) => "read",
                BufferOperation::Write(_) => "write",
                BufferOperation::Recv => "recv",
                BufferOperation::Send => "send",
            },
            Self::Poll { .. } => "poll",
            Self::Accept(_) => "accept",
            Self::Connect { .. } => "connect",
            Self::Open { .. } => "open",
            Self::Stat { .. } => "stat",
            Self::Sync(_) => "fsync",
            Self::Mkdir { .. } => "mkdir",
            Self::Rename { .. } => "rename",
            Self::Unlink { .. } => "unlink",
        }
    }
    fn execute(&self, sim: &Simulation, limit: usize) -> io::Result<KernelResult> {
        use std::os::unix::ffi::OsStrExt;
        let path = |name: &CString| PathBuf::from(std::ffi::OsStr::from_bytes(name.as_bytes()));
        let handle = |fd: &Rc<Descriptor>| match &**fd {
            fd if fd.as_sim().is_some_and(|h| Rc::ptr_eq(&h.sim.0, &sim.0)) => {
                Ok(fd.as_sim().unwrap().id)
            }
            _ => Err(errno(libc::EXDEV)),
        };
        // Temporary handles are not owners; only call through references below.
        let h = |fd: &Rc<Descriptor>| {
            handle(fd)?;
            Ok::<_, io::Error>(())
        };
        match self {
            Self::Buffer {
                fd,
                operation,
                ptr,
                len,
            } => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                if let BufferOperation::Read(offset) | BufferOperation::Write(offset) = operation {
                    let (_, flags) = fd.node()?;
                    check_direct(flags, *offset, *ptr, *len)?;
                }
                let len = (*len).min(limit).min(i32::MAX as usize);
                let n = match operation {
                    // SAFETY: receive/read entries own exclusive IoBuffers through
                    // both fences; immutable sends may have shared aliases.
                    BufferOperation::Read(offset) => fd.file_read(*offset, unsafe {
                        std::slice::from_raw_parts_mut(*ptr, len)
                    }),
                    BufferOperation::Recv => {
                        fd.recv(unsafe { std::slice::from_raw_parts_mut(*ptr, len) })
                    }
                    BufferOperation::Write(offset) => {
                        fd.file_write(*offset, unsafe { std::slice::from_raw_parts(*ptr, len) })
                    }
                    BufferOperation::Send => {
                        fd.send(unsafe { std::slice::from_raw_parts(*ptr, len) })
                    }
                }?;
                Ok(KernelResult::Value(n as i32))
            }
            Self::Poll { fd, interest } => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                fd.ready(*interest).map(KernelResult::Value)
            }
            Self::Accept(fd) => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                fd.accept().map(KernelResult::Accepted)
            }
            Self::Connect { fd, address } => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                fd.connect(address)?;
                Ok(KernelResult::Value(0))
            }
            Self::Open {
                dir,
                path: name,
                flags,
                resolve,
            } => sim
                .open_resolved(dir.as_deref(), &path(name), *flags, *resolve)
                .map(KernelResult::Accepted),
            Self::Stat { fd, ptr } => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                unsafe {
                    **ptr = fd.stat()?;
                }
                Ok(KernelResult::Value(0))
            }
            Self::Sync(fd) => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                fd.sync()?;
                Ok(KernelResult::Value(0))
            }
            Self::Mkdir { dir, name } => {
                h(dir)?;
                let path = sim.0.borrow().path(Some(dir), &path(name))?;
                if sim.0.borrow().paths.contains_key(&path) {
                    return Err(errno(libc::EEXIST));
                }
                sim.create_dir_all(&path)?;
                sim.chmod(&path, 0o700)?;
                Ok(KernelResult::Value(0))
            }
            Self::Rename { dir, from, to } => {
                h(dir)?;
                let a = sim.0.borrow().path(Some(dir), &path(from))?;
                let b = sim.0.borrow().path(Some(dir), &path(to))?;
                sim.rename(&a, &b, 0)?;
                Ok(KernelResult::Value(0))
            }
            Self::Unlink { dir, name } => {
                h(dir)?;
                let path = sim.0.borrow().path(Some(dir), &path(name))?;
                sim.unlink(&path)?;
                Ok(KernelResult::Value(0))
            }
        }
    }
}

struct Pending {
    op: Op,
    delay: usize,
    limit: usize,
    error: Option<i32>,
    hold: usize,
    result: Option<KernelResult>,
    disk_generation: u64,
    disk_paths: Vec<PathBuf>,
}
pub(super) struct Driver {
    sim: Simulation,
    pending: RefCell<BTreeMap<u64, Pending>>,
    completed: RefCell<VecDeque<(u64, KernelResult)>>,
}
impl Driver {
    pub fn new(sim: Simulation) -> Self {
        Self {
            sim,
            pending: RefCell::default(),
            completed: RefCell::default(),
        }
    }
    pub fn push(&mut self, id: u64, op: Op) {
        let name = op.name();
        let fault = self.sim.0.borrow_mut().fault(name);
        let mut pending = Pending {
            disk_generation: self.sim.0.borrow().disk.generation,
            disk_paths: op.disk_paths(&self.sim),
            op,
            delay: 0,
            limit: usize::MAX,
            error: None,
            hold: 0,
            result: None,
        };
        match fault {
            Some(Fault::Errno(n)) => pending.error = Some(n),
            Some(Fault::Short(n)) => pending.limit = n,
            Some(Fault::Delay(n)) => pending.delay = n,
            Some(Fault::HoldCompletion(n)) => pending.hold = n,
            None => (),
        }
        self.sim
            .0
            .borrow_mut()
            .record(&format!("submit:{name}"), id, 0);
        self.pending.borrow_mut().insert(id, pending);
    }
    pub fn cancel(&mut self, id: u64) {
        let mut pending = self.pending.borrow_mut();
        // An executed operation cannot be canceled retroactively, nor may its
        // held CQE be replaced with ECANCELED and release the owners early.
        let removed = pending.get(&id).is_some_and(|p| p.result.is_none());
        if removed {
            pending.remove(&id);
        }
        let mut completed = self.completed.borrow_mut();
        // Deliberately emit cancel first, exercising the shared two-CQE fence.
        completed.push_back((
            id | CANCEL_BIT,
            KernelResult::Value(if removed { 0 } else { -libc::ENOENT }),
        ));
        if removed {
            completed.push_back((id, KernelResult::Value(-libc::ECANCELED)));
            if !self.sim.0.borrow().cancel_first {
                let len = completed.len();
                completed.swap(len - 2, len - 1);
            }
        }
        self.sim
            .0
            .borrow_mut()
            .record("cancel", id, i64::from(removed));
    }
    pub fn submit(&self) {
        let mut pending = self.pending.borrow_mut();
        let mut done = Vec::new();
        for (&id, operation) in pending.iter_mut() {
            if operation.result.is_some() {
                if operation.hold != 0 {
                    operation.hold -= 1;
                    continue;
                }
                self.completed
                    .borrow_mut()
                    .push_back((id, operation.result.take().unwrap()));
                done.push(id);
                continue;
            }
            if operation.delay != 0 {
                operation.delay -= 1;
                continue;
            }
            self.sim.0.borrow_mut().executing = true;
            let injected = operation.error.take();
            let result = match injected {
                Some(n) => Err(errno(n)),
                None if self
                    .sim
                    .0
                    .borrow()
                    .disk
                    .crashed_since(operation.disk_generation, &operation.disk_paths) =>
                {
                    Err(errno(libc::EIO))
                }
                None => operation.op.execute(&self.sim, operation.limit),
            };
            self.sim.0.borrow_mut().executing = false;
            match result {
                Err(error)
                    if injected.is_none()
                        && error.kind() == io::ErrorKind::WouldBlock
                        && matches!(
                            operation.op,
                            Op::Buffer {
                                operation: BufferOperation::Recv | BufferOperation::Send,
                                ..
                            } | Op::Poll { .. }
                                | Op::Accept(_)
                                | Op::Connect { .. }
                        ) =>
                {
                    ()
                }
                result => {
                    let result = result.unwrap_or_else(|error| {
                        KernelResult::Value(-error.raw_os_error().unwrap_or(libc::EIO))
                    });
                    let value = match &result {
                        KernelResult::Value(n) => *n as i64,
                        KernelResult::Accepted(fd) if fd.as_sim().is_some() => {
                            fd.as_sim().unwrap().id as i64
                        }
                        _ => unreachable!(),
                    };
                    self.sim.0.borrow_mut().record(
                        &format!("complete:{}", operation.op.name()),
                        id,
                        value,
                    );
                    if operation.hold != 0 {
                        operation.result = Some(result);
                    } else {
                        self.completed.borrow_mut().push_back((id, result));
                        done.push(id);
                    }
                }
            }
        }
        for id in done {
            pending.remove(&id);
        }
    }
    pub fn pop(&mut self) -> Option<(u64, KernelResult)> {
        self.completed.borrow_mut().pop_front()
    }
}
