//! The block storage a simulated node is built on.
//!
//! A [`Device`] is a sparse map from block number to bytes, which is why a
//! node's store and its handle on a peer are the same type: the store holds
//! blocks, the handle holds none and the runtime turns writes to it into
//! frames instead. Every block a store holds is interned in the one
//! [`BlockPool`], so the simulation costs what the cluster wrote rather than
//! what it copied.
//!
//! This sits below the file seam. What the node above sees is a path it opened
//! and offsets it read and wrote, exactly as it would see a store on a disk.

use std::collections::BTreeMap;
use std::io;
use std::path::{Path, PathBuf};
use std::rc::Rc;

/// The unit a simulated store is addressed in, and the size of a page the
/// allocator checksums.
pub(crate) const BLOCK: usize = 4096;

/// Interns between pool sweeps. Sweeping is a full walk, so it wants to be
/// rare, and a block that outlives a sweep costs nothing but a slot.
const SWEEP: u32 = 4096;

type Block = Rc<[u8; BLOCK]>;

/// Content-addressed storage for every block every simulated store holds.
///
/// A cluster writes the same page to three nodes; a fill writes the same
/// thousand blocks to all of them. Interning means the run pays once.
#[derive(Default)]
pub(crate) struct BlockPool {
    by_hash: BTreeMap<u64, Vec<Block>>,
    since: u32,
}

impl BlockPool {
    fn intern(&mut self, src: &[u8]) -> Block {
        self.since += 1;

        if self.since >= SWEEP {
            self.sweep();
        }

        let v = self.by_hash.entry(spread(src)).or_default();

        if let Some(b) = v.iter().find(|b| b[..] == *src) {
            return Rc::clone(b);
        }

        let mut b = [0u8; BLOCK];

        b.copy_from_slice(src);

        let b: Block = Rc::new(b);

        v.push(Rc::clone(&b));

        b
    }

    /// Drops the blocks no store holds any more. The pool's own reference is
    /// discounted, so a count of one means nobody else kept it.
    fn sweep(&mut self) {
        self.since = 0;
        self.by_hash.retain(|_, v| {
            v.retain(|b| Rc::strong_count(b) > 1);
            !v.is_empty()
        });
    }
}

/// FNV-1a, a word at a time. Not a checksum: it only has to spread contents
/// over buckets, and a collision costs a memcmp.
fn spread(b: &[u8]) -> u64 {
    let mut h = 0xcbf2_9ce4_8422_2325u64;

    for w in b.chunks_exact(8) {
        h = (h ^ u64::from_le_bytes(w.try_into().unwrap())).wrapping_mul(0x0100_0000_01b3);
    }

    h
}

/// One simulated block device: a node's own store, or its handle on a peer's.
pub(crate) struct Device {
    /// The node the device belongs to.
    pub(crate) node: u32,
    /// Whether this is a handle on a peer rather than a store of our own.
    pub(crate) fabric: bool,
    blocks: BTreeMap<u64, Block>,
    /// Store size. Zero until the node sizes it, and unused on a handle.
    len: u64,
}

impl Device {
    fn new(node: u32, fabric: bool) -> Device {
        Device {
            node,
            fabric,
            blocks: BTreeMap::new(),
            len: 0,
        }
    }

    /// Reads one block. A block that was never written reads as a hole, which
    /// is what a freshly allocated store does.
    pub(crate) fn read(&self, lba: u64, dst: &mut [u8]) {
        match self.blocks.get(&lba) {
            Some(b) => dst.copy_from_slice(&b[..dst.len()]),
            None => dst.fill(0),
        }
    }

    /// Writes one block. An all-zero write erases, so a trimmed page costs
    /// nothing and reads back as the hole it became.
    pub(crate) fn write(&mut self, pool: &mut BlockPool, lba: u64, src: &[u8]) {
        if src.iter().all(|&b| b == 0) {
            self.blocks.remove(&lba);
            return;
        }

        self.blocks.insert(lba, pool.intern(src));
    }

    /// Sizes the store. Grow-only, for the same reason a real one is: the
    /// layout's offsets are absolute, so shrinking would move every extent.
    fn resize(&mut self, want: u64) -> io::Result<()> {
        if self.len > want {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!(
                    "store is {} B, node.store.size_bytes is {want} B; a store cannot shrink",
                    self.len
                ),
            ));
        }

        self.len = want;

        Ok(())
    }
}

/// Every simulated device in the run, and the paths they answer to.
///
/// A path is resolved once and remembered, so a node that reopens its store
/// after a crash finds the blocks it left behind.
#[derive(Default)]
pub(super) struct Store {
    devs: Vec<Device>,
    pool: BlockPool,
    paths: BTreeMap<PathBuf, u32>,
}

impl Store {
    /// Resolves a path to a device id, creating the device the first time.
    ///
    /// The simulator names devices `/sim/n{node}/{store,fabric}`. Anything
    /// else is `ENOENT`, which is what a node opening a path that does not
    /// exist would see.
    pub(super) fn open(&mut self, path: &Path) -> io::Result<u32> {
        if let Some(&id) = self.paths.get(path) {
            return Ok(id);
        }

        let (node, fabric) = parse(path).ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::NotFound,
                format!("no simulated device at {}", path.display()),
            )
        })?;

        let id = self.devs.len() as u32;

        self.devs.push(Device::new(node, fabric));
        self.paths.insert(path.to_path_buf(), id);

        Ok(id)
    }

    /// The device a path already names, if it names one.
    #[allow(dead_code)]
    pub(super) fn find(&self, path: &Path) -> Option<u32> {
        self.paths.get(path).copied()
    }

    pub(super) fn dev(&self, id: u32) -> &Device {
        &self.devs[id as usize]
    }

    pub(super) fn len(&self, id: u32) -> u64 {
        self.devs[id as usize].len
    }

    pub(super) fn resize(&mut self, id: u32, want: u64) -> io::Result<()> {
        self.devs[id as usize].resize(want)
    }

    /// Reads a byte range, which the caller has already sized to whole blocks.
    pub(super) fn read_at(&self, id: u32, off: u64, out: &mut [u8]) {
        let base = off / BLOCK as u64;
        let d = &self.devs[id as usize];

        for (i, chunk) in out.chunks_mut(BLOCK).enumerate() {
            d.read(base + i as u64, chunk);
        }
    }

    /// Writes a byte range, which the caller has already sized to whole blocks.
    pub(super) fn write_at(&mut self, id: u32, off: u64, src: &[u8]) {
        let base = off / BLOCK as u64;
        let d = &mut self.devs[id as usize];

        for (i, chunk) in src.chunks(BLOCK).enumerate() {
            d.write(&mut self.pool, base + i as u64, chunk);
        }
    }
}

/// Parses `/sim/n{node}/{store,fabric}`.
fn parse(path: &Path) -> Option<(u32, bool)> {
    let s = path.to_str()?;
    let rest = s.strip_prefix("/sim/n")?;
    let (node, kind) = rest.split_once('/')?;
    let fabric = match kind {
        "store" => false,
        "fabric" => true,
        _ => return None,
    };

    Some((node.parse().ok()?, fabric))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_block_that_was_never_written_reads_as_a_hole() {
        let mut s = Store::default();
        let d = s.open(Path::new("/sim/n1/store")).unwrap();
        let mut page = [0xffu8; BLOCK];

        s.read_at(d, 0, &mut page);

        assert!(page.iter().all(|&b| b == 0));
    }

    #[test]
    fn a_written_block_reads_back_and_a_zeroed_one_becomes_a_hole_again() {
        let mut s = Store::default();
        let d = s.open(Path::new("/sim/n1/store")).unwrap();
        let page = [7u8; BLOCK];

        s.write_at(d, BLOCK as u64, &page);

        let mut back = [0u8; BLOCK];

        s.read_at(d, BLOCK as u64, &mut back);
        assert_eq!(back, page);

        s.write_at(d, BLOCK as u64, &[0u8; BLOCK]);
        back.fill(0xff);
        s.read_at(d, BLOCK as u64, &mut back);
        assert!(back.iter().all(|&b| b == 0));
    }

    #[test]
    fn the_same_contents_on_two_devices_are_stored_once() {
        let mut s = Store::default();
        let a = s.open(Path::new("/sim/n1/store")).unwrap();
        let b = s.open(Path::new("/sim/n2/store")).unwrap();
        let page = [3u8; BLOCK];

        s.write_at(a, 0, &page);
        s.write_at(b, 0, &page);

        let one = s.dev(a).blocks.get(&0).cloned().unwrap();
        let two = s.dev(b).blocks.get(&0).cloned().unwrap();

        assert!(Rc::ptr_eq(&one, &two));
    }

    #[test]
    fn a_path_resolves_to_the_same_device_every_time() {
        let mut s = Store::default();
        let first = s.open(Path::new("/sim/n3/store")).unwrap();
        let again = s.open(Path::new("/sim/n3/store")).unwrap();

        assert_eq!(first, again);
        assert_eq!(s.find(Path::new("/sim/n3/store")), Some(first));
        assert_eq!(s.find(Path::new("/sim/n4/store")), None);
    }

    #[test]
    fn a_store_and_a_fabric_handle_are_told_apart_by_their_path() {
        let mut s = Store::default();
        let store = s.open(Path::new("/sim/n2/store")).unwrap();
        let fabric = s.open(Path::new("/sim/n2/fabric")).unwrap();

        assert_eq!(s.dev(store).node, 2);
        assert!(!s.dev(store).fabric);
        assert_eq!(s.dev(fabric).node, 2);
        assert!(s.dev(fabric).fabric);
    }

    #[test]
    fn a_path_that_is_not_a_device_is_not_found() {
        let mut s = Store::default();

        assert_eq!(
            s.open(Path::new("/var/lib/racer/store.img"))
                .unwrap_err()
                .kind(),
            io::ErrorKind::NotFound
        );
        assert!(s.open(Path::new("/sim/n1/other")).is_err());
        assert!(s.open(Path::new("/sim/nx/store")).is_err());
    }

    #[test]
    fn a_store_grows_and_never_shrinks() {
        let mut s = Store::default();
        let d = s.open(Path::new("/sim/n1/store")).unwrap();

        assert_eq!(s.len(d), 0);
        s.resize(d, 1 << 20).unwrap();
        assert_eq!(s.len(d), 1 << 20);
        s.resize(d, 1 << 20).unwrap();
        s.resize(d, 1 << 21).unwrap();
        assert_eq!(s.len(d), 1 << 21);
        assert_eq!(
            s.resize(d, 1 << 20).unwrap_err().kind(),
            io::ErrorKind::InvalidInput
        );
        assert_eq!(s.len(d), 1 << 21);
    }

    #[test]
    fn the_pool_releases_blocks_no_store_holds() {
        let mut s = Store::default();
        let d = s.open(Path::new("/sim/n1/store")).unwrap();

        for i in 0..SWEEP as u64 + 1 {
            let mut page = [0u8; BLOCK];

            page[..8].copy_from_slice(&i.to_le_bytes());
            page[8] = 1;
            s.write_at(d, i * BLOCK as u64, &page);
            s.write_at(d, i * BLOCK as u64, &[0u8; BLOCK]);
        }

        let held: usize = s.pool.by_hash.values().map(Vec::len).sum();

        assert!(held < SWEEP as usize, "the pool kept {held} dead blocks");
    }
}
