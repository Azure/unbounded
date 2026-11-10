//! Compact, rank-relative holder tables (`racer-write-holder-ranks`).
//!
//! An explicit [`super::Holders`] table spends a node ID per holder per page,
//! so the commit of a large object does not fit a signed peer head. Store
//! targets are ranked members of each page's placement, so a holder set is a
//! bit mask over the page's first `depth` ranked members: bit `i` set means
//! the member at rank `i` holds the page. The writer and the coordinator agree
//! on the ranking because a peer request is served only under the sender's
//! membership version.
//!
//! Encoding: `u8 depth`, then runs of `LEB128 count` (at least 1) and
//! `u8 mask` (below `1 << depth`, nonzero). Consecutive pages with the same
//! mask share a run, so an object whose pages all landed on their first
//! ranked members encodes in a few bytes whatever its size.

use super::holders::{HolderError, Holders, NodeIdBytes};

/// Largest supported rank depth (bits per mask).
pub const MAX_RANK_DEPTH: u8 = 8;

/// Per-page holder masks over each page's ranked members.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct HolderRanks {
    depth: u8,
    /// `(count, mask)` runs in page order; counts are nonzero.
    runs: Vec<(u64, u8)>,
}

impl HolderRanks {
    /// An empty table over `depth` ranks.
    pub fn new(depth: u8) -> Result<Self, HolderError> {
        if depth == 0 || depth > MAX_RANK_DEPTH {
            return Err(HolderError::Malformed);
        }
        Ok(Self {
            depth,
            runs: Vec::new(),
        })
    }

    /// Bits per mask.
    pub fn depth(&self) -> u8 {
        self.depth
    }

    /// Append the next page's mask.
    pub fn push(&mut self, mask: u8) -> Result<(), HolderError> {
        if mask == 0 || u32::from(mask) >= 1u32 << self.depth {
            return Err(HolderError::Malformed);
        }
        match self.runs.last_mut() {
            Some((count, last)) if *last == mask => *count += 1,
            _ => self.runs.push((1, mask)),
        }
        Ok(())
    }

    /// Number of pages described.
    pub fn pages(&self) -> u64 {
        self.runs.iter().map(|(count, _)| count).sum()
    }

    /// Every page's mask, in page order.
    pub fn masks(&self) -> impl Iterator<Item = u8> + '_ {
        self.runs
            .iter()
            .flat_map(|&(count, mask)| std::iter::repeat_n(mask, count as usize))
    }

    /// Build masks from an explicit table and each page's ranked members.
    /// `None` when a holder is not among the page's first `depth` ranks.
    pub fn from_holders<'a>(
        depth: u8,
        holders: &Holders,
        mut ranked: impl FnMut(u64) -> &'a [NodeIdBytes],
    ) -> Result<Option<Self>, HolderError> {
        let mut out = Self::new(depth)?;
        for (page, nodes) in holders.0.iter().enumerate() {
            let ranks = ranked(page as u64);
            let mut mask = 0u8;
            for node in nodes {
                let Some(rank) = ranks
                    .iter()
                    .take(usize::from(depth))
                    .position(|r| r == node)
                else {
                    return Ok(None);
                };
                mask |= 1 << rank;
            }
            out.push(mask)?;
        }
        Ok(Some(out))
    }

    /// Mask of one page against its ranked members, as holder node IDs.
    pub fn resolve_page(mask: u8, ranked: &[NodeIdBytes]) -> Result<Vec<NodeIdBytes>, HolderError> {
        let mut nodes = Vec::new();
        for bit in 0..8u8 {
            if mask & (1 << bit) == 0 {
                continue;
            }
            nodes.push(
                ranked
                    .get(usize::from(bit))
                    .cloned()
                    .ok_or(HolderError::Malformed)?,
            );
        }
        Ok(nodes)
    }

    /// Append the binary encoding.
    pub fn encode(&self, out: &mut Vec<u8>) {
        out.push(self.depth);
        for &(count, mask) in &self.runs {
            let mut value = count;
            loop {
                let byte = (value & 0x7f) as u8;
                value >>= 7;
                if value == 0 {
                    out.push(byte);
                    break;
                }
                out.push(byte | 0x80);
            }
            out.push(mask);
        }
    }

    /// Encode into a new buffer.
    pub fn to_bytes(&self) -> Vec<u8> {
        let mut out = Vec::new();
        self.encode(&mut out);
        out
    }

    /// Decode a complete table. Runs must be canonical (adjacent runs differ
    /// and counts are minimally encoded), so each table has one encoding.
    pub fn decode(bytes: &[u8]) -> Result<Self, HolderError> {
        let (&depth, mut rest) = bytes.split_first().ok_or(HolderError::Truncated)?;
        let mut out = Self::new(depth)?;
        while !rest.is_empty() {
            let mut count = 0u64;
            let mut shift = 0u32;
            loop {
                let (&byte, tail) = rest.split_first().ok_or(HolderError::Truncated)?;
                rest = tail;
                if shift >= 63 && byte > 1 {
                    return Err(HolderError::Malformed);
                }
                count |= u64::from(byte & 0x7f) << shift;
                if byte & 0x80 == 0 {
                    if byte == 0 && shift != 0 {
                        return Err(HolderError::Malformed);
                    }
                    break;
                }
                shift += 7;
            }
            let (&mask, tail) = rest.split_first().ok_or(HolderError::Truncated)?;
            rest = tail;
            if count == 0
                || mask == 0
                || u32::from(mask) >= 1u32 << depth
                || out.runs.last().is_some_and(|&(_, last)| last == mask)
            {
                return Err(HolderError::Malformed);
            }
            out.runs.push((count, mask));
        }
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ranked(page: u64) -> Vec<NodeIdBytes> {
        (0..5)
            .map(|r| format!("n{}", (page + r) % 7).into_bytes())
            .collect()
    }

    #[test]
    fn round_trip_and_runs() {
        let mut ranks = HolderRanks::new(5).unwrap();
        for mask in [3, 3, 3, 5, 3] {
            ranks.push(mask).unwrap();
        }
        assert_eq!(ranks.pages(), 5);
        let bytes = ranks.to_bytes();
        assert_eq!(bytes, vec![5, 3, 3, 1, 5, 1, 3]);
        assert_eq!(HolderRanks::decode(&bytes).unwrap(), ranks);
        assert_eq!(ranks.masks().collect::<Vec<_>>(), vec![3, 3, 3, 5, 3]);
    }

    #[test]
    fn max_object_with_default_placement_is_tiny() {
        let mut ranks = HolderRanks::new(5).unwrap();
        for _ in 0..crate::write::MAX_PAGES {
            ranks.push(0b011).unwrap();
        }
        let bytes = ranks.to_bytes();
        assert!(bytes.len() <= 5, "{}", bytes.len());
        let decoded = HolderRanks::decode(&bytes).unwrap();
        assert_eq!(decoded.pages(), crate::write::MAX_PAGES);
    }

    #[test]
    fn from_holders_and_resolve() {
        let holders = Holders(
            (0..4)
                .map(|p| {
                    let r = ranked(p);
                    vec![r[0].clone(), r[2].clone()]
                })
                .collect(),
        );
        let table: Vec<_> = (0..4).map(ranked).collect();
        let ranks = HolderRanks::from_holders(5, &holders, |p| &table[p as usize])
            .unwrap()
            .unwrap();
        assert_eq!(ranks.masks().collect::<Vec<_>>(), vec![0b101; 4]);
        for (page, mask) in ranks.masks().enumerate() {
            assert_eq!(
                HolderRanks::resolve_page(mask, &table[page]).unwrap(),
                holders.0[page]
            );
        }
        // A holder outside the ranking cannot be expressed.
        let outside = Holders(vec![vec![b"stranger".to_vec()]]);
        assert_eq!(
            HolderRanks::from_holders(5, &outside, |p| &table[p as usize]).unwrap(),
            None
        );
        // A mask naming a rank the ranking does not have is malformed.
        assert_eq!(
            HolderRanks::resolve_page(0b1_0000, &table[0][..2]),
            Err(HolderError::Malformed)
        );
    }

    #[test]
    fn rejects_malformed() {
        assert_eq!(HolderRanks::new(0), Err(HolderError::Malformed));
        assert_eq!(HolderRanks::new(9), Err(HolderError::Malformed));
        let mut ranks = HolderRanks::new(3).unwrap();
        assert_eq!(ranks.push(0), Err(HolderError::Malformed));
        assert_eq!(ranks.push(8), Err(HolderError::Malformed));
        assert_eq!(HolderRanks::decode(&[]), Err(HolderError::Truncated));
        assert_eq!(HolderRanks::decode(&[5, 1]), Err(HolderError::Truncated));
        assert_eq!(HolderRanks::decode(&[5, 0x81]), Err(HolderError::Truncated));
        // Zero count, zero mask, mask too wide, non-canonical runs and counts.
        assert_eq!(HolderRanks::decode(&[5, 0, 1]), Err(HolderError::Malformed));
        assert_eq!(HolderRanks::decode(&[5, 1, 0]), Err(HolderError::Malformed));
        assert_eq!(
            HolderRanks::decode(&[5, 1, 32]),
            Err(HolderError::Malformed)
        );
        assert_eq!(
            HolderRanks::decode(&[5, 1, 3, 1, 3]),
            Err(HolderError::Malformed)
        );
        assert_eq!(
            HolderRanks::decode(&[5, 0x81, 0, 3]),
            Err(HolderError::Malformed)
        );
        assert_eq!(HolderRanks::decode(&[5]).unwrap().pages(), 0);
    }
}
