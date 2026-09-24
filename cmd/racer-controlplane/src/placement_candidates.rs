// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

#[derive(Default)]
pub(super) struct Rankings {
    universe: String,
    ids: Vec<String>,
    width: usize,
    rows: Vec<(usize, u64)>,
}

impl PlacementCache {
    /// Slot-major top-k physical members, indexed by sorted identity. This cache
    /// is independent of roles and disposable. Missing ranked members rescan the
    /// affected row; additions only compare new scores. Cold work is O(S*N*k),
    /// with k <= 8 and no per-slot sort of the full membership.
    pub fn candidates(
        &mut self,
        slots: u32,
        universe: &str,
        ids: &[String],
        width: u32,
    ) -> Result<Vec<u32>> {
        let mut ids = ids.to_vec();
        ids.sort();
        if slots == 0
            || slots > SLOT_COUNT
            || universe.is_empty()
            || ids.len() > 100_000
            || width == 0
            || width > 8
            || width as usize > ids.len()
            || ids.windows(2).any(|w| w[0] == w[1])
        {
            return Err(Error("invalid physical candidate geometry".into()));
        }
        let universe_id = identity_bytes("universe", universe);
        let prefixes: Vec<_> = ids
            .iter()
            .map(|id| {
                if id.len() != 64
                    || !id
                        .bytes()
                        .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
                {
                    return Err(Error("invalid candidate node identity".into()));
                }
                let mut node = [0; 32];
                hex::decode_to_slice(id, &mut node).map_err(|e| Error(e.to_string()))?;
                Ok(score_prefix(&universe_id, &node))
            })
            .collect::<Result<_>>()?;
        let width = width as usize;
        let cache = &mut self.rankings;
        let compatible = cache.universe == universe
            && cache.width == width
            && cache.rows.len() == slots as usize * width;
        let remap: Vec<_> = cache
            .ids
            .iter()
            .map(|id| ids.binary_search(id).ok())
            .collect();
        let added: Vec<_> = ids
            .iter()
            .enumerate()
            .filter_map(|(i, id)| cache.ids.binary_search(id).is_err().then_some(i))
            .collect();
        let mut rows = Vec::with_capacity(slots as usize * width);
        for slot in 0..slots as usize {
            let mut best = [(usize::MAX, 0u64); 8];
            let retained = compatible
                && cache.rows[slot * width..(slot + 1) * width]
                    .iter()
                    .all(|&(i, _)| remap[i].is_some());
            if retained {
                for (target, &(i, value)) in best
                    .iter_mut()
                    .zip(&cache.rows[slot * width..(slot + 1) * width])
                {
                    *target = (remap[i].unwrap(), value);
                }
            }
            let mut consider = |i: usize| {
                let value = score(prefixes[i], slot as u32);
                let mut position = width;
                while position > 0
                    && (value > best[position - 1].1
                        || (value == best[position - 1].1 && i < best[position - 1].0))
                {
                    position -= 1;
                }
                if position < width {
                    for j in (position + 1..width).rev() {
                        best[j] = best[j - 1];
                    }
                    best[position] = (i, value);
                }
            };
            if retained {
                for &i in &added {
                    consider(i);
                }
            } else {
                for i in 0..ids.len() {
                    consider(i);
                }
            }
            rows.extend_from_slice(&best[..width]);
        }
        let result = rows.iter().map(|&(i, _)| i as u32).collect();
        *cache = Rankings {
            universe: universe.into(),
            ids,
            width,
            rows,
        };
        Ok(result)
    }
}
