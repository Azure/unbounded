//! A minimal test node built from racer-io primitives only.
//!
//! A small, fast Full-fidelity workload that exercises real racer-io code
//! without the whole application: two [`Lane`]s per node on `Driver::Sim`.
//! The world tests and the `lane-pair` scenarios use it.
//!
//! - `writer`: opens `/state` on the node's simulated disk, recovers its
//!   counter from `/state/counter` (so a crash and restart resumes from what
//!   was durably written), then per round sleeps a jittered interval (reactor
//!   timer, jitter from the worker's seeded entropy), atomically rewrites the
//!   counter, reads it back, and asks `checker` over an xthread mailbox to
//!   double it.
//! - `checker`: answers mailbox requests until the writer hangs up.
//!
//! Both lanes fold what they observe into the trace and report mismatches as
//! safety violations.
use std::{path::Path, rc::Rc};

use racer_io::{
    exec::Lane,
    ring::OpCtx,
    xthread::mailbox::{self, Receiver, Sender},
};

use crate::{
    engine::MILLIS,
    world::{DoneFlag, LaneRole, NodeEnv, Roles},
};

const TAG_WRITE: u64 = 100;
const TAG_REPLY: u64 = 101;
const TAG_SERVE: u64 = 102;
const TAG_RECOVER: u64 = 103;

/// Build the two lanes of one node, running `rounds` rounds.
pub fn lane_pair(env: &NodeEnv, rounds: u64) -> racer_io::Result<Roles> {
    let writer = env.reactor(1, 64)?;
    let checker = env.reactor(2, 64)?;
    let (tx, rx) = mailbox::channel::<u64, u64>(2)?;
    tx.set_notify(checker.wake_handle());
    let writer_done = DoneFlag::default();
    let checker_done = DoneFlag::default();

    let writer_lane = Lane::new(writer.clone(), writer_done.clone());
    writer_lane
        .executor
        .spawn_detached(write_loop(writer, tx, env.clone(), rounds, writer_done));
    let checker_lane = Lane::new(checker, checker_done.clone());
    checker_lane
        .executor
        .spawn_detached(serve(rx, env.clone(), checker_done));
    Ok(vec![
        ("writer".into(), Box::new(LaneRole(writer_lane))),
        ("checker".into(), Box::new(LaneRole(checker_lane))),
    ])
}

async fn write_loop(
    reactor: racer_io::Reactor,
    tx: Sender<u64, u64>,
    env: NodeEnv,
    rounds: u64,
    done: DoneFlag,
) {
    let ctx = OpCtx::none();
    let result: Result<(), String> = async {
        let dir = reactor
            .open_directory(Path::new("/state"), true, false, &ctx)
            .await
            .map_err(|e| format!("open_directory: {e:?}"))?;
        let mut n: u64 = match reactor.read_child(&dir, "counter", 64, false, &ctx).await {
            Ok(bytes) => std::str::from_utf8(&bytes)
                .ok()
                .and_then(|s| s.parse().ok())
                .ok_or("corrupt counter")?,
            Err(_) => 0,
        };
        env.trace.record(TAG_RECOVER, u64::from(env.index), n);
        for _ in 0..rounds {
            let jitter = reactor
                .ctx()
                .rng
                .next_u64()
                .map_err(|e| format!("rng: {e:?}"))?
                % (5 * MILLIS);
            let until = reactor.now() + std::time::Duration::from_nanos(MILLIS + jitter);
            reactor.sleep_until(until).await;
            n += 1;
            let text = n.to_string();
            reactor
                .atomic_write(&dir, "counter", text.as_bytes(), &ctx)
                .await
                .map_err(|e| format!("atomic_write: {e:?}"))?;
            let back = reactor
                .read_child(&dir, "counter", 64, false, &ctx)
                .await
                .map_err(|e| format!("read back: {e:?}"))?;
            if *back != *text.as_bytes() {
                env.report
                    .violation(format!("node {} read back {:?}", env.index, &*back));
            }
            env.trace.record_bytes(TAG_WRITE, &back);
            let credit = tx.credit().await.map_err(|e| format!("credit: {e:?}"))?;
            let reply = credit
                .call(n)
                .map_err(|(e, _)| format!("call: {e:?}"))?
                .await
                .map_err(|e| format!("reply: {e:?}"))?;
            if reply != n * 2 {
                env.report
                    .violation(format!("node {} checker said {reply} for {n}", env.index));
            }
            env.trace.record(TAG_REPLY, n, reply);
        }
        Ok(())
    }
    .await;
    if let Err(message) = result {
        env.report
            .violation(format!("node {} writer: {message}", env.index));
    }
    drop(tx);
    done.set();
}

async fn serve(rx: Receiver<u64, u64>, env: NodeEnv, done: DoneFlag) {
    let rx = Rc::new(rx);
    while let Some(envelope) = rx.recv().await {
        let (request, responder) = envelope.into_parts();
        env.trace.record(TAG_SERVE, u64::from(env.index), request);
        if responder.respond(request * 2).is_err() {
            env.report.violation("reply not delivered");
        }
    }
    done.set();
}
