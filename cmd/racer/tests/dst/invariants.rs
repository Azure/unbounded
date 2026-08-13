//! What has to be true, and when.
//!
//! Three separate contracts. Safety holds at every moment, faults or no
//! faults. Convergence and resource claims are owed only once the faults stop
//! and the cluster goes quiet. The envelope is a budget, not a correctness
//! claim, so it is measured over the whole run.

use crate::coverage::Reach;
use crate::model::Value;
use crate::world::{self, World};

/// Checked after every action, while the cluster is still being hurt.
///
/// The client facing half of safety lives in [`World::reap`] and
/// [`crate::model::Page::settle`], which between them refuse bytes no client
/// wrote, errors no client should see, and any history that no ordering of the
/// operations could explain. What is left is the state the client cannot see:
/// the allocator's slot accounting, its index, its census, and its assembly
/// table.
pub fn always(w: &World) -> Result<(), String> {
    w.sim.check_invariants()
}

/// Checked once the faults have stopped and the cluster has drained.
///
/// Every node has to serve every page, every node has to serve the same value,
/// and that value has to be one the history allows. The last of those is not
/// spelled out here: the reads go through the same model as the workload's, so
/// an illegal answer is caught by the checker with the whole history to hand.
pub fn converged(w: &mut World) -> Result<(), String> {
    let (small, huge) = w.pages();
    let nodes = w.sim.nodes();

    for page in 0..small {
        let mut agreed: Option<(usize, Value)> = None;

        for node in 0..nodes {
            let saw = w.read_now(node, page, false)?;

            match agreed {
                None => agreed = Some((node, saw)),
                Some((first, was)) if was != saw => {
                    return Err(format!(
                        "a healed cluster disagrees about small page {page}: node \
                         {first} says {was}, node {node} says {saw}"
                    ));
                }
                Some(_) => {}
            }
        }
    }

    for page in 0..huge {
        let mut agreed: Option<(usize, Value)> = None;

        for node in 0..nodes {
            let saw = w.read_now(node, page, true)?;

            match agreed {
                None => agreed = Some((node, saw)),
                Some((first, was)) if was != saw => {
                    return Err(format!(
                        "a healed cluster disagrees about huge page {page}: node \
                         {first} says {was}, node {node} says {saw}"
                    ));
                }
                Some(_) => {}
            }
        }
    }

    w.cov.reach(Reach::Converged);

    Ok(())
}

/// Checked once the cluster has drained: no client work may still be owed.
///
/// Huge page reservations are deliberately not part of this. An assembly whose
/// pieces stopped arriving keeps its reservation until something needs the
/// room, because there is no timer that could safely tell an abandoned
/// assembly from a slow one (`Allocator::open_parts`). What racer promises is
/// that the reservation cannot wedge the class, and [`reclaims`] is where that
/// is checked.
pub fn idle(w: &World) -> Result<(), String> {
    let s = w.sim.status();

    if !s.idle() {
        return Err(format!("a drained cluster is still busy: {s:?}"));
    }

    Ok(())
}

/// Checked once the cluster has drained: an abandoned assembly gives its room
/// back under pressure.
///
/// Every profile that fills huge pages leaves spare ones the workload never
/// touches. Filling them here has to work: if a reservation left behind by a
/// write that never finished could keep the class from taking new pages, this
/// is where the door stays shut.
pub fn reclaims(w: &mut World) -> Result<(), String> {
    let (_, huge) = w.pages();

    if huge == 0 {
        return Ok(());
    }

    if w.sim.assemblies() > 0 {
        w.cov.reach(Reach::Abandoned);
    }

    for page in huge..huge + world::SPARE {
        w.fill_now(page)?;
    }

    w.cov.reach(Reach::Reclaimed);

    Ok(())
}

/// Checked once the faults have stopped: damage done on purpose has to have
/// been undone, and undone durably.
///
/// The read that repairs a replica is the client's own, so this only has to
/// look. A crash and a restart afterwards says the repair reached the disk
/// rather than a page of memory.
pub fn repaired(w: &mut World) -> Result<(), String> {
    let damaged = w.damaged();

    if damaged.is_empty() {
        return Ok(());
    }

    for &(_, lba, replica) in &damaged {
        if !w.sim.small_replica_valid(lba, replica) {
            return Err(format!(
                "replica {replica} of page {lba} was damaged, has been read since, \
                 and still does not match its checksum"
            ));
        }
    }

    w.cov.reach(Reach::Repaired);

    // The same claim, but about the disk rather than about whatever the node
    // happened to be holding.
    for i in 0..w.sim.nodes() {
        w.sim.crash(i);
        w.sim
            .restart(i)
            .map_err(|e| format!("restart {i} while checking repairs: {e}"))?;
    }

    w.drain()?;
    w.sim.run(w.convergence());
    w.drain()?;

    for &(_, lba, replica) in &damaged {
        if !w.sim.small_replica_valid(lba, replica) {
            return Err(format!(
                "replica {replica} of page {lba} was repaired, but the repair did not \
                 survive a restart, so it was never written down"
            ));
        }
    }

    w.cov.reach(Reach::RepairDurable);

    Ok(())
}

/// Checked at the end of a run that asked for a rate limit.
///
/// The limiter is allowed to be approximate, so this is a ceiling with room in
/// it rather than an exact accounting. The other half matters just as much: a
/// limiter that lets nothing through would pass a ceiling test, so the run also
/// has to have pushed against it.
pub fn envelope(w: &mut World) -> Result<(), String> {
    let cap = w.profile.opts.device_iops;

    if cap == 0 {
        return Ok(());
    }

    let secs = w.sim.now().as_secs_f64();
    let mut pressed = false;

    for i in 0..w.sim.nodes() {
        let ops = w.sim.device_ops(i);

        if ops > cap {
            pressed = true;
        }

        let rate = ops as f64 / secs.max(1e-6);

        if rate > 1.2 * cap as f64 {
            return Err(format!(
                "node {i} ran its store at {rate:.0} operations a second over {secs:.1} \
                 seconds, which is past the {cap} it was given"
            ));
        }
    }

    if pressed {
        w.cov.reach(Reach::Throttled);
    }

    Ok(())
}

/// Checked at the end of a run in a cluster with more than one zone and a
/// cache that admits on first sight.
///
/// Warming and caching are advisory: they may never change what a client is
/// told, which [`converged`] has already established by reading every page from
/// every node. What they are for is locality, and that is the claim here. A
/// page a node has just read is a page that node holds, so reading it again
/// must not cross the fabric a second time.
pub fn locality(w: &mut World) -> Result<(), String> {
    let (_, huge) = w.pages();

    if w.profile.opts.zones < 2 || w.profile.opts.cache_admit != 1 || huge == 0 {
        return Ok(());
    }

    let per = w.sim.nodes() / w.profile.opts.zones as usize;

    for node in per..w.sim.nodes() {
        for page in 0..huge {
            if w.read_now(node, page, true)? == Value::Hole {
                continue;
            }

            let before = w.sim.crossings(node);

            w.read_now(node, page, true)?;

            let after = w.sim.crossings(node);

            if after != before {
                return Err(format!(
                    "node {node} read huge page {page}, then read it again and still \
                     crossed the fabric {} times for it, so nothing it fetched was \
                     kept",
                    after - before
                ));
            }
        }
    }

    Ok(())
}
