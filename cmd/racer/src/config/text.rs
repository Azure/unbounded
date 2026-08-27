use std::io;
use std::path::PathBuf;

use super::{Config, bad, pb, store_path};

impl Config {
    /// A human-writable spelling of the same schema, for tests and reading by eye.
    /// Line-oriented; `peer`, `group`, `zone` and `extent` bind to the `universe` above.
    ///
    /// ```text
    /// generation 7
    /// node id=1 zone=1 cohort=0 store=/var/lib/racer/store.img size=68719476736
    /// universe 1 epoch=3 fabric_device_id=9
    ///   peer id=2 device=/dev/nvme1n1
    ///   group 1 2 3
    ///   zone id=2 gateways=4,5,6
    ///   extent id=10 base=0    blocks=4096 kind=mutable zone=1 cache_admit=2
    ///   extent id=11 base=4096 blocks=512  kind=mutable zone=1
    /// device 1 extents=10,11
    /// ```
    pub fn parse(text: &str) -> io::Result<Config> {
        let mut p = pb::NodeConfig::default();
        let mut store: Option<PathBuf> = None;
        for (n, line) in text.lines().enumerate() {
            let line = line.split('#').next().unwrap_or("").trim();
            if line.is_empty() {
                continue;
            }
            let parts: Vec<&str> = line.split_whitespace().collect();
            let (key, rest) = (parts[0], &parts[1..]);
            let f = fields(rest);
            let at = |e: io::Error| bad(format!("line {}: {e}", n + 1));
            match key {
                "generation" => p.generation = num(rest, 0).map_err(at)?,
                "node" => {
                    let f = only(
                        &f,
                        &[
                            "id", "zone", "cohort", "store", "size", "max_iops", "max_bps",
                        ],
                    )
                    .map_err(at)?;
                    // Absent is not empty: with no `store=` the path falls back to the env.
                    store = text_field(f, "store").ok().map(PathBuf::from);
                    p.node = Some(pb::Node {
                        id: get(f, "id").map_err(at)? as u32,
                        zone: get(f, "zone").map_err(at)? as u32,
                        cohort: Some(get_or(f, "cohort", 0).map_err(at)? as i32),
                        store: Some(pb::Store {
                            size_bytes: get(f, "size").map_err(at)?,
                            max_iops: unmetered(get_or(f, "max_iops", 0).map_err(at)?),
                            max_bytes_per_sec: unmetered(get_or(f, "max_bps", 0).map_err(at)?),
                        }),
                    });
                }
                "universe" => {
                    let f = only(&f, &["epoch", "fabric_device_id"]).map_err(at)?;
                    p.universes.push(pb::Universe {
                        id: num(rest, 0).map_err(at)? as u32,
                        epoch: get_or(f, "epoch", 0).map_err(at)? as u32,
                        fabric_device_id: get_or(f, "fabric_device_id", 0).map_err(at)? as u32,
                        ..Default::default()
                    });
                }
                "peer" => {
                    let f = only(&f, &["id", "device"]).map_err(at)?;
                    let peer = pb::Peer {
                        id: get(f, "id").map_err(at)? as u32,
                        device: text_field(f, "device").map_err(at)?,
                    };
                    last(&mut p, key).map_err(at)?.peers.push(peer);
                }
                "group" => {
                    let t = ids(rest).and_then(as_trio).map_err(at)?;
                    last(&mut p, key).map_err(at)?.catalog.push(t);
                }
                "zone" => {
                    let f = only(&f, &["id", "gateways"]).map_err(at)?;
                    let z = pb::Zone {
                        id: get(f, "id").map_err(at)? as u32,
                        gateways: list(f, "gateways").map_err(at)?,
                    };
                    last(&mut p, key).map_err(at)?.zones.push(z);
                }
                "extent" => {
                    let f = only(
                        &f,
                        &[
                            "id",
                            "base",
                            "blocks",
                            "kind",
                            "zone",
                            "next_zone",
                            "tombstone_epoch",
                            "cache_admit",
                            "warm_zones",
                        ],
                    )
                    .map_err(at)?;
                    let e = pb::Extent {
                        id: get(f, "id").map_err(at)? as u32,
                        base_lba: get(f, "base").map_err(at)?,
                        blocks: get(f, "blocks").map_err(at)?,
                        kind: named(f, "kind", "MUTABLE", pb::Kind::from_str_name).map_err(at)? as i32,
                        zone: get(f, "zone").map_err(at)? as u32,
                        next_zone: get_or(f, "next_zone", 0).map_err(at)? as u32,
                        tombstone_epoch: get_or(f, "tombstone_epoch", 0).map_err(at)? as u32,
                        cache_admit: get_or(f, "cache_admit", 0).map_err(at)? as u32,
                        warm_zones: list_or(f, "warm_zones").map_err(at)?,
                    };
                    last(&mut p, key).map_err(at)?.extents.push(e);
                }
                "device" => {
                    let f = only(&f, &["extents"]).map_err(at)?;
                    p.devices.push(pb::Device {
                        id: num(rest, 0).map_err(at)? as u32,
                        extents: list(f, "extents").map_err(at)?,
                    });
                }
                "policy" => {
                    let f = only(
                        &f,
                        &[
                            "max_index_bytes",
                            "occ_bytes",
                            "cache_index_bytes",
                            "repairs_per_replay",
                        ],
                    )
                    .map_err(at)?;
                    p.policy = Some(pb::Policy {
                        max_index_bytes: opt(f, "max_index_bytes").map_err(at)?,
                        occ_bytes: opt(f, "occ_bytes").map_err(at)?,
                        cache_index_bytes: opt(f, "cache_index_bytes").map_err(at)?,
                        repairs_per_replay: opt(f, "repairs_per_replay")
                            .map_err(at)?
                            .map(|v| v as u32),
                    });
                }
                other => return Err(bad(format!("line {}: unknown key {other}", n + 1))),
            }
        }
        Config::from_pb(p).map(|mut c| {
            c.node.store = store.unwrap_or_else(store_path);
            c
        })
    }
}

/// A rate of zero is what the model calls unmetered, and the wire says so by leaving the
/// field out, so the text spelling `max_iops=0` and omitting it agree.
fn unmetered(rate: u64) -> Option<u64> {
    (rate != 0).then_some(rate)
}

/// The universe the line being parsed belongs to: whichever `universe` line came last.
fn last<'a>(p: &'a mut pb::NodeConfig, key: &str) -> io::Result<&'a mut pb::Universe> {
    p.universes
        .last_mut()
        .ok_or_else(|| bad(format!("{key} before universe")))
}

fn num(rest: &[&str], i: usize) -> io::Result<u64> {
    rest.get(i)
        .ok_or_else(|| bad("missing value"))?
        .parse()
        .map_err(|_| bad("expected a number"))
}

fn fields<'a>(rest: &[&'a str]) -> Vec<(&'a str, &'a str)> {
    rest.iter().filter_map(|s| s.split_once('=')).collect()
}

/// Reject an unknown field rather than ignoring it: a silent default would mis-run the
/// node.
fn only<'a, 'b>(
    f: &'b [(&'a str, &'a str)],
    allowed: &[&str],
) -> io::Result<&'b [(&'a str, &'a str)]> {
    match f.iter().find(|(k, _)| !allowed.contains(k)) {
        Some((k, _)) => Err(bad(format!("unknown field {k}"))),
        None => Ok(f),
    }
}

fn ids(rest: &[&str]) -> io::Result<Vec<u32>> {
    rest.iter()
        .map(|s| s.parse::<u32>().map_err(|_| bad("expected a node id")))
        .collect()
}

/// A catalog group, the one place three is still the shape: position is the paxos member
/// index and the cohort column, so "not three" is not a state the model considers.
fn as_trio(v: Vec<u32>) -> io::Result<pb::Trio> {
    let a: [u32; 3] = v
        .as_slice()
        .try_into()
        .map_err(|_| bad(format!("expected 3 node ids, got {}", v.len())))?;
    Ok(a.into())
}

fn text_field(f: &[(&str, &str)], k: &str) -> io::Result<String> {
    f.iter()
        .find(|(a, _)| *a == k)
        .map(|(_, v)| v.to_string())
        .ok_or_else(|| bad(format!("missing field {k}")))
}

fn get(f: &[(&str, &str)], k: &str) -> io::Result<u64> {
    text_field(f, k)?
        .parse()
        .map_err(|_| bad(format!("field {k} is not a number")))
}

fn get_or(f: &[(&str, &str)], k: &str, d: u64) -> io::Result<u64> {
    Ok(opt(f, k)?.unwrap_or(d))
}

fn opt(f: &[(&str, &str)], k: &str) -> io::Result<Option<u64>> {
    match text_field(f, k) {
        Ok(_) => get(f, k).map(Some),
        Err(_) => Ok(None),
    }
}

/// An enum by name, case-insensitively; `from` is prost's lookup, so the two cannot drift.
fn named<T>(
    f: &[(&str, &str)],
    k: &str,
    default: &str,
    from: fn(&str) -> Option<T>,
) -> io::Result<T> {
    let v = text_field(f, k).unwrap_or_else(|_| default.to_string());
    from(&v.to_uppercase()).ok_or_else(|| bad(format!("field {k}: unknown value {v:?}")))
}

fn list(f: &[(&str, &str)], k: &str) -> io::Result<Vec<u32>> {
    text_field(f, k)?
        .split(',')
        .map(|s| {
            s.parse::<u32>()
                .map_err(|_| bad(format!("field {k} is not a node id list")))
        })
        .collect()
}

/// [`list`], but an absent field is an empty list rather than an error.
fn list_or(f: &[(&str, &str)], k: &str) -> io::Result<Vec<u32>> {
    match f.iter().any(|(a, _)| *a == k) {
        true => list(f, k),
        false => Ok(Vec::new()),
    }
}
