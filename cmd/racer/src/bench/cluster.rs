//! A racer cluster, brought up for measurement and torn down after.
//!
//! Every node is a separate process, as in production and in `tests/cluster.rs`, and its
//! store is an image file on an ext4 file system of its own, on a ram disk: storage at
//! memory speed leaves the CPU as the only thing that can be the limit, which is the
//! question being asked.

use std::io::{BufRead, BufReader};
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdout, Command, Stdio};

use racer::config::Config;

pub const ROOT: &str = "/tmp/racer-bench";
/// Device ids, in config order. Each maps one extent of the universe.
pub const LWW: u32 = 1;
pub const BIG: u32 = 2;

/// How the cluster is laid out. `racer-bench e2e` overrides `nodes`, `cores`, `groups`
/// and `small_pages` from its flags and takes the rest from here.
#[derive(Clone)]
pub struct Plan {
    /// 1 for a node alone (no peers, so quorum is one and nothing crosses the fabric)
    /// or 3 for a real consensus group.
    pub nodes: u32,
    /// Physical cores per node process. The runtime runs one worker per physical core in
    /// its affinity mask, so this is the only knob there is.
    pub cores: usize,
    /// The first physical core to hand out. Nodes take `cores` each from here in order,
    /// and the load generator takes the ones above them.
    pub first_core: usize,
    /// 4 KiB pages in the LWW extent.
    pub small_pages: u64,
    /// 4 MiB pages in the immutable extent.
    pub huge_pages: u64,
    /// Consensus groups in the catalog. A group's allocator index shard and its
    /// consensus state both live on core `group % cores`, so fewer groups than cores
    /// leaves cores with nothing to do.
    pub groups: usize,
    pub cache_4k: u64,
    pub cache_4m: u64,
    /// Zero disables the cooperative cache, which is what a member-only read wants.
    pub cache_target_rate: u32,
}

impl Default for Plan {
    fn default() -> Plan {
        Plan {
            nodes: 3,
            cores: 2,
            first_core: 0,
            small_pages: 262_144,
            huge_pages: 512,
            groups: 12,
            cache_4k: 0,
            cache_4m: 0,
            cache_target_rate: 0,
        }
    }
}

impl Plan {
    /// The bytes of each extent a workload touches: three quarters of it, so
    /// out-of-place writes always have free slots, since slabs carry only 5% spare.
    pub fn small_bytes(&self) -> u64 {
        self.small_pages / 4 * 3 * 4096
    }

    pub fn huge_bytes(&self) -> u64 {
        self.huge_pages / 4 * 3 * (4 << 20)
    }

    /// The first core above the node processes', which is the client's.
    pub fn client_first_core(&self) -> usize {
        self.first_core + self.nodes as usize * self.cores
    }
}

pub struct Cluster {
    pub nodes: Vec<Node>,
}

pub struct Node {
    pub id: u32,
    dir: PathBuf,
    store: PathBuf,
    child: Option<Child>,
    out: Option<BufReader<ChildStdout>>,
    devices: Vec<(u32, PathBuf)>,
    fabric: PathBuf,
    pub metrics: String,
}

impl Node {
    /// This node's block device `id`.
    pub fn device(&self, id: u32) -> &Path {
        &self
            .devices
            .iter()
            .find(|(v, _)| *v == id)
            .expect("device")
            .1
    }
}

impl Cluster {
    /// Lay down fresh stores, start the nodes and wire them to each other.
    pub fn start(plan: &Plan) -> std::io::Result<Cluster> {
        let racer = racer_bin();
        // A run killed before its `Drop` leaves nodes spinning on the ram disks, which
        // would corrupt the next run's measurements as well as its data.
        let _ = Command::new("pkill")
            .args(["-9", "-f", &format!("racer serve {ROOT}/")])
            .status();
        // A fresh file system per node, so every run formats a blank store rather than
        // finding one an earlier run left behind.
        let images = stores(plan.nodes as usize)?;

        let mut nodes = Vec::new();
        for (i, store) in images.into_iter().enumerate() {
            let id = i as u32 + 1;
            let dir = PathBuf::from(ROOT).join(format!("n{id}"));
            std::fs::create_dir_all(&dir)?;
            nodes.push(Node {
                id,
                dir,
                store,
                child: None,
                out: None,
                devices: Vec::new(),
                fabric: PathBuf::new(),
                metrics: String::new(),
            });
        }

        // Generation 1 names no peer: a peer's fabric device does not exist until its
        // process is running.
        for n in &nodes {
            install(n, &text(plan, n, &[], 1))?;
        }
        for (i, n) in nodes.iter_mut().enumerate() {
            let lo = plan.first_core + i * plan.cores;
            n.serve(&racer, lo, lo + plan.cores)?;
        }
        if plan.nodes > 1 {
            let fabrics: Vec<(u32, PathBuf)> =
                nodes.iter().map(|n| (n.id, n.fabric.clone())).collect();
            for n in &mut nodes {
                let peers: Vec<(u32, PathBuf)> = fabrics
                    .iter()
                    .filter(|(id, _)| *id != n.id)
                    .cloned()
                    .collect();
                let t = text(plan, n, &peers, 2);
                install(n, &t)?;
                n.await_reload();
            }
        }
        Ok(Cluster { nodes })
    }
}

impl Node {
    /// Start `racer serve` pinned to physical cores `lo..hi`, and read back the devices
    /// it published.
    fn serve(&mut self, racer: &Path, lo: usize, hi: usize) -> std::io::Result<()> {
        // Both SMT siblings of every core: the runtime folds them back to one worker per
        // physical core and parks the control thread on the spare sibling.
        let cpus: Vec<usize> = (lo..hi).flat_map(|c| [c * 2, c * 2 + 1]).collect();
        let mut cmd = Command::new(racer);
        cmd.arg("serve")
            .arg(self.dir.join("node.pb"))
            .env("METRICS_ADDR", "127.0.0.1:0")
            // The store's path is this process's own, so it is passed in rather than
            // named in a generation every node shares.
            .env(racer::config::STORE_PATH_ENV, &self.store)
            .stdout(Stdio::piped());
        unsafe {
            cmd.pre_exec(move || {
                let mut set: libc::cpu_set_t = std::mem::zeroed();
                libc::CPU_ZERO(&mut set);
                for &c in &cpus {
                    libc::CPU_SET(c, &mut set);
                }
                libc::sched_setaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &set);
                Ok(())
            })
        };
        let mut child = cmd.spawn()?;
        let mut out = BufReader::new(child.stdout.take().unwrap());
        self.devices.clear();
        loop {
            let line = next_line(&mut out, self.id);
            if let Some(rest) = line.strip_prefix("metrics -> ") {
                self.metrics = rest.to_string();
            } else if let Some(rest) = line.strip_prefix("device ") {
                let (id, path) = rest.split_once(" -> ").expect("device line");
                self.devices
                    .push((id.parse().unwrap(), PathBuf::from(path)));
            } else if let Some(rest) = line.strip_prefix("universe ") {
                // One universe here, and its fabric device is the last line printed.
                let (_, path) = rest.split_once(" fabric -> ").expect("universe line");
                self.fabric = PathBuf::from(path);
                break;
            }
        }
        for (_, p) in &self.devices {
            wait_for(p);
        }
        wait_for(&self.fabric);
        self.child = Some(child);
        self.out = Some(out);
        Ok(())
    }

    fn await_reload(&mut self) {
        let out = self.out.as_mut().unwrap();
        while next_line(out, self.id) != "racer: configuration applied" {}
    }

    fn signal(&mut self, sig: i32) {
        if let Some(c) = &self.child {
            unsafe { libc::kill(c.id() as i32, sig) };
        }
    }
}

impl Drop for Cluster {
    /// Nodes hold each other's fabric devices open and a ublk device cannot be torn down
    /// while someone has it open, so they go down together.
    fn drop(&mut self) {
        for n in &mut self.nodes {
            n.signal(libc::SIGTERM);
        }
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(15);
        for n in &mut self.nodes {
            while std::time::Instant::now() < deadline {
                match n.child.as_mut().map(|c| c.try_wait().unwrap()) {
                    None | Some(Some(_)) => break,
                    Some(None) => std::thread::sleep(std::time::Duration::from_millis(20)),
                }
            }
            n.signal(libc::SIGKILL);
            if let Some(mut c) = n.child.take() {
                let _ = c.wait();
            }
        }
    }
}

// ---------------------------------------------------------------------------
// configuration
// ---------------------------------------------------------------------------

/// One universe holding an LWW extent of 4 KiB pages and an immutable extent of 4 MiB
/// ones: the two page classes, one device each. OCC is left out because its cost is the
/// read it requires, which the LWW extent already measures.
fn text(plan: &Plan, n: &Node, peers: &[(u32, PathBuf)], generation: u32) -> String {
    let mut s = format!(
        "generation {generation}\nnode id={} zone=1 cohort={} size={STORE_BYTES} cache_4k={} cache_4m={}\n",
        n.id,
        (n.id - 1) % 3,
        plan.cache_4k,
        plan.cache_4m
    );
    s += &format!("policy cache_target_rate={}\n", plan.cache_target_rate);
    s += "universe 1 epoch=1\n";
    for (id, dev) in peers {
        s += &format!("peer id={id} device={}\n", dev.display());
    }
    // Every group is the same three nodes in a different order: one replica set, many
    // groups sharing it. A lone node is a member of all of them and, having no peers,
    // proposes and reads by itself at quorum one.
    const ORDER: [[u32; 3]; 6] = [
        [1, 2, 3],
        [2, 3, 1],
        [3, 1, 2],
        [1, 3, 2],
        [3, 2, 1],
        [2, 1, 3],
    ];
    for i in 0..plan.groups {
        let g = ORDER[i % ORDER.len()];
        s += &format!("group {} {} {}\n", g[0], g[1], g[2]);
    }
    // The huge extent starts on a 4 MiB boundary above the small one: a 4 MiB page is
    // 1024 blocks of the universe's flat address space.
    let huge_base = plan.small_pages.next_multiple_of(1024);
    s += &format!(
        "extent id=1 base=0 pages={} kind=lww zone=1\n\
         extent id=2 base={huge_base} pages={} kind=immutable_4m zone=1\n\
         device {LWW} extents=1\ndevice {BIG} extents=2\n",
        plan.small_pages, plan.huge_pages
    );
    s
}

fn install(n: &Node, text: &str) -> std::io::Result<()> {
    let cfg = Config::parse(text)?;
    cfg.validate()?;
    let tmp = n.dir.join("node.next");
    std::fs::write(&tmp, cfg.encode())?;
    std::fs::rename(&tmp, n.dir.join("node.pb"))
}

// ---------------------------------------------------------------------------
// backing store
// ---------------------------------------------------------------------------

/// Bytes per ram disk: room for a node's file system and the store image on it. `brd`
/// allocates a page only when written, so an unused tail costs nothing.
const RD_BYTES: u64 = 8 << 30;

/// Bytes of the store image on each node's file system: the default plan's extents plus
/// the metadata and over-provisioning around them, with the rest of the ram disk left to
/// ext4 for its inode table, journal and group metadata.
pub const STORE_BYTES: u64 = RD_BYTES - (1 << 30);

/// One backing store per node: a fresh ext4 file system on each ram disk, with an empty
/// image file of `STORE_BYTES` already reserved on it. Call this before anything opens a
/// ram disk: `brd` cannot be reloaded while a disk is in use.
///
/// The file systems are made again on every call, which is how a run is guaranteed a
/// blank store. A node reformats only a store whose superblock region reads as zeros, so
/// anything left over would be adopted, index and all.
pub fn stores(n: usize) -> std::io::Result<Vec<PathBuf>> {
    // An earlier run's mounts pin both the ram disk under them and the directory tree
    // above, so they are the first thing to go. More than this run needs, because the
    // previous one may have been larger.
    for i in 0..n.max(4) {
        unmount(&mount_dir(i));
    }
    let devices = ram_disks(n)?;
    let _ = std::fs::remove_dir_all(ROOT);
    std::fs::create_dir_all(ROOT)?;

    let mut images = Vec::new();
    for (i, dev) in devices.iter().enumerate() {
        let mnt = mount_dir(i);
        std::fs::create_dir_all(&mnt)?;
        mkfs(dev)?;
        mount(dev, &mnt)?;
        let img = mnt.join("store.img");
        // Reserved rather than written: the file is sparse until racer formats it, and
        // a ram disk only spends a page once one is stored in it.
        std::fs::File::create(&img)?.set_len(STORE_BYTES)?;
        images.push(img);
    }
    Ok(images)
}

fn mount_dir(i: usize) -> PathBuf {
    PathBuf::from(ROOT).join(format!("fs{i}"))
}

fn mkfs(dev: &Path) -> std::io::Result<()> {
    // One 4 KiB block, few inodes and no journal: the file system holds a single large
    // file opened with O_DIRECT, and everything else on it is waste.
    let rc = Command::new("mkfs.ext4")
        .args(["-q", "-F", "-b", "4096", "-N", "64", "-O", "^has_journal"])
        .arg(dev)
        .status()?;
    assert!(rc.success(), "mkfs.ext4 {} failed", dev.display());
    Ok(())
}

fn mount(dev: &Path, at: &Path) -> std::io::Result<()> {
    let rc = Command::new("mount")
        .args(["-t", "ext4", "-o", "noatime"])
        .arg(dev)
        .arg(at)
        .status()?;
    assert!(
        rc.success(),
        "mount {} at {} failed",
        dev.display(),
        at.display()
    );
    Ok(())
}

/// Detach `at` if it is a mount point, and say nothing if it is not: this is called on
/// paths that may never have existed.
fn unmount(at: &Path) {
    if at.exists() {
        let _ = Command::new("umount").arg("-l").arg(at).status();
    }
}

/// `n` ram disks, loading `brd` if it is missing or the wrong size.
fn ram_disks(n: usize) -> std::io::Result<Vec<PathBuf>> {
    let paths: Vec<PathBuf> = (0..n)
        .map(|i| PathBuf::from(format!("/dev/ram{i}")))
        .collect();
    let ok = paths.iter().all(|p| size_of(p) == Some(RD_BYTES));
    if !ok {
        let _ = Command::new("modprobe").args(["-r", "brd"]).status();
        let rc = Command::new("modprobe")
            .arg("brd")
            .arg(format!("rd_nr={}", n.max(4)))
            .arg(format!("rd_size={}", RD_BYTES / 1024))
            .arg("max_part=1")
            .status()?;
        assert!(
            rc.success(),
            "modprobe brd failed; is a ram disk still in use?"
        );
        for p in &paths {
            wait_for(p);
        }
    }
    for p in &paths {
        let got = size_of(p);
        assert_eq!(
            got,
            Some(RD_BYTES),
            "{} is {got:?} bytes, not {RD_BYTES}",
            p.display()
        );
    }
    Ok(paths)
}

fn size_of(p: &Path) -> Option<u64> {
    let f = std::fs::File::open(p).ok()?;
    let mut n: u64 = 0;
    // BLKGETSIZE64
    let rc = unsafe { libc::ioctl(std::os::fd::AsRawFd::as_raw_fd(&f), 0x8008_1272, &mut n) };
    if rc == 0 { Some(n) } else { None }
}

// ---------------------------------------------------------------------------
// plumbing
// ---------------------------------------------------------------------------

/// The `racer` binary beside this one.
fn racer_bin() -> PathBuf {
    let me = std::env::current_exe().expect("current_exe");
    let p = me.parent().expect("a bin directory").join("racer");
    assert!(
        p.exists(),
        "{} is missing; build the `racer` binary too",
        p.display()
    );
    p
}

fn next_line(out: &mut BufReader<ChildStdout>, id: u32) -> String {
    let mut s = String::new();
    let n = out.read_line(&mut s).expect("read node stdout");
    assert!(n > 0, "node {id} exited before publishing its devices");
    s.trim_end().to_string()
}

fn wait_for(p: &Path) {
    for _ in 0..1000 {
        if p.exists() {
            return;
        }
        std::thread::sleep(std::time::Duration::from_millis(10));
    }
    panic!("{} never appeared", p.display());
}
