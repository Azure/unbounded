//! A cluster of racer nodes in one address space.
//!
//! Every node here boots the way the binary boots: format the store, start the runtime,
//! install the first generation. Nothing is stubbed and nothing is special-cased, because
//! the whole point of the kernel seam is that a node cannot tell which kernel it got. What
//! this module supplies is the two things a node cannot supply for itself: the
//! configuration text describing the cluster it belongs to, and the turns its threads take.

use std::rc::Rc;
use std::sync::Arc;

use crate::kernel::{self, Kernel, sim};
use crate::{config, config::Config, layout, runtime, server};

/// The shape of a simulated cluster.
#[derive(Clone, Copy, Debug)]
pub struct Options {
    /// How many nodes. Three is the smallest that can hold a quorum.
    pub nodes: u32,
    /// How many zones the nodes are split into, evenly. One zone is the default shape:
    /// every node in one catalog, groups sliding over it. More than one splits the nodes
    /// into equal zones, each with its own catalog, its own extent and its own gateways,
    /// which is the shape a real deployment has.
    pub zones: u32,
    /// Whether each zone's catalog holds one group covering the whole zone rather than one
    /// group per node. A deployment small enough to place every page on every node of a
    /// zone is configured this way, and it is the only shape where `catalog.len() == 1`.
    pub one_group: bool,
    /// Cores per node. Each one is a worker, and each worker is a thread.
    pub cores: usize,
    /// Mutable 4 KiB blocks the cluster serves.
    pub mutable_blocks: u64,
    /// Immutable 4 KiB blocks the cluster serves.
    pub immutable_blocks: u64,
    /// Where the run's nondeterminism starts. The same seed and the same calls make the
    /// same run, every time and on any host.
    pub seed: u64,
}

impl Default for Options {
    fn default() -> Options {
        Options {
            nodes: 3,
            zones: 1,
            one_group: false,
            cores: 1,
            mutable_blocks: 256,
            immutable_blocks: 0,
            seed: 1,
        }
    }
}

/// A node the cluster has booted.
struct Node {
    id: u32,
    /// Held for the life of the node: dropping it stops the runtime.
    rt: runtime::Runtime<server::NodeGeneration>,
    /// Held because a reconfiguration builds its next generation from the same node.
    node: Arc<server::Node>,
}

/// A running cluster, and the kernel underneath it.
pub struct Cluster {
    kernel: Rc<sim::Sim>,
    /// The kernel that was installed on this thread, put back when the cluster goes.
    previous: Option<Kernel>,
    opts: Options,
    nodes: Vec<Node>,
}

impl Cluster {
    /// Boots `opts.nodes` nodes and returns once every one of them is serving.
    pub fn new(opts: Options) -> std::io::Result<Cluster> {
        assert!(opts.nodes >= 3, "a cluster smaller than a quorum");
        assert!(opts.cores >= 1, "a node with no cores");
        assert!(opts.zones >= 1, "a cluster with no zones");
        assert!(
            opts.nodes.is_multiple_of(opts.zones) && opts.nodes / opts.zones >= 3,
            "a zone smaller than a quorum"
        );

        let kernel = sim::Sim::new();
        kernel.set_cpus(opts.cores);
        kernel.set_seed(opts.seed);
        // Every node in the process shares one address space, so every table it builds
        // has to be small enough that hundreds of them fit. Installed before anything is
        // constructed, because a table keeps the shape it was built with.
        runtime::install_limits(&runtime::COMPACT);
        let previous = kernel::install(Kernel::Sim(kernel.clone()));

        let mut c = Cluster {
            kernel,
            previous: Some(previous),
            opts,
            nodes: Vec::new(),
        };

        for id in 1..=opts.nodes {
            c.boot(id)?;
        }

        Ok(c)
    }

    /// How many nodes are up.
    pub fn nodes(&self) -> usize {
        self.nodes.len()
    }

    /// Lets the cluster run for `us` virtual microseconds, or until it has nothing to do.
    pub fn run(&self, us: u64) {
        // A run that never reaches its deadline is not waiting on time, it is waiting on
        // itself, and a budget is what turns that from a hang into something a test can
        // report.
        const BUDGET: u64 = 1 << 20;
        let deadline = self.kernel.now_us() + us;
        let mut spent = 0u64;
        while self.kernel.now_us() < deadline {
            let turns = self.kernel.pump_until(deadline) as u64;
            if turns == 0 {
                // Nobody can move and nothing is owed before the deadline, so the
                // rest of the run is time the cluster spent waiting. Spending it
                // here rather than returning early is what makes a run always
                // reach the instant it was asked for.
                self.kernel.advance_to(deadline);
                break;
            }
            spent += turns;
            assert!(
                spent < BUDGET,
                "the cluster took {BUDGET} turns without spending {us} us: livelock"
            );
        }
    }

    /// Builds a configuration against one node, the way a reload does.
    ///
    /// The node is the one running while the build runs, so what the build declares is
    /// declared by it and against its driver. Answering `Ok(None)` rolls the whole thing
    /// back, which is what a test that only wants to see what a build refuses should do.
    pub fn update<F>(&self, id: u32, build: F) -> Result<bool, runtime::UpdateError>
    where
        F: FnOnce(
                &runtime::ResourceBuild,
                Option<&server::NodeGeneration>,
            ) -> std::io::Result<Option<server::NodeGeneration>>
            + Send
            + 'static,
    {
        let node = self
            .nodes
            .iter()
            .find(|n| n.id == id)
            .expect("no such node in this cluster");
        let was = self.kernel.enter_node(id);
        let applied = node.rt.update(build);
        self.kernel.enter_node(was);
        applied
    }

    /// Virtual microseconds since the run began.
    pub fn now_us(&self) -> u64 {
        self.kernel.now_us()
    }

    /// Hands a request to a node's exported device, the way a guest would.
    ///
    /// Nothing blocks, because nothing can: the cluster only moves inside [`Cluster::run`].
    /// A test submits, runs, and then asks [`Cluster::done`] what happened.
    pub fn submit(&self, node: u32, op: Op, lba: u64, data: Vec<u8>) -> std::io::Result<u64> {
        // The block layer picks a queue by the CPU it was submitted from; a page is as
        // good a stand-in, and it spreads the same way.
        let queues = self.opts.cores * runtime::QUEUES_PER_WORKER;
        let q_id = (lba as usize) % queues;
        self.kernel
            .ublk_submit(minor(node, 1), q_id, op.code(), lba, data)
    }

    /// Writes one page of `fill`.
    pub fn write(&self, node: u32, lba: u64, fill: u8) -> std::io::Result<u64> {
        self.submit(node, Op::Write, lba, vec![fill; PAGE])
    }

    /// Reads one page.
    pub fn read(&self, node: u32, lba: u64) -> std::io::Result<u64> {
        self.submit(node, Op::Read, lba, vec![0u8; PAGE])
    }

    /// Discards one page.
    pub fn trim(&self, node: u32, lba: u64) -> std::io::Result<u64> {
        self.submit(node, Op::Trim, lba, vec![0u8; PAGE])
    }

    /// Loses a node, the way a power cut does.
    ///
    /// The runtime is forgotten rather than dropped, because dropping one shuts it down
    /// and a crash is the opposite of that: nothing is told, nothing is drained, and
    /// what the node had not yet written it never will.
    pub fn crash(&mut self, id: u32) {
        let at = self
            .nodes
            .iter()
            .position(|n| n.id == id)
            .expect("no such node in this cluster");
        let n = self.nodes.remove(at);
        std::mem::forget(n.rt);
        std::mem::forget(n.node);
        self.kernel.crash_node(id, &[minor(id, 0), minor(id, 1)]);
    }

    /// Starts a crashed node again, from what its store still holds.
    ///
    /// The same call that booted it the first time, because a restart is a boot: what
    /// comes back is exactly what was durable.
    pub fn restart(&mut self, id: u32) -> std::io::Result<()> {
        self.boot(id)
    }

    /// Destroys a downed node's metadata index, leaving its superblock intact.
    ///
    /// Both copies of every metadata block, so that neither passes its checksum and the
    /// scan quarantines the lot: this is a store that came back without its index rather
    /// than one that came back blank, and the difference is the whole point. A blank store
    /// is reformatted at boot and then honestly holds nothing; a quarantined one cannot
    /// tell a page it never held from a page whose entry was in a block it lost, so it
    /// answers `MISSING` where a healthy node answers with a register at version zero.
    ///
    /// The node has to be down, because a running one would write its own index back over
    /// this.
    pub fn wipe_index(&self, id: u32) -> std::io::Result<()> {
        assert!(
            !self.nodes.iter().any(|n| n.id == id),
            "a running node would write its index back"
        );
        let was = self.kernel.enter_node(id);
        let r = wipe(&store_path(id));
        self.kernel.enter_node(was);
        r
    }

    /// What this run goes wrong with, from here on.
    ///
    /// A cluster starts on hardware that never fails, so a test says what it wants to
    /// survive rather than the other way round.
    pub fn faults(&self, f: Faults) {
        self.kernel.set_faults(f);
    }

    /// How many times the run took a path.
    ///
    /// A fault that was asked for and never happened is a test that passed for the wrong
    /// reason, so what a run went through is worth asserting on.
    pub fn hits(&self, h: Hit) -> u64 {
        self.kernel.hits(h)
    }

    /// What a request came to, once the node has answered it.
    ///
    /// Taken rather than read: an answer is collected once. The number is what the device
    /// reported, bytes on success and a negative errno on failure, and the bytes are what
    /// the guest's page holds now.
    pub fn done(&self, id: u64) -> Option<(i32, Vec<u8>)> {
        self.kernel.ublk_done(id)
    }

    /// Boots one node, exactly as `main` does: format, start, install the first generation.
    fn boot(&mut self, id: u32) -> std::io::Result<()> {
        let text = self.config_text(id);
        let cfg = Config::parse(&text)?;
        cfg.validate()?;

        // Everything this node reaches from here is its own: its counters, its metrics
        // tables, and the threads it starts.
        // Say where this node answers before it starts serving, so a peer that reaches
        // for it during its own boot finds it rather than a hole.
        self.kernel.set_fabric(id, minor(id, 0));
        let was = self.kernel.enter_node(id);
        let out = boot_node(cfg);
        self.kernel.enter_node(was);

        let (rt, node) = out?;
        self.nodes.push(Node { id, rt, node });
        Ok(())
    }

    /// How many nodes each zone holds.
    fn per_zone(&self) -> u32 {
        self.opts.nodes / self.opts.zones
    }

    /// The zone a node belongs to: the zones are equal runs of consecutive ids.
    fn zone_of(&self, id: u32) -> u32 {
        (id - 1) / self.per_zone() + 1
    }

    /// The ids of one zone, in order.
    fn zone_nodes(&self, zone: u32) -> Vec<u32> {
        let per = self.per_zone();
        ((zone - 1) * per + 1..=zone * per).collect()
    }

    /// The mutable extent a zone's blocks live in, and where its run starts.
    fn mutable_extent_of(&self, zone: u32) -> (u32, u64) {
        (10 + zone, (zone as u64 - 1) * self.opts.mutable_blocks)
    }

    /// The immutable extent a zone's blocks live in, and where its stripe-aligned run starts.
    fn immutable_extent_of(&self, zone: u32) -> (u32, u64) {
        let mutable_end = self.opts.zones as u64 * self.opts.mutable_blocks;
        let base = mutable_end.div_ceil(config::HUGE_BLOCKS) * config::HUGE_BLOCKS;
        let stride = self.opts.immutable_blocks.div_ceil(config::HUGE_BLOCKS) * config::HUGE_BLOCKS;
        (100 + zone, base + (zone as u64 - 1) * stride)
    }

    /// The configuration this node would have been handed.
    ///
    /// Device ids are ublk minors, and one driver serves the whole process, so they are
    /// spread by node rather than numbered from one on each.
    fn config_text(&self, id: u32) -> String {
        let o = &self.opts;
        let zone = self.zone_of(id);
        let mine = self.zone_nodes(zone);
        let size = layout::store_floor(o.mutable_blocks, o.immutable_blocks);
        let mut s = String::new();

        s.push_str("generation 1\n");
        s.push_str(&format!(
            "node id={id} zone={zone} cohort={} store={} size={size}\n",
            mine.iter().position(|&n| n == id).unwrap_or(0) % 3,
            store_path(id).display(),
        ));
        s.push_str(&format!(
            "universe 1 epoch=1 fabric_device_id={}\n",
            minor(id, 0)
        ));

        // Every node holds a link to every other, in either shape: a real cluster opens
        // the links its config names, and a zone's gateways need the ones out of it.
        for p in 1..=o.nodes {
            if p != id {
                s.push_str(&format!("  peer id={p} device=/sim/n{p}/fabric\n"));
            }
        }

        // A catalog names one zone's groups and no other's, so this is our zone's alone.
        // One group covering the whole zone, or sliding windows of three, so that every
        // node is in one and consecutive nodes share two.
        if o.one_group {
            let n = mine.len();
            s.push_str(&format!(
                "  group {} {} {}\n",
                mine[0],
                mine[1 % n],
                mine[2 % n]
            ));
        } else {
            for (i, &a) in mine.iter().enumerate() {
                let b = mine[(i + 1) % mine.len()];
                let c = mine[(i + 2) % mine.len()];
                s.push_str(&format!("  group {a} {b} {c}\n"));
            }
        }

        // Where the other zones answer from. Every node of a zone is a gateway of it,
        // which is what a small deployment looks like.
        for z in 1..=o.zones {
            if z != zone {
                let gws: Vec<String> = self.zone_nodes(z).iter().map(u32::to_string).collect();
                s.push_str(&format!("  zone id={z} gateways={}\n", gws.join(",")));
            }
        }

        // One extent of each enabled kind per zone, laid out end to end, so an address
        // names the zone that answers for it. Every node carries all of them: a node has
        // to resolve a foreign address to the zone holding it.
        for z in 1..=o.zones {
            if o.mutable_blocks > 0 {
                let (e, base) = self.mutable_extent_of(z);
                s.push_str(&format!(
                    "  extent id={e} base={base} blocks={} kind=mutable zone={z}\n",
                    o.mutable_blocks
                ));
            }
            if o.immutable_blocks > 0 {
                let (e, base) = self.immutable_extent_of(z);
                s.push_str(&format!(
                    "  extent id={e} base={base} blocks={} kind=immutable zone={z}\n",
                    o.immutable_blocks
                ));
            }
        }

        let mut ids = Vec::new();
        if o.mutable_blocks > 0 {
            ids.push(self.mutable_extent_of(zone).0);
        }
        if o.immutable_blocks > 0 {
            ids.push(self.immutable_extent_of(zone).0);
        }
        s.push_str(&format!(
            "device {} extents={}",
            minor(id, 1),
            ids.iter().map(u32::to_string).collect::<Vec<_>>().join(",")
        ));
        s.push('\n');
        s
    }
}

impl Drop for Cluster {
    fn drop(&mut self) {
        // Stop in the node's own scope, so a teardown reports where the boot did.
        for n in self.nodes.drain(..) {
            let was = self.kernel.enter_node(n.id);
            let _ = n.rt.shutdown();
            self.kernel.enter_node(was);
        }
        if let Some(previous) = self.previous.take() {
            kernel::install(previous);
        }
    }
}

pub use crate::kernel::sim::{Faults, Hit};

/// What a guest asks a device for.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Op {
    Read,
    Write,
    Trim,
}

impl Op {
    fn code(self) -> u8 {
        match self {
            Op::Read => sim::ublk::OP_READ,
            Op::Write => sim::ublk::OP_WRITE,
            Op::Trim => sim::ublk::OP_DISCARD,
        }
    }
}

/// The page a request is measured in.
pub const PAGE: usize = 4096;

/// A ublk minor belonging to `node`. Sixteen apiece is more than a node declares.
fn minor(node: u32, k: u32) -> u32 {
    node * 16 + k
}

/// Where a node keeps its store.
fn store_path(node: u32) -> std::path::PathBuf {
    std::path::PathBuf::from(format!("/sim/n{node}/store"))
}

/// Zero both copies of every metadata block a store's superblock accounts for. Written
/// through the same device calls the format does, so the store cannot tell this from the
/// media having rotted underneath it.
fn wipe(path: &std::path::Path) -> std::io::Result<()> {
    let g = layout::read_geometry(path)?;
    let f = layout::open_direct(path, true)?;
    let zero = {
        let mut b = layout::Aligned::new(layout::MBLOCK);
        b.as_mut().fill(0);
        b
    };
    for class in [layout::Class::Mutable, layout::Class::Immutable] {
        for id in 0..g.mblocks(class) {
            for copy in 0..2 {
                layout::write_at(&f, zero.as_ref(), g.mblock_off(class, id as u32, copy))?;
            }
        }
    }
    Ok(())
}

/// The boot sequence, which is `main`'s minus the parts a test has no use for: no signal
/// mask, no metrics listener, no configuration watch.
fn boot_node(
    cfg: Config,
) -> std::io::Result<(runtime::Runtime<server::NodeGeneration>, Arc<server::Node>)> {
    let store = cfg.node.store.clone();
    layout::format_if_needed(&store, &cfg)?;
    layout::grow_if_needed(&store, &cfg)?;

    let rt = runtime::start::<server::Server>()?;
    let node = Arc::new(server::Node::new());

    let boot = node.clone();
    rt.update(move |c, previous| {
        let d = boot
            .build_generation(c, previous, cfg)?
            .expect("the first generation is never stale");
        Ok(Some(d))
    })
    .map_err(runtime::UpdateError::into_inner)?;

    Ok((rt, node))
}

/// Names the seed a run was given, if the run does not come back.
///
/// A sweep that fails is only useful if it says which seed failed, and the seed is not in
/// the panic: it is in the loop that chose it.
pub struct Seeded(pub u64);

impl Drop for Seeded {
    fn drop(&mut self) {
        if std::thread::panicking() {
            eprintln!("racer: the failing run is seed {}", self.0);
        }
    }
}

/// A test that runs against a simulated cluster.
///
/// The body is handed a [`Cluster`] built from the options named above it, and any field
/// of [`Options`] may be named. A test that gives `seeds` instead runs its body once per
/// seed, and says which one failed, which is what makes a failure something to reproduce
/// rather than something to have witnessed.
///
/// ```ignore
/// sim_test! {
///     #[options(nodes = 5, seed = 7)]
///     fn a_page_is_read_back(c) { .. }
///
///     #[options(seeds = 1..32, nodes = 5)]
///     fn a_page_is_linearizable(c) { .. }
/// }
/// ```
#[macro_export]
macro_rules! sim_test {
    () => {};

    (
        #[options(seeds = $seeds:expr $(, $key:ident = $value:expr)* $(,)?)]
        fn $name:ident($cluster:ident) $body:block
        $($rest:tt)*
    ) => {
        #[test]
        fn $name() {
            for seed in $seeds {
                let _seeded = $crate::sim::Seeded(seed);
                #[allow(unused_mut)]
                let mut options = $crate::sim::Options { seed, ..Default::default() };
                $(options.$key = $value;)*
                #[allow(unused_mut)]
                #[allow(unused_mut)]
                let mut $cluster = $crate::sim::Cluster::new(options).expect("a cluster");
                $body
            }
        }
        $crate::sim_test! { $($rest)* }
    };

    (
        #[options($($key:ident = $value:expr),* $(,)?)]
        fn $name:ident($cluster:ident) $body:block
        $($rest:tt)*
    ) => {
        #[test]
        fn $name() {
            #[allow(unused_mut)]
            let mut options = $crate::sim::Options::default();
            $(options.$key = $value;)*
            let _seeded = $crate::sim::Seeded(options.seed);
            #[allow(unused_mut)]
            #[allow(unused_mut)]
                let mut $cluster = $crate::sim::Cluster::new(options).expect("a cluster");
            $body
        }
        $crate::sim_test! { $($rest)* }
    };
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_cluster_boots_every_node_it_was_asked_for() {
        let c = Cluster::new(Options::default()).unwrap();
        assert_eq!(c.nodes(), 3);
    }

    /// Runs until `id` has an answer, or gives up after `us` virtual microseconds.
    fn settle(c: &Cluster, id: u64, us: u64) -> (i32, Vec<u8>) {
        let deadline = c.now_us() + us;
        while c.now_us() < deadline {
            if let Some(answer) = c.done(id) {
                return answer;
            }
            c.run(1_000);
        }
        panic!("no answer in {us} us");
    }

    /// A mutable block requires the writer to have observed the version it replaces.
    fn observe(c: &Cluster, node: u32, page: u64, us: u64) {
        let r = c.read(node, page).expect("a mutable read");
        let (res, _) = settle(c, r, us);
        assert_eq!(res, PAGE as i32, "the observation was refused");
    }

    #[test]
    fn a_mutable_write_without_an_observation_conflicts() {
        let c = Cluster::new(Options::default()).expect("a cluster");
        let w = c.write(1, 5, 0x2a).expect("a write");
        let (res, _) = settle(&c, w, 5_000_000);
        assert_eq!(res, -libc::EAGAIN);
    }

    #[test]
    fn a_mutable_write_with_a_stale_observation_conflicts() {
        let c = Cluster::new(Options::default()).expect("a cluster");
        observe(&c, 1, 6, 5_000_000);
        observe(&c, 2, 6, 5_000_000);

        let first = c.write(1, 6, 0x2a).expect("a write");
        let (res, _) = settle(&c, first, 5_000_000);
        assert_eq!(res, PAGE as i32);

        let stale = c.write(2, 6, 0x3b).expect("a stale write");
        let (res, _) = settle(&c, stale, 5_000_000);
        assert_eq!(res, -libc::EAGAIN);
    }

    crate::sim_test! {
        #[options()]
        fn a_page_is_read_back_the_way_it_was_written(c) {
            observe(&c, 1, 7, 5_000_000);
            let w = c.write(1, 7, 0xab).expect("a write");
            let (res, _) = settle(&c, w, 5_000_000);
            assert_eq!(res, PAGE as i32, "a write was refused");
            let r = c.read(1, 7).expect("a read");
            let (res, page) = settle(&c, r, 5_000_000);
            assert_eq!(res, PAGE as i32, "a read was refused");
            assert!(page.iter().all(|b| *b == 0xab), "a page came back changed");
        }

        // Every seed draws a different set of lost frames, so what this asserts is not
        // one run but the shape of all of them: a write that came back acknowledged is
        // a write the cluster can still serve.
        #[options(seeds = 1..8)]
        fn a_page_survives_whatever_the_wire_does(c) {
            observe(&c, 1, 17, 20_000_000);
            c.faults(Faults { drop: 100, ..Faults::default() });
            let w = c.write(1, 17, 0x6d).expect("a write");
            let (res, _) = settle(&c, w, 20_000_000);
            assert_eq!(res, PAGE as i32, "a write was refused");
            let r = c.read(1, 17).expect("a read");
            let (res, page) = settle(&c, r, 20_000_000);
            assert_eq!(res, PAGE as i32, "a read was refused");
            assert!(page.iter().all(|b| *b == 0x6d), "a page came back changed");
        }
    }

    /// Immutable blocks share a 4 MiB placement stripe, and nothing else. Two blocks of
    /// one stripe hold their own bytes, are written independently, and the blocks between
    /// them are never allocated at all.
    #[test]
    fn immutable_blocks_in_one_stripe_are_independent() {
        let c = Cluster::new(Options {
            mutable_blocks: 0,
            immutable_blocks: 2048,
            ..Options::default()
        })
        .expect("a cluster");

        // The first and last block of the first stripe.
        for (lba, fill) in [(0u64, 0xa1u8), (1023, 0xb2)] {
            let w = c.write(1, lba, fill).expect("a write");
            let (res, _) = settle(&c, w, 5_000_000);
            assert_eq!(res, PAGE as i32, "block {lba} was refused");
        }

        for (lba, fill) in [(0u64, 0xa1u8), (1023, 0xb2)] {
            let r = c.read(1, lba).expect("a read");
            let (res, page) = settle(&c, r, 5_000_000);
            assert_eq!(res, PAGE as i32, "block {lba} was refused");
            assert!(
                page.iter().all(|b| *b == fill),
                "block {lba} came back as its sibling"
            );
        }

        // Between them the stripe was never written, so it is still a hole.
        let r = c.read(1, 512).expect("a read");
        let (res, page) = settle(&c, r, 5_000_000);
        assert_eq!(res, PAGE as i32);
        assert!(
            page.iter().all(|b| *b == 0),
            "an unwritten block held bytes"
        );

        // Write-once is per block: the sibling's write did not license a refill.
        let again = c.write(1, 0, 0xc3).expect("a write");
        let (res, _) = settle(&c, again, 5_000_000);
        assert_eq!(res, -libc::EAGAIN, "an immutable block was rewritten");
    }

    #[test]
    fn every_node_serves_the_page_one_of_them_was_given() {
        let c = Cluster::new(Options::default()).expect("a cluster");
        observe(&c, 1, 9, 5_000_000);
        let w = c.write(1, 9, 0x5c).expect("a write");
        let (res, _) = settle(&c, w, 5_000_000);
        assert_eq!(res, PAGE as i32, "a write was refused");
        // A write is acknowledged by a quorum, so every member answers for it, and the
        // ones that were not written to answer by asking whoever was.
        for node in 1..=c.nodes() as u32 {
            let r = c.read(node, 9).expect("a read");
            let (res, page) = settle(&c, r, 5_000_000);
            assert_eq!(res, PAGE as i32, "node {node} refused a read");
            assert!(
                page.iter().all(|b| *b == 0x5c),
                "node {node} served a different page"
            );
        }
    }

    #[test]
    fn a_store_that_refuses_everything_is_reported_not_swallowed() {
        let c = Cluster::new(Options::default()).expect("a cluster");
        observe(&c, 1, 13, 5_000_000);
        c.faults(Faults {
            io_error: 1000,
            ..Faults::default()
        });
        let w = c.write(1, 13, 0x11).expect("a write");
        let (res, _) = settle(&c, w, 5_000_000);
        assert!(res < 0, "a write onto dead media was acknowledged");
        assert!(c.hits(Hit::IoError) > 0, "no store ever refused");
    }

    #[test]
    fn a_cut_between_two_nodes_is_survived_by_the_rest() {
        let c = Cluster::new(Options::default()).expect("a cluster");
        observe(&c, 1, 15, 5_000_000);
        c.faults(Faults {
            cut: [(1, 2)].into_iter().collect(),
            ..Faults::default()
        });
        // Two of three can still hear each other, and two of three is a quorum.
        let w = c.write(1, 15, 0x33).expect("a write");
        let (res, _) = settle(&c, w, 5_000_000);
        assert_eq!(res, PAGE as i32, "a quorum could not commit");
        assert!(c.hits(Hit::Cut) > 0, "nothing was ever refused a peer");
        let r = c.read(1, 15).expect("a read");
        let (res, page) = settle(&c, r, 5_000_000);
        assert_eq!(res, PAGE as i32, "a quorum could not serve");
        assert!(page.iter().all(|b| *b == 0x33), "a page came back changed");
    }

    #[test]
    fn a_crash_does_not_lose_an_acknowledged_write() {
        let mut c = Cluster::new(Options::default()).expect("a cluster");

        observe(&c, 1, 21, 5_000_000);
        let w = c.write(1, 21, 0x7e).expect("a write");
        let (res, _) = settle(&c, w, 5_000_000);
        assert_eq!(res, PAGE as i32, "a write was not acknowledged");

        // Told nothing, drained of nothing. What comes back is what was durable.
        c.crash(1);
        c.restart(1).expect("a restart");

        let r = c.read(1, 21).expect("a read");
        let (res, page) = settle(&c, r, 5_000_000);
        assert_eq!(res, PAGE as i32, "a restarted node could not serve");
        assert!(page.iter().all(|b| *b == 0x7e), "a page came back changed");
    }

    #[test]
    fn the_rest_of_a_cluster_keeps_serving_while_one_node_is_gone() {
        let mut c = Cluster::new(Options::default()).expect("a cluster");
        observe(&c, 1, 23, 5_000_000);
        c.crash(3);

        // Two of three is a quorum, so the two that are left owe an answer.
        let w = c.write(1, 23, 0x11).expect("a write");
        let (res, _) = settle(&c, w, 5_000_000);
        assert_eq!(res, PAGE as i32, "a quorum could not commit");

        let r = c.read(2, 23).expect("a read");
        let (res, page) = settle(&c, r, 5_000_000);
        assert_eq!(res, PAGE as i32, "a quorum could not serve");
        assert!(page.iter().all(|b| *b == 0x11), "a page came back changed");
    }

    #[test]
    fn a_page_nobody_wrote_reads_as_a_hole() {
        let c = Cluster::new(Options::default()).unwrap();

        let r = c.read(1, 11).unwrap();
        let (res, page) = settle(&c, r, 60_000);
        assert_eq!(res, PAGE as i32);
        assert!(page.iter().all(|b| *b == 0), "a hole came back filled");
    }

    /// The same page, on the shape a deployment has, with a quorum of the group answering
    /// `MISSING` rather than with a register.
    ///
    /// A member whose index was lost holds nothing and says so, and that is an answer: it
    /// is not a vote for version zero, but it is not silence either, and asking again will
    /// not change it. The round therefore has a full view, `choose` can see that nothing
    /// was ever chosen, and the reader is entitled to zeroes. Counting only the members
    /// that returned a register made the round abandon a group every one of whose members
    /// had answered, and the `Io` that came of it reached the guest as `EIO`.
    #[test]
    fn a_page_nobody_wrote_reads_as_a_hole_when_the_group_lost_its_index() {
        let mut c = Cluster::new(Options {
            nodes: 6,
            zones: 2,
            one_group: true,
            ..Default::default()
        })
        .unwrap();

        // Two of zone one's three, so a quorum of the group holds no index at all and the
        // third is the only member left that can answer with a register.
        for id in [2, 3] {
            c.crash(id);
            c.wipe_index(id).expect("a wiped index");
            c.restart(id).expect("a restart");
        }

        let r = c.read(1, 11).unwrap();
        let (res, page) = settle(&c, r, 5_000_000);
        assert_eq!(res, PAGE as i32, "a hole was refused");
        assert!(page.iter().all(|b| *b == 0), "a hole came back filled");
    }

    /// The same page again, with one member of the trio simply gone.
    ///
    /// The two that are left answer with a register at version zero, which is a member
    /// saying it holds nothing, and two of three is the quorum: whatever the crashed member
    /// has it has alone, and a value one acceptor holds was never chosen. So the round can
    /// prove the page is free without it. Refusing whenever anybody was silent made that
    /// proof unreachable, and a page nobody had written could then be neither read nor
    /// written for as long as the third member stayed down: the prepare round answered
    /// `Conflict` every time and the guest saw `EAGAIN` forever.
    #[test]
    fn a_page_nobody_wrote_still_serves_while_one_member_is_gone() {
        let mut c = Cluster::new(Options {
            nodes: 6,
            zones: 2,
            one_group: true,
            ..Default::default()
        })
        .unwrap();
        c.crash(3);

        let r = c.read(1, 11).unwrap();
        let (res, page) = settle(&c, r, 5_000_000);
        assert_eq!(res, PAGE as i32, "a hole was refused");
        assert!(page.iter().all(|b| *b == 0), "a hole came back filled");

        // And the first write to it has a quorum to land on, which is what the same round
        // has to decide before an accept can pick a term.
        let w = c.write(1, 11, 0x4d).expect("a write");
        let (res, _) = settle(&c, w, 5_000_000);
        assert_eq!(res, PAGE as i32, "a first write was refused");

        let r = c.read(2, 11).expect("a read");
        let (res, page) = settle(&c, r, 5_000_000);
        assert_eq!(res, PAGE as i32, "a quorum could not serve");
        assert!(page.iter().all(|b| *b == 0x4d), "a page came back changed");
    }

    /// What a build refuses, and what it hands back twice.
    ///
    /// This runs against a node that is already serving, because that is the only place a
    /// build context exists: a resource is declared against the driver the node is using,
    /// and a build that answers with nothing is rolled back whole.
    #[test]
    fn a_build_refuses_what_it_cannot_export() {
        fn errno<T>(result: std::io::Result<T>) -> Option<i32> {
            match result {
                Ok(_) => panic!("declaration unexpectedly succeeded"),
                Err(error) => error.raw_os_error(),
            }
        }

        let c = Cluster::new(Options::default()).unwrap();
        let applied = c
            .update(1, |build, previous| {
                assert!(previous.is_some(), "the node is already serving");

                // A device is a whole number of pages of its class, and at least one.
                for size in [0, 4097] {
                    assert_eq!(errno(build.device(1, 100, size)), Some(libc::EINVAL));
                }

                // The same key declared the same way is the same export, not a second one.
                let first = build.device(1, 100, 4096).unwrap();
                let same = build.device(1, 100, 4096).unwrap();
                assert_eq!(first.path(), same.path());
                assert_eq!(first.path(), std::path::Path::new("/dev/ublkb100"));

                // Declared differently, it is a contradiction rather than a second export.
                assert_eq!(errno(build.device(1, 101, 4096)), Some(libc::EINVAL));
                assert_eq!(errno(build.fabric(1, 100, 4096)), Some(libc::EINVAL));

                // The slot table is finite, and running out of it says so.
                let mut exports = vec![first, same];
                let mut refused = None;
                for i in 1..64u32 {
                    match build.device(i as u64 + 1, 100 + i, 4096) {
                        Ok(e) => exports.push(e),
                        Err(error) => {
                            refused = error.raw_os_error();
                            break;
                        }
                    }
                }
                assert_eq!(refused, Some(libc::ENOSPC));

                Ok(None)
            })
            .unwrap();
        assert!(
            !applied,
            "a build that answers with nothing changes nothing"
        );
    }
}
