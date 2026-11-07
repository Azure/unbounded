//! Combined Model runs: the control plane ([`ControlSim`]) and the read
//! path ([`Model`]) on one timeline. Pod `i` is node `i`. Control events at or before a read-path
//! event run first; their recorded [`Changes`] then apply: committed memberships become
//! [`super::membership::Versions`] entries, each node adopts exactly the version its client
//! applied (none: it neither reads nor accepts peer requests), and Pod crashes, restarts and
//! removals crash or restart the node.

use serde_json::{Value, json};

use super::{
    Ev, Model,
    control::{Changes, ControlSim, Life},
};
use crate::{
    engine::{Nanos, SECONDS},
    world::Fault,
};

/// Run both timelines to `end` (or `max_steps` read-path events).
pub(super) fn run(model: &mut Model<'_>, ctl: &mut ControlSim, end: Nanos, max_steps: u64) {
    let mut pruned = 0;
    loop {
        let tn = model.engine.next_at().filter(|t| *t <= end);
        let tc = ctl.engine.next_at().filter(|t| *t <= end);
        match (tn, tc) {
            (None, None) => break,
            (tn, Some(c)) if tn.is_none_or(|n| c <= n) => {
                ctl.advance_to(c);
                sync(model, ctl, c, &mut pruned);
            }
            _ => {
                let Some(event) = model.engine.pop_until(end) else {
                    break;
                };
                if model.engine.processed() > max_steps {
                    break;
                }
                model.step(event);
            }
        }
    }
    ctl.advance_to(end);
    sync(model, ctl, end, &mut pruned);
}

/// Apply control changes to the read path at `at`.
fn sync(model: &mut Model<'_>, ctl: &mut ControlSim, at: Nanos, pruned: &mut Nanos) {
    let Changes {
        memberships,
        applied,
        lives,
    } = std::mem::take(&mut ctl.changes);
    {
        let mut versions = model.versions.borrow_mut();
        for (version, pods) in memberships {
            if versions.commit(version, pods, at) {
                model.report.counters.membership_publications += 1;
            }
        }
    }
    let changed = !applied.is_empty();
    for (pod, version) in applied {
        if (pod as usize) < model.nodes.len() {
            model.versions.borrow_mut().apply(pod, version);
        }
    }
    for (pod, life) in lives {
        if pod as usize >= model.nodes.len() {
            continue;
        }
        let fault = match life {
            Life::Crashed | Life::Removed => Fault::Crash { node: pod },
            Life::Restarted => Fault::Restart { node: pod },
        };
        model.engine.at(at, Ev::Fault(fault));
    }
    // Clients catching up release old versions; prune at most once per simulated second.
    if changed && at >= *pruned + SECONDS {
        *pruned = at;
        let nodes = &model.nodes;
        model
            .versions
            .borrow_mut()
            .prune(|n| nodes[n as usize].down);
    }
}

/// Fold the control report into the read-path `report`.
pub(super) fn report(report: &mut Value, model: &Model<'_>, ctl: &mut ControlSim, scenario: &str) {
    let ctl_report = ctl.report(scenario, model.choices.seed());
    let ctl_failures = ctl_report["invariants"]["hard_failures"]
        .as_u64()
        .unwrap_or(0);
    let node_failures = report["invariants"]["hard_failures"].as_u64().unwrap_or(0);
    report["logic"] = json!("combined");
    report["control_trace_hash"] = ctl_report["trace_hash"].clone();
    report["control_events"] = ctl_report["events"].clone();
    let mut components = serde_json::Map::new();
    let mut extra = 0u64;
    for (name, bytes) in ctl.memory() {
        components.insert(name.into(), json!(bytes));
        extra += bytes as u64;
    }
    let nodes = model.nodes.len().max(1) as u64;
    report["combined"] = json!({
        "hard_failures": node_failures + ctl_failures,
        "control_invariants": ctl_report["invariants"],
        "control_metrics": ctl_report["metrics"],
        "control": ctl_report["control"],
        "control_memory": {
            "components": components,
            "accounted_bytes": extra,
            "accounted_bytes_per_node": extra / nodes,
        },
        "membership_skew_at_end": skew(model, ctl),
        "versions_retained": model.versions.borrow().retained_versions(),
    });
    report["invariants"]["hard_failures"] = json!(node_failures + ctl_failures);
}

/// Up nodes by how many committed versions they trail at the end.
fn skew(model: &Model<'_>, ctl: &ControlSim) -> Value {
    let latest = ctl.committed_membership_version().unwrap_or(0);
    let (mut current, mut behind, mut unenrolled) = (0u64, 0u64, 0u64);
    for n in 0..model.nodes.len() as u32 {
        if model.nodes[n as usize].down {
            continue;
        }
        match ctl.membership_version(n) {
            0 => unenrolled += 1,
            v if v == latest => current += 1,
            _ => behind += 1,
        }
    }
    json!({"current": current, "behind": behind, "unenrolled": unenrolled, "latest": latest})
}
