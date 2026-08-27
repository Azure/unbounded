use std::path::Path;
use std::sync::Arc;

use crate::config::Config;
use crate::layout::{self, Class, Geometry, MBLOCK};
use crate::runtime::{Disk, Limiter};

use super::shard::{self, Maps};
use super::{Allocator, GlobalAddr, Row, shape_of};

/// Open a formatted device and rebuild every shard from the metadata scan.
pub(super) fn open(
    path: &Path,
    disk: Disk,
    cfg: &Config,
    cores: usize,
) -> std::io::Result<(&'static Allocator, Vec<Row>)> {
    let geo = layout::read_geometry(path)?;
    let boot = layout::read_consensus(path)?;
    let limit = Arc::new(Limiter::new(
        cfg.node.max_iops(),
        cfg.node.max_bytes_per_sec(),
    ));
    let scans = scan(path, &geo, cores, &limit)?;

    let recoverable = cfg.peer_count() > 0;
    let (shards, quarantined) = {
        maps!(cfg, m);
        shard::rebuild(&shape_of(&geo, cfg, cores), cores, scans, &m)
    };
    let rows = shards
        .into_iter()
        .map(|mut shard| {
            shard.set_recoverable(recoverable);
            Row::new(shard)
        })
        .collect::<Vec<_>>();

    let alloc = Box::leak(Box::new(Allocator {
        disk,
        geo,
        cores,
        boot,
        quarantined,
    }));
    Ok((alloc, rows))
}

/// Read the whole metadata region and resolve each mblock's A/B copies.
fn scan(
    path: &Path,
    geo: &Geometry,
    threads: usize,
    limit: &Arc<Limiter>,
) -> std::io::Result<Vec<shard::Scanned>> {
    let mut out = Vec::new();
    for class in [Class::Mutable, Class::Immutable] {
        let n = geo.mblocks(class);
        if n == 0 {
            continue;
        }
        let per = n.div_ceil(threads as u64);
        // The split is made here, so the scan covers the same mblocks in the same pieces
        // whether the kernel runs them at once or one after another.
        let parts: Vec<std::io::Result<Vec<shard::Scanned>>> =
            crate::kernel::parallel(threads, |t| {
                let lo = (t as u64 * per).min(n);
                let hi = ((t as u64 + 1) * per).min(n);
                scan_range(path, geo, class, lo, hi, limit)
            });
        for p in parts {
            out.extend(p?);
        }
    }
    Ok(out)
}

fn scan_range(
    path: &Path,
    geo: &Geometry,
    class: Class,
    lo: u64,
    hi: u64,
    limit: &Arc<Limiter>,
) -> std::io::Result<Vec<shard::Scanned>> {
    let mut out = Vec::new();
    if lo >= hi {
        return Ok(out);
    }
    let f = layout::open_direct(path, false)?.meter(limit.clone());
    const BATCH: u64 = 1024;
    let mut a = layout::Aligned::new(BATCH as usize * MBLOCK);
    let mut b = layout::Aligned::new(BATCH as usize * MBLOCK);
    let mut at = lo;
    while at < hi {
        let n = BATCH.min(hi - at).min(geo.ext_end(class, at) - at);
        let len = n as usize * MBLOCK;
        layout::read_at(
            &f,
            &mut a.as_mut()[..len],
            geo.mblock_off(class, at as u32, 0),
        )?;
        layout::read_at(
            &f,
            &mut b.as_mut()[..len],
            geo.mblock_off(class, at as u32, 1),
        )?;
        for i in 0..n as usize {
            let id = (at + i as u64) as u32;
            let ba = &a.as_ref()[i * MBLOCK..(i + 1) * MBLOCK];
            let bb = &b.as_ref()[i * MBLOCK..(i + 1) * MBLOCK];
            let ha = layout::get_header(ba).filter(|h| h.mblock_id == id && h.class == class);
            let hb = layout::get_header(bb).filter(|h| h.mblock_id == id && h.class == class);
            match shard::pick_ab(ha, hb).map(|(h, b)| (h, if b { bb } else { ba })) {
                Some((h, raw)) => {
                    let k = class.k();
                    let mut entries = Vec::with_capacity(k as usize);
                    for j in 0..k {
                        entries.push(layout::get_entry(raw, class, j));
                    }
                    out.push(shard::Scanned {
                        id,
                        class,
                        generation: h.generation,
                        quarantined: false,
                        entries,
                    });
                }
                None => out.push(shard::Scanned {
                    id,
                    class,
                    generation: 0,
                    quarantined: true,
                    entries: Vec::new(),
                }),
            }
        }
        at += n;
    }
    Ok(out)
}
