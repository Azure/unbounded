use std::path::Path;

use racer::config::{self, Config};
use racer::layout;
use racer::metrics;
use racer::runtime;
use racer::server::{self, SERVER};

fn usage() -> ! {
    eprintln!("usage: racer serve <config>");
    std::process::exit(2)
}

fn main() -> std::io::Result<()> {
    let mut args = std::env::args().skip(1);
    let path = match (args.next().as_deref(), args.next()) {
        (Some("serve"), Some(p)) => p,
        _ => usage(),
    };
    let cfg = Config::load(Path::new(&path))?;
    cfg.validate()?;

    if args.next().is_some() {
        usage();
    }
    let metrics = std::env::var("METRICS_ADDR").unwrap_or_else(|_| ":9090".into());
    serve(cfg, path, metrics)
}

fn serve(cfg: Config, path: String, metrics: String) -> std::io::Result<()> {
    let dev = Path::new(&cfg.node.device);
    layout::format_if_needed(dev, &cfg)?;

    // A config that has outgrown the device gets the extra slots here, before the
    // allocator sizes its shards from the geometry. Fails the start rather than running
    // short: a device that cannot be grown is an operator's problem, not an ENOSPC an
    // hour later.
    layout::grow_if_needed(dev, &cfg)?;

    // Block the shutdown signals before any thread exists, so every thread inherits the
    // mask and `sigwait` below is the only place they are delivered.
    let mut set: libc::sigset_t = unsafe { std::mem::zeroed() };
    unsafe {
        libc::sigemptyset(&mut set);
        libc::sigaddset(&mut set, libc::SIGINT);
        libc::sigaddset(&mut set, libc::SIGTERM);
        libc::pthread_sigmask(libc::SIG_BLOCK, &set, std::ptr::null_mut());
    }

    // Bind before the runtime exists: an address we cannot have is a startup error, not
    // a thread that dies later. An early scrape waits in the backlog.
    let listener = metrics::listen(&metrics)?;
    println!("metrics -> {}", listener.local_addr()?);

    // Consensus and allocator for this node; sim.rs holds one per simulated node.
    let rt = runtime::start(&SERVER)?;
    let node = std::sync::Arc::new(server::Node::new());
    let first = cfg.clone();
    let boot = node.clone();
    rt.reload(move |c| {
        let d = boot.attach(c, first)?;
        if d.quarantined() > 0 {
            eprintln!("racer: {} metadata blocks quarantined", d.quarantined());
        }
        for (id, p) in d.devices() {
            println!("volume {} -> {}", id, p.display());
        }
        // The control plane publishes this one through nvmet, and the kernel picks the
        // ublk minor, so it has to be told which device it is.
        println!("fabric -> {}", d.fabric().display());
        Ok(d)
    })?;

    // Only now: the first configuration allocates the metric rows, since that is when
    // the worker count is settled.
    std::thread::Builder::new()
        .name("racer-metrics".into())
        .spawn(move || metrics::serve(listener))?;

    // The control plane replaces the file by rename; inotify reports it and every
    // accepted generation is applied whole. A rejected one leaves the running config
    // alone and only bumps `racer_config_rejected_total`.
    let watcher = rt.clone();
    std::thread::Builder::new().name("racer-cfg".into()).spawn(move || {
        let apply = move |c: Config| {
            let n = node.clone();
            watcher.reload(move |cfgr| n.attach(cfgr, c))?;
            println!("racer: configuration applied");
            Ok(())
        };
        if let Err(e) = config::watch(Path::new(&path), cfg, apply) {
            eprintln!("racer: config watch stopped: {e}");
        }
    })?;

    let mut sig = 0i32;
    unsafe { libc::sigwait(&set, &mut sig) };
    rt.shutdown()
}
