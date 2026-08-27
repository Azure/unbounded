use std::io;

use super::{
    Config, Extent, HUGE_BLOCKS, Kind, MAX_EXPORTS, MAX_EXTENTS, MAX_GATEWAYS, MAX_LBA,
    MAX_UNIVERSE, MAX_WARM_ZONES, SMALL_PAGE, Universe, bad,
};

impl Config {
    /// Every check that does not need the previous configuration.
    pub fn validate(&self) -> io::Result<()> {
        self.check_node()?;
        self.check_universes()?;
        self.check_devices()?;
        self.check_exports()?;
        if self.index.len() > MAX_EXTENTS {
            return Err(bad(format!(
                "{} extents, more than the {MAX_EXTENTS} this node can hold",
                self.index.len()
            )));
        }
        let blocks = self.mutable_blocks() + self.immutable_blocks();
        let index_bytes = blocks * crate::alloc::INDEX_BYTES_PER_BLOCK;
        if index_bytes > self.max_index_bytes() {
            return Err(bad(format!(
                "the block index would need {index_bytes} bytes, over the {} allowed",
                self.max_index_bytes()
            )));
        }
        if self.repairs_per_replay() == 0 {
            return Err(bad(
                "repairs_per_replay is zero, so a replay never finishes",
            ));
        }
        Ok(())
    }

    fn check_node(&self) -> io::Result<()> {
        if self.node.id == 0 {
            return Err(bad("node id is zero"));
        }
        if self.node.zone == 0 {
            return Err(bad("node zone is zero"));
        }
        if self.node.store.as_os_str().is_empty() {
            return Err(bad("node has no store path"));
        }
        if self.node.store_bytes() == 0 {
            return Err(bad("node store size is zero"));
        }
        if !self.node.store_bytes().is_multiple_of(SMALL_PAGE) {
            return Err(bad(format!(
                "node store size {} is not a multiple of {SMALL_PAGE}",
                self.node.store_bytes()
            )));
        }
        Ok(())
    }

    fn check_universes(&self) -> io::Result<()> {
        if self.universes.is_empty() {
            return Err(bad("node is in no universe"));
        }
        let mut paths: Vec<&str> = Vec::new();
        for (i, u) in self.universes.iter().enumerate() {
            if u.id == 0 {
                return Err(bad("universe id is zero"));
            }
            if u.id >= MAX_UNIVERSE {
                return Err(bad(format!(
                    "universe {} is at or above the {MAX_UNIVERSE} an address can name",
                    u.id
                )));
            }
            if i > 0 && self.universes[i - 1].id == u.id {
                return Err(bad(format!("universe {} appears twice", u.id)));
            }
            self.check_universe(u)?;
            for p in &u.peers {
                paths.push(&p.device);
            }
        }
        paths.sort_unstable();
        if let Some(w) = paths.windows(2).find(|w| w[0] == w[1]) {
            return Err(bad(format!(
                "namespace {} is named by two peers; a namespace belongs to one universe",
                w[0]
            )));
        }
        Ok(())
    }

    fn check_universe(&self, u: &Universe) -> io::Result<()> {
        let id = u.id;
        if u.catalog.is_empty() {
            return Err(bad(format!("universe {id} has an empty catalog")));
        }
        for (g, m) in u.catalog.iter().map(|g| g.nodes()).enumerate() {
            if m.contains(&0) {
                return Err(bad(format!("universe {id} group {g} names node 0")));
            }
            if m[0] == m[1] || m[0] == m[2] || m[1] == m[2] {
                return Err(bad(format!(
                    "universe {id} group {g} names {:?}, which is not three distinct nodes",
                    m
                )));
            }
        }
        // Not checked: whether the catalog spreads its groups evenly. It used to be, on the
        // grounds that a node could then size its store from the zone total and the node
        // count alone. But a zone that is growing, shrinking or levelling out is uneven by
        // construction: groups move one at a time so that every group keeps two of the three
        // nodes that held it, and every state between two even catalogs is an odd one. A
        // node sizes itself from the slots it actually holds instead, which is the same
        // number when the catalog happens to be even.
        for (i, z) in u.zones.iter().enumerate() {
            if z.id == 0 {
                return Err(bad(format!("universe {id} names zone 0")));
            }
            if z.id == self.node.zone {
                return Err(bad(format!(
                    "universe {id} lists our own zone {} among the others",
                    z.id
                )));
            }
            if u.zones[..i].iter().any(|o| o.id == z.id) {
                return Err(bad(format!("universe {id} names zone {} twice", z.id)));
            }
            // Not checked: whether we hold a link to any of them. A node may hear of a zone
            // before its namespaces are attached, and a routing-only node never holds one;
            // both fail at runtime with `EIO`.
            if z.gateways.is_empty() {
                return Err(bad(format!(
                    "universe {id} zone {} names no gateways, so nothing can reach it",
                    z.id
                )));
            }
            if z.gateways.len() > MAX_GATEWAYS {
                return Err(bad(format!(
                    "universe {id} zone {} names {} gateways, above the {MAX_GATEWAYS} allowed",
                    z.id,
                    z.gateways.len()
                )));
            }
            for (j, &g) in z.gateways.iter().enumerate() {
                if g == 0 {
                    return Err(bad(format!("universe {id} zone {} names gateway 0", z.id)));
                }
                if g == self.node.id {
                    return Err(bad(format!(
                        "universe {id} zone {} names this node as one of its gateways",
                        z.id
                    )));
                }
                if z.gateways[..j].contains(&g) {
                    return Err(bad(format!(
                        "universe {id} zone {} names gateway {g} twice",
                        z.id
                    )));
                }
            }
        }
        for (i, p) in u.peers.iter().enumerate() {
            if p.id == 0 {
                return Err(bad(format!("universe {id} has a peer with id 0")));
            }
            if p.id == self.node.id {
                return Err(bad(format!("universe {id} names this node as a peer")));
            }
            if p.device.is_empty() {
                return Err(bad(format!("universe {id} peer {} has no device", p.id)));
            }
            if u.peers[..i].iter().any(|o| o.id == p.id) {
                return Err(bad(format!("universe {id} names peer {} twice", p.id)));
            }
        }
        let mut end = 0u64;
        for e in &u.extents {
            if e.id == 0 {
                return Err(bad(format!("universe {id} has an extent with id 0")));
            }
            if e.blocks == 0 {
                return Err(bad(format!("extent {} is empty", e.id)));
            }
            if e.zone == 0 {
                return Err(bad(format!("extent {} is in zone 0", e.id)));
            }
            if e.next_zone == e.zone {
                return Err(bad(format!(
                    "extent {} is migrating to the zone it is already in",
                    e.id
                )));
            }
            if !u.known_zone(e.zone, self.node.zone) {
                return Err(bad(format!(
                    "extent {} is in zone {}, which universe {id} does not name",
                    e.id, e.zone
                )));
            }
            if e.next_zone != 0 && !u.known_zone(e.next_zone, self.node.zone) {
                return Err(bad(format!(
                    "extent {} is migrating to zone {}, which universe {id} does not name",
                    e.id, e.next_zone
                )));
            }
            // A warmed copy is read without a confirmation round, the round trip warming
            // exists to avoid. Only an immutable page can be believed on sight: its version
            // is a function of the tombstone epoch, so a copy is live or visibly not.
            if !e.warm_zones.is_empty() && e.guard() != Kind::Immutable {
                return Err(bad(format!(
                    "extent {} asks to warm other zones, which only an immutable extent may: \
                     a {:?} block carries no version a remote reader could trust on sight",
                    e.id,
                    e.guard()
                )));
            }
            if e.warm_zones.len() > MAX_WARM_ZONES {
                return Err(bad(format!(
                    "extent {} warms {} zones, above the {MAX_WARM_ZONES} allowed",
                    e.id,
                    e.warm_zones.len()
                )));
            }
            for (j, &w) in e.warm_zones.iter().enumerate() {
                if !u.known_zone(w, self.node.zone) {
                    return Err(bad(format!(
                        "extent {} warms zone {w}, which universe {id} does not name",
                        e.id
                    )));
                }
                // The home zone holds the blocks already, and a migration destination is
                // being sent every block and is about to become the home.
                if w == e.zone || w == e.next_zone {
                    return Err(bad(format!(
                        "extent {} warms zone {w}, which already holds its blocks",
                        e.id
                    )));
                }
                if e.warm_zones[..j].contains(&w) {
                    return Err(bad(format!("extent {} warms zone {w} twice", e.id)));
                }
            }
            // An immutable block is placed by the 4 MiB stripe it falls in, so an extent
            // that started mid-stripe would share a group with its neighbor and drag its
            // placement around whenever that neighbor was resized.
            if e.guard() == Kind::Immutable && e.base_lba % HUGE_BLOCKS != 0 {
                return Err(bad(format!(
                    "immutable extent {} starts at block {}, which is not a 4 MiB stripe \
                     boundary",
                    e.id, e.base_lba
                )));
            }
            if e.base_lba < end {
                return Err(bad(format!(
                    "extent {} starts at block {}, inside the extent before it",
                    e.id, e.base_lba
                )));
            }
            let Some(extent_end) = e.base_lba.checked_add(e.blocks()) else {
                return Err(bad(format!(
                    "extent {} runs past the end of universe {id}",
                    e.id
                )));
            };
            if extent_end > MAX_LBA {
                return Err(bad(format!(
                    "extent {} runs past the end of universe {id}",
                    e.id
                )));
            }
            end = extent_end;
        }
        Ok(())
    }

    fn check_devices(&self) -> io::Result<()> {
        for (i, d) in self.devices.iter().enumerate() {
            if d.id == 0 {
                return Err(bad("device id is zero"));
            }
            if i > 0 && self.devices[i - 1].id == d.id {
                return Err(bad(format!("device {} appears twice", d.id)));
            }
            if d.extents.is_empty() {
                return Err(bad(format!("device {} maps no extents", d.id)));
            }
            for (j, e) in d.extents.iter().enumerate() {
                if d.extents[..j].contains(e) {
                    return Err(bad(format!("device {} maps extent {e} twice", d.id)));
                }
            }
        }
        Ok(())
    }

    /// The minors this node asks the kernel for. Every universe exports its fabric
    /// namespace and every device exports itself, each as the ublk device named by the id
    /// given here, so the paths follow from the config alone. Two exports asking for one
    /// minor is caught now rather than half way through attaching.
    fn check_exports(&self) -> io::Result<()> {
        let mut ids: Vec<(u32, String)> =
            Vec::with_capacity(self.universes.len() + self.devices.len());
        for u in &self.universes {
            if u.fabric_device_id == 0 {
                return Err(bad(format!(
                    "universe {} names no device for its fabric namespace",
                    u.id
                )));
            }
            ids.push((u.fabric_device_id, format!("universe {} fabric", u.id)));
        }
        for d in &self.devices {
            ids.push((d.id, format!("device {}", d.id)));
        }
        ids.sort();
        if let Some(w) = ids.windows(2).find(|w| w[0].0 == w[1].0) {
            return Err(bad(format!(
                "{} and {} both ask to be device {}",
                w[0].1, w[1].1, w[0].0
            )));
        }
        if ids.len() > MAX_EXPORTS {
            return Err(bad(format!(
                "{} universes plus {} devices is more than the {MAX_EXPORTS} this node can export",
                self.universes.len(),
                self.devices.len()
            )));
        }
        Ok(())
    }

    /// The checks that need the configuration this one replaces. Everything frozen here
    /// is frozen because the dataplane has already built something around it.
    pub(crate) fn validate_against(&self, prev: &Config) -> io::Result<()> {
        if self.generation <= prev.generation {
            return Err(bad(format!(
                "generation {} does not advance on {}",
                self.generation, prev.generation
            )));
        }
        if self.node.store_bytes() < prev.node.store_bytes() {
            return Err(bad(format!(
                "store size {} is below the {} already formatted",
                self.node.store_bytes(),
                prev.node.store_bytes()
            )));
        }
        for pu in &prev.universes {
            let Some(u) = self.universe(pu.id) else {
                continue;
            };
            if u.catalog.len() != pu.catalog.len() {
                return Err(bad(format!(
                    "universe {} changes from {} groups to {}",
                    u.id,
                    pu.catalog.len(),
                    u.catalog.len()
                )));
            }
            // The fabric namespace is already exported at this minor and peers hold the
            // path, so moving it would strand every link into this universe.
            if u.fabric_device_id != pu.fabric_device_id {
                return Err(bad(format!(
                    "universe {} moves its fabric from device {} to {}",
                    u.id, pu.fabric_device_id, u.fabric_device_id
                )));
            }
            // What makes a membership change safe is per group, not per catalog: a group
            // that kept two of its three nodes has two replicas holding every version it
            // ever agreed, so it serves reads while the third replays from them. A group
            // that changed two runs on one copy, and one that changed all three has no copy
            // at all and, worse, looks converged, because three empty replicas agree with
            // each other. The id counts are the second rule: they bound how much of the zone
            // replays at once and make a departure something the control plane can wait on.
            //
            // Only across one generation, because a node that missed a push cannot tell how
            // many steps a catalog took and two legal steps compose into a group that
            // changed twice. The control plane holds every transition to this rule whatever
            // the stride, so a gap here means a push was missed, not that the rule was.
            if self.generation == prev.generation + 1 {
                for (g, (was, now)) in pu
                    .catalog
                    .iter()
                    .map(|g| g.nodes())
                    .zip(u.catalog.iter().map(|g| g.nodes()))
                    .enumerate()
                {
                    let kept = (0..3).filter(|&c| was[c] != 0 && was[c] == now[c]).count();
                    if kept < 2 {
                        return Err(bad(format!(
                            "universe {} group {g} keeps only {kept} of {was:?}, becoming {now:?}",
                            u.id
                        )));
                    }
                }
                let (was, now) = (pu.zone_nodes(), u.zone_nodes());
                let joined = now.iter().filter(|n| !was.contains(n)).count();
                let left = was.iter().filter(|n| !now.contains(n)).count();
                if joined > 1 || left > 1 {
                    return Err(bad(format!(
                        "universe {} moves {joined} nodes in and {left} out at once",
                        u.id
                    )));
                }
            }
        }
        for (pu, pe) in prev.extents() {
            let Some((u, e)) = self.extent_by_id(pe.id) else {
                continue;
            };
            if u.id != pu.id {
                return Err(bad(format!(
                    "extent {} moves from universe {} to {}",
                    pe.id, pu.id, u.id
                )));
            }
            self.check_replacement(pe, e)?;
        }
        for pd in &prev.devices {
            let Some(d) = self.devices.iter().find(|d| d.id == pd.id) else {
                continue;
            };
            if d.extents != pd.extents {
                return Err(bad(format!(
                    "device {} changes from extents {:?} to {:?}",
                    d.id, pd.extents, d.extents
                )));
            }
        }
        Ok(())
    }

    /// What one extent may change into. Shape is frozen because the allocator placed
    /// blocks by it; placement may move only along the migration the previous config
    /// declared.
    fn check_replacement(&self, old: &Extent, new: &Extent) -> io::Result<()> {
        if new.base_lba != old.base_lba {
            return Err(bad(format!(
                "extent {} moves from block {} to {}",
                old.id, old.base_lba, new.base_lba
            )));
        }
        if new.blocks != old.blocks {
            return Err(bad(format!(
                "extent {} changes from {} blocks to {}",
                old.id, old.blocks, new.blocks
            )));
        }
        if new.kind != old.kind {
            return Err(bad(format!(
                "extent {} changes from {:?} to {:?}",
                old.id,
                old.wire_kind(),
                new.wire_kind()
            )));
        }
        if new.tombstone_epoch < old.tombstone_epoch {
            return Err(bad(format!(
                "extent {} rewinds its tombstone epoch from {} to {}",
                old.id, old.tombstone_epoch, new.tombstone_epoch
            )));
        }
        if new.zone != old.zone && new.zone != old.next_zone {
            return Err(bad(format!(
                "extent {} moves from zone {} to {} without having been migrating there",
                old.id, old.zone, new.zone
            )));
        }
        Ok(())
    }
}
