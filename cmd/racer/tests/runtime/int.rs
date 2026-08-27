use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::task::{Context, Poll, Wake, Waker};
use std::time::Instant;

use super::*;
use crate::config::Class;

fn poll<F: Future>(future: Pin<&mut F>, waker: &Waker) -> Poll<F::Output> {
    future.poll(&mut Context::from_waker(waker))
}

#[derive(Default)]
struct WakeCount(AtomicUsize);

impl Wake for WakeCount {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
}

struct PendingDrop(Arc<AtomicUsize>);

impl Future for PendingDrop {
    type Output = Result<(), ()>;

    fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Self::Output> {
        Poll::Pending
    }
}

impl Drop for PendingDrop {
    fn drop(&mut self) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
}

#[test]
fn quorum_edges_abandon_unneeded_work() {
    let drops = Arc::new(AtomicUsize::new(0));
    let waker = Waker::noop();

    let mut zero = std::pin::pin!(quorum(
        [PendingDrop(drops.clone()), PendingDrop(drops.clone())],
        0,
    ));
    assert_eq!(poll(zero.as_mut(), waker), Poll::Ready([None, None]));
    assert_eq!(drops.load(Ordering::Relaxed), 2);

    let mut impossible = std::pin::pin!(quorum([PendingDrop(drops.clone())], 2));
    assert_eq!(poll(impossible.as_mut(), waker), Poll::Ready([None]));
    assert_eq!(drops.load(Ordering::Relaxed), 3);
    assert_eq!(Errno::from_raw(-libc::EIO), Errno::EIO);
    assert_eq!(Errno::from_raw(libc::EIO), Errno::EIO);
}

#[test]
fn pool_backpressure_and_kernel_holds() {
    let limits = limits();
    let pool = pool::Pool::new(limits).unwrap();
    let outer = pool::enter(&pool);

    for &(size, _) in limits.pool_classes {
        let mut buf = PoolBuf::try_alloc(size).unwrap();
        assert_eq!(buf.len(), size);
        buf[0] = 0x5a;
        buf.truncate(size / 2);
        assert_eq!((buf.len(), buf[0]), (size / 2, 0x5a));
    }

    let (size, count) = limits.pool_classes[0];
    let mut held: Vec<_> = (0..count)
        .map(|_| PoolBuf::try_alloc(size).unwrap())
        .collect();
    assert!(PoolBuf::try_alloc(size).is_none());

    let wakes = Arc::new(WakeCount::default());
    let waker = Waker::from(wakes.clone());
    let mut waiting = std::pin::pin!(PoolBuf::alloc(size));
    assert!(poll(waiting.as_mut(), &waker).is_pending());
    assert!(poll(waiting.as_mut(), &waker).is_pending());
    assert_eq!(pool.waiter_count(size), 1);

    let orphan = held.pop().unwrap();
    let index = orphan.index();
    pool.hold(index);
    pool.hold(index);
    drop(orphan);
    assert!(PoolBuf::try_alloc(size).is_none());
    pool.unhold(index);
    assert!(PoolBuf::try_alloc(size).is_none());
    pool.unhold(index);
    assert_eq!(wakes.0.load(Ordering::Relaxed), 1);

    let recycled = match poll(waiting.as_mut(), &waker) {
        Poll::Ready(buf) => buf,
        Poll::Pending => panic!("released buffer did not wake its waiter"),
    };
    assert_eq!(recycled.index(), index);
    drop(recycled);
    drop(held);
    pool::leave(outer);
}

#[test]
fn sys_and_ublk_encodings_match_the_kernel_abi() {
    use std::os::unix::ffi::OsStrExt;

    assert_eq!(
        crate::kernel::parse_cpu_list("0-2,7,9-10"),
        vec![0, 1, 2, 7, 9, 10]
    );
    assert_eq!(crate::kernel::parse_cpu_list("bad,4-2,8-x,11"), vec![11]);
    let nul = std::path::Path::new(std::ffi::OsStr::from_bytes(b"bad\0path"));
    let refused = crate::kernel::open(nul, libc::O_RDONLY, 0).err();
    assert_eq!(refused.and_then(|e| e.raw_os_error()), Some(libc::EINVAL));

    assert_eq!(size_of::<ublk::CtrlCmd>(), 32);
    assert_eq!(size_of::<ublk::DevInfo>(), 64);
    assert_eq!(size_of::<ublk::IoDesc>(), 24);
    assert_eq!(size_of::<ublk::IoCmd>(), 16);
    assert_eq!(size_of::<ublk::Params>(), 136);
    assert_eq!(
        ublk::buf_offset(3, 5, 7),
        0x8000_0000 + (3 << 41) + (5 << 25) + 7
    );
    assert_eq!(ublk::auto_buf_reg(0x1234, 0x56), 0x56_1234);
    assert_eq!(ublk::char_dev_path(8), "/dev/ublkc8");
    assert_eq!(ublk::block_dev_path(8), "/dev/ublkb8");

    let desc = ublk::IoDesc {
        op_flags: 0x1234_5600 | ublk::OP_WRITE as u32,
        ..Default::default()
    };
    assert_eq!(desc.op(), ublk::OP_WRITE);
    assert_eq!(desc.flags(), 0x1234_5600);

    let cmd = ublk::IoCmd {
        q_id: 0x1234,
        tag: 0x5678,
        result: -5,
        addr: 0x0102_0304_0506_0708,
    };
    let mut encoded = [0; 16];
    encoded[0..2].copy_from_slice(&cmd.q_id.to_ne_bytes());
    encoded[2..4].copy_from_slice(&cmd.tag.to_ne_bytes());
    encoded[4..8].copy_from_slice(&cmd.result.to_ne_bytes());
    encoded[8..16].copy_from_slice(&cmd.addr.to_ne_bytes());
    assert_eq!(cmd.encode(), encoded);

    for class in [Class::Small, Class::Huge, Class::Mixed] {
        let p = ublk::params_for(8 << 20, class);
        assert_eq!(p.basic.dev_sectors, 16_384);
        assert_eq!(p.basic.logical_bs_shift, 12);
        assert_eq!(p.dma.alignment, 4095);
        match class {
            Class::Small => assert_eq!((p.basic.max_sectors, p.basic.chunk_sectors), (8, 8)),
            Class::Huge => assert_eq!((p.basic.max_sectors, p.basic.chunk_sectors), (8192, 8192)),
            Class::Mixed => assert_eq!((p.basic.max_sectors, p.basic.chunk_sectors), (8192, 0)),
        }
    }
    let fabric = ublk::params_for_fabric(4 << 20);
    assert_eq!(
        (fabric.basic.max_sectors, fabric.basic.chunk_sectors),
        (8192, 8192)
    );
    assert!(fabric.discard == ublk::ParamDiscard::default());

    let limits = limits();
    for slot in [0, limits.max_devices - 1] {
        for queue in [0, QUEUES_PER_WORKER - 1] {
            for tag in [0, limits.queue_depth - 1] {
                let id = ublk::test_tag_id(slot, queue, tag);
                assert_eq!(ublk::test_tag_parts(id), (slot, queue, tag));
            }
        }
    }
    let op = worker::ud_op(0xf_ffff, u16::MAX);
    let link = worker::ud_link(17, 23);
    assert_eq!(worker::test_ud_parts(op), (0, u16::MAX, 0xf_ffff));
    assert_eq!(worker::test_ud_parts(link), (1, 23, 17));
    assert!(worker::test_park_timeout() <= std::time::Duration::from_millis(1));
}

const WORKFLOW_KEY: u64 = 0xfeed;
const PAGE: usize = 4096;

struct Gate {
    open: AtomicBool,
    waker: std::sync::Mutex<Option<Waker>>,
}

impl Gate {
    fn new() -> Gate {
        Gate {
            open: AtomicBool::new(false),
            waker: std::sync::Mutex::new(None),
        }
    }

    async fn wait(&self) {
        std::future::poll_fn(|cx| {
            if self.open.load(Ordering::Acquire) {
                Poll::Ready(())
            } else {
                *self.waker.lock().unwrap() = Some(cx.waker().clone());
                Poll::Pending
            }
        })
        .await
    }
}

#[derive(Default)]
struct Workflow {
    cores: AtomicUsize,
    ticks: AtomicUsize,
    requests: AtomicUsize,
    reads: AtomicUsize,
    writes: AtomicUsize,
    task: AtomicBool,
    task_started: AtomicBool,
    spawned: AtomicUsize,
    failed: AtomicBool,
}

struct WorkflowCfg {
    generation: u32,
    workflow: Arc<Workflow>,
    gate: Gate,
    states: Box<[std::sync::Mutex<Option<WorkflowCore>>]>,
}

struct WorkflowCore {
    index: usize,
    ticked: AtomicBool,
}

struct WorkflowHandler;

struct WorkflowWorker {
    cfg: Arc<WorkflowCfg>,
    core: WorkflowCore,
}

impl Handler for WorkflowHandler {
    type Config = WorkflowCfg;
    type Worker = WorkflowWorker;

    fn build_worker(
        id: CoreId,
        cfg: Arc<WorkflowCfg>,
        previous: Option<&WorkflowWorker>,
    ) -> WorkflowWorker {
        let core = match previous {
            Some(previous) => WorkflowCore {
                index: previous.core.index,
                ticked: AtomicBool::new(previous.core.ticked.load(Ordering::Acquire)),
            },
            None => cfg.states[id.index()].lock().unwrap().take().unwrap(),
        };
        WorkflowWorker { cfg, core }
    }

    async fn handle(worker: Rc<WorkflowWorker>, req: Request) -> Result<(), Errno> {
        let cfg = &worker.cfg;
        let workflow = cfg.workflow.clone();
        let valid_core = core().index() == worker.core.index;
        let generation = cfg.generation;
        if !valid_core || generation != 1 || req.lba != 0 || req.buf.len() != PAGE {
            workflow.failed.store(true, Ordering::Release);
            return Err(Errno::EIO);
        }

        if req.dev == 1 {
            cfg.gate.wait().await;
            return Ok(());
        }

        if req.dev == 2 {
            let here = core();
            if to::<Self, _, _>(here, |id, state| id.index() + state.core.index).await
                != here.index() * 2
                || to::<Self, _, _>(CoreId::of(1), |id, state| id.index() + state.core.index).await
                    != 2
                || to_async_with::<Self, _, _, _>(CoreId::of(1), |_| async { core().index() }).await
                    != 1
            {
                workflow.failed.store(true, Ordering::Release);
            }
            return Ok(());
        }

        if req.dev != WORKFLOW_KEY {
            workflow.failed.store(true, Ordering::Release);
            return Err(Errno::EIO);
        }
        match req.op {
            Op::Write => workflow.writes.fetch_add(1, Ordering::Relaxed),
            Op::Read => workflow.reads.fetch_add(1, Ordering::Relaxed),
            Op::Discard => return Err(Errno::EOPNOTSUPP),
        };
        workflow.requests.fetch_add(1, Ordering::Release);
        Ok(())
    }

    fn tick(worker: Rc<WorkflowWorker>, _: Instant) {
        let cfg = &worker.cfg;
        let workflow = cfg.workflow.clone();
        let id = core().index();
        let valid = id == worker.core.index;
        let first = !worker.core.ticked.swap(true, Ordering::AcqRel);
        if !valid {
            workflow.failed.store(true, Ordering::Release);
        }
        if first {
            workflow.ticks.fetch_add(1, Ordering::Release);
        }
        if id != 0 || workflow.task_started.swap(true, Ordering::AcqRel) {
            return;
        }

        let target = usize::from(cores() > 1);
        let spawned = workflow.clone();
        if !spawn_local(async move {
            spawned.spawned.fetch_add(1, Ordering::Release);
        }) {
            workflow.failed.store(true, Ordering::Release);
        }
        let task = workflow.clone();
        drop(to_async_with::<Self, _, _, _>(
            CoreId::of(target),
            move |worker| async move {
                if core().index() == target && worker.cfg.generation == 1 {
                    task.task.store(true, Ordering::Release);
                } else {
                    task.failed.store(true, Ordering::Release);
                }
            },
        ));
    }
}

fn new_workflow() -> Arc<Workflow> {
    Arc::new(Workflow::default())
}

fn workflow_config(workflow: Arc<Workflow>, cores: usize) -> WorkflowCfg {
    workflow.cores.store(cores, Ordering::Relaxed);
    WorkflowCfg {
        generation: 1,
        workflow,
        gate: Gate::new(),
        states: (0..cores)
            .map(|index| {
                std::sync::Mutex::new(Some(WorkflowCore {
                    index,
                    ticked: AtomicBool::new(false),
                }))
            })
            .collect(),
    }
}

fn workflow_request(dev: u64, op: Op) -> Request {
    Request {
        dev,
        op,
        lba: 0,
        buf: Buf {
            index: 0,
            addr: 0,
            len: PAGE as u32,
        },
    }
}

fn workflow_complete(workflow: &Workflow) -> bool {
    workflow.requests.load(Ordering::Acquire) == 2
        && workflow.ticks.load(Ordering::Acquire) == workflow.cores.load(Ordering::Relaxed)
        && workflow.spawned.load(Ordering::Acquire) == 1
        && workflow.task.load(Ordering::Acquire)
}

fn assert_workflow(workflow: &Workflow) {
    assert!(workflow_complete(workflow));
    assert_eq!(workflow.reads.load(Ordering::Relaxed), 1);
    assert_eq!(workflow.writes.load(Ordering::Relaxed), 1);
    assert!(!workflow.failed.load(Ordering::Acquire));
}

mod reload {
    use std::sync::Mutex;
    use std::time::Duration;

    use crate::kernel::{self, Kernel};

    use super::*;

    const KEY: u64 = 0xdead;
    const MINOR: u32 = 12;

    struct ReloadGate(AtomicBool);

    impl ReloadGate {
        async fn wait(&self) {
            while !self.0.load(Ordering::Acquire) {
                sleep(Duration::from_micros(1)).await;
            }
        }

        fn open(&self) {
            self.0.store(true, Ordering::Release);
        }
    }

    #[derive(Debug, PartialEq, Eq)]
    struct Build {
        generation: usize,
        serial: usize,
        previous: Option<(usize, usize)>,
    }

    #[derive(Debug, PartialEq, Eq)]
    struct Seen {
        lba: u64,
        before: (usize, usize),
        after: (usize, usize),
    }

    #[derive(Default)]
    struct Probe {
        next_serial: AtomicUsize,
        builds: Mutex<Vec<Build>>,
        seen: Mutex<Vec<Seen>>,
        entered: AtomicBool,
        workers_dropped: Mutex<Vec<usize>>,
        configs_dropped: Mutex<Vec<usize>>,
        premature_config_drop: AtomicBool,
    }

    struct Generation {
        generation: usize,
        probe: Arc<Probe>,
        gate: Arc<ReloadGate>,
        _export: Export,
    }

    impl Drop for Generation {
        fn drop(&mut self) {
            let worker_gone = self
                .probe
                .workers_dropped
                .lock()
                .unwrap()
                .contains(&self.generation);
            if !worker_gone {
                self.probe
                    .premature_config_drop
                    .store(true, Ordering::Release);
            }
            self.probe
                .configs_dropped
                .lock()
                .unwrap()
                .push(self.generation);
        }
    }

    struct Worker {
        cfg: Arc<Generation>,
        serial: usize,
    }

    impl Drop for Worker {
        fn drop(&mut self) {
            self.cfg
                .probe
                .workers_dropped
                .lock()
                .unwrap()
                .push(self.cfg.generation);
        }
    }

    struct ReloadHandler;

    impl Handler for ReloadHandler {
        type Config = Generation;
        type Worker = Worker;

        fn build_worker(_: CoreId, cfg: Arc<Generation>, previous: Option<&Worker>) -> Worker {
            let serial = cfg.probe.next_serial.fetch_add(1, Ordering::Relaxed);
            cfg.probe.builds.lock().unwrap().push(Build {
                generation: cfg.generation,
                serial,
                previous: previous.map(|w| (w.cfg.generation, w.serial)),
            });
            Worker { cfg, serial }
        }

        async fn handle(worker: Rc<Worker>, req: Request) -> Result<(), Errno> {
            let before = (worker.cfg.generation, worker.serial);
            if req.lba == 0 {
                worker.cfg.probe.entered.store(true, Ordering::Release);
                worker.cfg.gate.wait().await;
            }
            let after = (worker.cfg.generation, worker.serial);
            worker.cfg.probe.seen.lock().unwrap().push(Seen {
                lba: req.lba,
                before,
                after,
            });
            Ok(())
        }
    }

    struct Installed {
        previous_kernel: Option<Kernel>,
        previous_limits: &'static limits::Limits,
    }

    impl Drop for Installed {
        fn drop(&mut self) {
            kernel::install(self.previous_kernel.take().unwrap());
            install_limits(self.previous_limits);
        }
    }

    struct OpenGate(Arc<ReloadGate>);

    impl Drop for OpenGate {
        fn drop(&mut self) {
            self.0.open();
        }
    }

    fn generation(
        resources: &ResourceBuild,
        generation: usize,
        probe: Arc<Probe>,
        gate: Arc<ReloadGate>,
    ) -> std::io::Result<Generation> {
        Ok(Generation {
            generation,
            probe,
            gate,
            _export: resources.device(KEY, MINOR, PAGE as u64, Class::Small)?,
        })
    }

    fn pump_until(sim: &kernel::sim::Sim, mut done: impl FnMut() -> bool) {
        for _ in 0..10_000 {
            if done() {
                return;
            }
            sim.pump();
        }
        panic!("simulated runtime did not settle");
    }

    #[test]
    fn accepted_reload_overlaps_an_old_request() {
        let _only = exclusive();
        let sim = kernel::sim::Sim::new();
        sim.set_cpus(1);
        let gate = Arc::new(ReloadGate(AtomicBool::new(false)));
        let installed = Installed {
            previous_kernel: Some(kernel::install(Kernel::Sim(sim.clone()))),
            previous_limits: install_limits(&COMPACT),
        };

        let runtime = start::<ReloadHandler>().unwrap();
        let open_gate = OpenGate(gate.clone());
        let probe = Arc::new(Probe::default());
        let first_probe = probe.clone();
        let first_gate = gate.clone();
        assert!(
            runtime
                .update(move |resources, current| {
                    assert!(current.is_none());
                    generation(resources, 1, first_probe, first_gate).map(Some)
                })
                .unwrap()
        );

        let old = sim
            .ublk_submit(
                MINOR,
                0,
                crate::kernel::sim::ublk::OP_READ,
                0,
                vec![0; PAGE],
            )
            .unwrap();
        pump_until(&sim, || probe.entered.load(Ordering::Acquire));
        assert!(sim.ublk_done(old).is_none());

        let second_probe = probe.clone();
        let second_gate = gate.clone();
        assert!(
            runtime
                .update(move |resources, current| {
                    assert_eq!(current.unwrap().generation, 1);
                    generation(resources, 2, second_probe, second_gate).map(Some)
                })
                .unwrap()
        );
        assert!(sim.ublk_done(old).is_none());
        assert_eq!(
            *probe.builds.lock().unwrap(),
            [
                Build {
                    generation: 1,
                    serial: 0,
                    previous: None,
                },
                Build {
                    generation: 2,
                    serial: 1,
                    previous: Some((1, 0)),
                },
            ]
        );
        assert!(probe.workers_dropped.lock().unwrap().is_empty());
        assert!(probe.configs_dropped.lock().unwrap().is_empty());

        let new = sim
            .ublk_submit(
                MINOR,
                0,
                crate::kernel::sim::ublk::OP_READ,
                8,
                vec![0; PAGE],
            )
            .unwrap();
        pump_until(&sim, || sim.ublk_done(new).is_some());
        assert_eq!(
            *probe.seen.lock().unwrap(),
            [Seen {
                lba: 8,
                before: (2, 1),
                after: (2, 1),
            }]
        );

        assert!(!runtime.update(|_, _| Ok(None)).unwrap());
        assert_eq!(probe.builds.lock().unwrap().len(), 2);

        gate.open();
        pump_until(&sim, || sim.ublk_done(old).is_some());
        assert_eq!(
            *probe.seen.lock().unwrap(),
            [
                Seen {
                    lba: 8,
                    before: (2, 1),
                    after: (2, 1),
                },
                Seen {
                    lba: 0,
                    before: (1, 0),
                    after: (1, 0),
                },
            ]
        );
        pump_until(&sim, || probe.configs_dropped.lock().unwrap().contains(&1));
        assert!(probe.workers_dropped.lock().unwrap().contains(&1));
        assert!(!probe.premature_config_drop.load(Ordering::Acquire));

        runtime.shutdown().unwrap();
        drop(open_gate);
        drop(runtime);
        drop(installed);
    }
}

mod real {
    use std::time::Duration;

    use super::*;

    #[test]
    fn handler_workflow_runs_on_real_workers_and_requests() {
        let _only = exclusive();
        if crate::kernel::open(std::path::Path::new("/dev/ublk-control"), libc::O_RDWR, 0).is_err()
        {
            eprintln!("skipping: real Handler workflow needs /dev/ublk-control");
            return;
        }

        let runtime = start::<WorkflowHandler>().expect("start runtime");
        let second = match start::<WorkflowHandler>() {
            Ok(_) => panic!("a second runtime unexpectedly started"),
            Err(error) => error,
        };
        assert_eq!(second.raw_os_error(), Some(libc::EEXIST));

        let workflow = new_workflow();
        let observed = workflow.clone();
        assert!(
            runtime
                .update(move |resources, current| {
                    assert!(current.is_none());
                    let cores = resources.cores();
                    Ok(Some(workflow_config(workflow, cores)))
                })
                .expect("publish configuration")
        );
        runtime
            .request(0, workflow_request(WORKFLOW_KEY, Op::Write))
            .unwrap()
            .unwrap();
        runtime
            .request(0, workflow_request(WORKFLOW_KEY, Op::Read))
            .unwrap()
            .unwrap();

        let deadline = Instant::now() + Duration::from_secs(5);
        while !workflow_complete(&observed) && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(1));
        }
        assert_workflow(&observed);

        assert!(
            !runtime
                .update(|_, current| {
                    assert!(current.is_some());
                    Ok(None)
                })
                .unwrap()
        );
        runtime.shutdown().unwrap();
        runtime.shutdown().unwrap();
        assert_eq!(
            runtime
                .update(|_, _| Ok(None))
                .unwrap_err()
                .into_inner()
                .raw_os_error(),
            Some(libc::EINVAL)
        );
    }
}

/// The runtime encodes its control commands from the ioctl bit layout; the simulated
/// driver spells the same numbers out. They are the two ends of one wire, so a change to
/// either that the other did not make is a change that would have gone unnoticed against a
/// real driver and been caught only on hardware.
#[test]
fn the_simulated_driver_answers_the_commands_the_runtime_sends() {
    use crate::kernel::sim::ublk;

    assert_eq!(
        ublk::TEST_CTRL_CMDS,
        crate::runtime::ublk::TEST_CTRL_CMDS,
        "the runtime and the simulated driver disagree about a control command"
    );
    assert_eq!(
        ublk::FEATURES,
        crate::runtime::ublk::TEST_REQUIRED_FEATURES,
        "the simulated driver does not offer what the node refuses to run without"
    );
    assert_eq!(
        ublk::IO_DESC,
        size_of::<crate::runtime::ublk::IoDesc>(),
        "the runtime and the simulated driver disagree about a descriptor's size"
    );
    assert_eq!(
        ublk::CMD_BUF,
        crate::runtime::ublk::cmd_buf_size(),
        "the runtime and the simulated driver disagree about the stride between queues"
    );
    assert_eq!(
        ublk::TEST_IO_CMDS,
        crate::runtime::ublk::TEST_IO_CMDS,
        "the simulated driver answers different data-plane commands than the node sends"
    );
    assert_eq!(
        [ublk::OP_READ, ublk::OP_WRITE, ublk::OP_DISCARD],
        [
            crate::runtime::ublk::OP_READ,
            crate::runtime::ublk::OP_WRITE,
            crate::runtime::ublk::OP_DISCARD
        ],
        "the runtime and the simulated driver disagree about what a request asks for"
    );
}

/// A minor an instance of us left behind is taken back; one another program is still
/// serving is not. This is the only reclaim path the node has, and getting it wrong means
/// either refusing to restart or stealing someone else's export.
#[test]
fn a_minor_is_reclaimed_from_the_dead_and_left_to_the_living() {
    use crate::kernel::{self, Kernel};
    use crate::runtime::ublk;

    let s = kernel::sim::Sim::new();
    let previous = kernel::install(Kernel::Sim(s.clone()));

    let mut ctl = ublk::Control::open().unwrap();

    // Nobody has minor 4, so it is simply ours.
    let mut info = ublk::DevInfo::new(4, 2, 4, 0);
    ublk::add_dev(&mut ctl, &mut info, 0).unwrap();
    assert!(s.holds_minor(4));

    // Minor 5 was left by a process that is gone: reclaimed.
    s.preoccupy_minor(5, 0);
    let mut info = ublk::DevInfo::new(5, 2, 4, 0);
    ublk::add_dev(&mut ctl, &mut info, 1).unwrap();
    assert!(s.holds_minor(5));

    // Minor 6 is being served by something that is still there: left alone, and said so.
    s.preoccupy_minor(6, 12_345);
    let mut info = ublk::DevInfo::new(6, 2, 4, 0);
    let refused = ublk::add_dev(&mut ctl, &mut info, 2).unwrap_err();
    assert!(
        refused.to_string().contains("12345"),
        "expected the holder to be named, got: {refused}"
    );

    kernel::install(previous);
}
