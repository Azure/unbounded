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
//!   counter, reads it back, and sends it to `checker` over an xthread SPSC
//!   queue, awaiting the doubled value on the reply queue.
//! - `checker`: answers requests until the writer closes the request queue.
//!
//! Both lanes fold what they observe into the trace and report mismatches as
//! safety violations.
use std::{future::poll_fn, path::Path, rc::Rc, time::Duration};

use racer_io::{
    scoped::fs::secure,
    xthread::spsc::{self, Receiver, Sender},
};

use crate::{
    engine::MILLIS,
    world::{DoneFlag, Lane, NodeEnv, Roles, SimReactor, SimScope},
};

const TAG_WRITE: u64 = 100;
const TAG_REPLY: u64 = 101;
const TAG_SERVE: u64 = 102;
const TAG_RECOVER: u64 = 103;

/// Build the two lanes of one node, running `rounds` rounds.
pub fn lane_pair(env: &NodeEnv, rounds: u64) -> racer_io::Result<Roles> {
    let writer = env.reactor(1, 64)?;
    let checker = env.reactor(2, 64)?;
    let (mut requests, received) = spsc::bounded::<u64>(1)?;
    let (replies, answered) = spsc::bounded::<u64>(1)?;
    requests.set_notify(
        checker
            .wake_handle()
            .expect("the checker reactor is initialized on construction"),
    );
    let writer_done = DoneFlag::default();
    let checker_done = DoneFlag::default();

    let writer_lane = Lane::new(writer.clone(), writer_done.clone());
    writer_lane.executor.spawn_detached(write_loop(
        writer,
        requests,
        answered,
        env.clone(),
        rounds,
        writer_done,
    ));
    let checker_lane = Lane::new(checker, checker_done.clone());
    checker_lane
        .executor
        .spawn_detached(serve(received, replies, env.clone(), checker_done));
    Ok(vec![
        ("writer".into(), Box::new(writer_lane)),
        ("checker".into(), Box::new(checker_lane)),
    ])
}

async fn write_loop(
    reactor: Rc<SimReactor>,
    mut requests: Sender<u64>,
    mut answered: Receiver<u64>,
    env: NodeEnv,
    rounds: u64,
    done: DoneFlag,
) {
    let scope = SimScope;
    let result: Result<(), String> = async {
        let dir = secure::directory(&reactor, Path::new("/state"), true, false, &scope)
            .await
            .map_err(|e| format!("open_directory: {e:?}"))?;
        let mut n: u64 = match secure::read_at(&reactor, &dir, "counter", 64, false, &scope).await {
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
            let until = reactor.ctx().now() + Duration::from_nanos(MILLIS + jitter);
            reactor
                .sleep_until_scheduled(until, &scope)
                .await
                .map_err(|e| format!("sleep: {e:?}"))?;
            n += 1;
            let text = n.to_string();
            secure::atomic_write(&reactor, &dir, "counter", text.as_bytes(), &scope)
                .await
                .map_err(|e| format!("atomic_write: {e:?}"))?;
            let back = secure::read_at(&reactor, &dir, "counter", 64, false, &scope)
                .await
                .map_err(|e| format!("read back: {e:?}"))?;
            if *back != *text.as_bytes() {
                env.report
                    .violation(format!("node {} read back {:?}", env.index, &*back));
            }
            env.trace.record_bytes(TAG_WRITE, &back);
            requests
                .ready()
                .await
                .map_err(|e| format!("send ready: {e:?}"))?;
            requests
                .try_send(n)
                .map_err(|failure| format!("send: {:?}", failure.error))?;
            let reply = poll_fn(|cx| answered.poll_receive(cx))
                .await
                .map_err(|e| format!("reply: {e:?}"))?
                .ok_or("checker closed the reply queue")?;
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
    drop(requests);
    done.set();
}

async fn serve(mut requests: Receiver<u64>, replies: Sender<u64>, env: NodeEnv, done: DoneFlag) {
    while let Ok(Some(request)) = poll_fn(|cx| requests.poll_receive(cx)).await {
        env.trace.record(TAG_SERVE, u64::from(env.index), request);
        if let Err(failure) = replies.try_send(request * 2) {
            env.report
                .violation(format!("reply not delivered: {:?}", failure.error));
            break;
        }
    }
    done.set();
}
