//! Per-page holder tables and their binary encoding (`racer-write-holders`).
//!
//! Encoding: for each page in order, `u8 count`, then `count` entries of
//! `u16 BE length` + node ID bytes. The page count is implied by the input
//! length; decoders that know it check it with [`Holders::validate`].

use std::collections::BTreeSet;

/// A node ID as raw bytes (the UTF-8 of `control::NodeId`).
pub type NodeIdBytes = Vec<u8>;

/// Nodes holding a durable copy, indexed by page number.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Holders(pub Vec<Vec<NodeIdBytes>>);

/// Why a holder table is malformed or insufficient.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum HolderError {
    /// Input ended inside an entry.
    Truncated,
    /// Empty node ID, more than 255 holders for a page, or a node ID over `u16::MAX` bytes.
    Malformed,
    /// Page count differs from the object.
    PageCount,
    /// A page has fewer holders than required.
    TooFew { page: u64 },
    /// A page lists the same node twice.
    Duplicate { page: u64 },
}

impl Holders {
    /// Number of pages in the table.
    pub fn pages(&self) -> u64 {
        self.0.len() as u64
    }

    /// The holders of one page.
    pub fn page(&self, page: u64) -> &[NodeIdBytes] {
        usize::try_from(page)
            .ok()
            .and_then(|p| self.0.get(p))
            .map_or(&[], Vec::as_slice)
    }

    /// Every distinct node in the table, in first-seen order.
    pub fn nodes(&self) -> Vec<NodeIdBytes> {
        let mut seen: BTreeSet<&[u8]> = BTreeSet::new();
        let mut out: Vec<NodeIdBytes> = Vec::new();
        for node in self.0.iter().flatten() {
            if seen.insert(node.as_slice()) {
                out.push(node.clone());
            }
        }
        out
    }

    /// Require exactly `pages` pages, at least `replicas` holders each, and distinct nodes per page.
    pub fn validate(&self, pages: u64, replicas: u8) -> Result<(), HolderError> {
        if self.pages() != pages {
            return Err(HolderError::PageCount);
        }
        for (page, nodes) in self.0.iter().enumerate() {
            let page = page as u64;
            if nodes.iter().any(Vec::is_empty) {
                return Err(HolderError::Malformed);
            }
            if nodes.len() < usize::from(replicas) {
                return Err(HolderError::TooFew { page });
            }
            for (i, node) in nodes.iter().enumerate() {
                if nodes[..i].contains(node) {
                    return Err(HolderError::Duplicate { page });
                }
            }
        }
        Ok(())
    }

    /// Append the binary encoding.
    pub fn encode(&self, out: &mut Vec<u8>) -> Result<(), HolderError> {
        for nodes in &self.0 {
            let count = u8::try_from(nodes.len()).map_err(|_| HolderError::Malformed)?;
            out.push(count);
            for node in nodes {
                let len = u16::try_from(node.len()).map_err(|_| HolderError::Malformed)?;
                if len == 0 {
                    return Err(HolderError::Malformed);
                }
                out.extend_from_slice(&len.to_be_bytes());
                out.extend_from_slice(node);
            }
        }
        Ok(())
    }

    /// Encode into a new buffer.
    pub fn to_bytes(&self) -> Result<Vec<u8>, HolderError> {
        let mut out = Vec::new();
        self.encode(&mut out)?;
        Ok(out)
    }

    /// Decode a complete table; the whole input must be consumed.
    pub fn decode(mut bytes: &[u8]) -> Result<Self, HolderError> {
        let mut pages = Vec::new();
        while let Some((&count, rest)) = bytes.split_first() {
            bytes = rest;
            let mut nodes = Vec::with_capacity(usize::from(count));
            for _ in 0..count {
                if bytes.len() < 2 {
                    return Err(HolderError::Truncated);
                }
                let len = usize::from(u16::from_be_bytes([bytes[0], bytes[1]]));
                bytes = &bytes[2..];
                if len == 0 {
                    return Err(HolderError::Malformed);
                }
                if bytes.len() < len {
                    return Err(HolderError::Truncated);
                }
                nodes.push(bytes[..len].to_vec());
                bytes = &bytes[len..];
            }
            pages.push(nodes);
        }
        Ok(Self(pages))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn table() -> Holders {
        Holders(vec![
            vec![b"node-a".to_vec(), b"node-b".to_vec()],
            vec![b"node-b".to_vec(), b"c".to_vec(), b"node-a".to_vec()],
        ])
    }

    #[test]
    fn round_trip_and_golden() {
        let bytes = table().to_bytes().unwrap();
        let mut golden = vec![2, 0, 6];
        golden.extend_from_slice(b"node-a");
        golden.extend_from_slice(&[0, 6]);
        golden.extend_from_slice(b"node-b");
        golden.extend_from_slice(&[3, 0, 6]);
        golden.extend_from_slice(b"node-b");
        golden.extend_from_slice(&[0, 1, b'c', 0, 6]);
        golden.extend_from_slice(b"node-a");
        assert_eq!(bytes, golden);
        assert_eq!(Holders::decode(&bytes).unwrap(), table());
        assert_eq!(Holders::decode(&[]).unwrap(), Holders::default());
        assert_eq!(Holders::decode(&[0]).unwrap(), Holders(vec![vec![]]));
    }

    #[test]
    fn every_truncation_fails_or_is_a_page_prefix() {
        let bytes = table().to_bytes().unwrap();
        for cut in 0..bytes.len() {
            match Holders::decode(&bytes[..cut]) {
                Err(HolderError::Truncated) => {}
                // Cutting right after a page leaves a valid shorter table.
                Ok(h) => assert!(h.pages() < 2 || h.0[1].len() < 3, "cut {cut}"),
                Err(e) => panic!("cut {cut}: {e:?}"),
            }
        }
    }

    #[test]
    fn rejects_malformed() {
        assert_eq!(Holders::decode(&[1, 0, 0]), Err(HolderError::Malformed));
        assert_eq!(
            Holders(vec![vec![vec![]]]).to_bytes(),
            Err(HolderError::Malformed)
        );
        assert_eq!(
            Holders(vec![vec![vec![1]; 256]]).to_bytes(),
            Err(HolderError::Malformed)
        );
        assert_eq!(
            Holders(vec![vec![vec![1; 65_536]]]).to_bytes(),
            Err(HolderError::Malformed)
        );
    }

    #[test]
    fn validation() {
        let t = table();
        assert_eq!(t.validate(2, 2), Ok(()));
        assert_eq!(t.validate(3, 2), Err(HolderError::PageCount));
        assert_eq!(t.validate(2, 3), Err(HolderError::TooFew { page: 0 }));
        let dup = Holders(vec![vec![b"a".to_vec(), b"a".to_vec()]]);
        assert_eq!(dup.validate(1, 1), Err(HolderError::Duplicate { page: 0 }));
        assert_eq!(
            t.nodes(),
            vec![b"node-a".to_vec(), b"node-b".to_vec(), b"c".to_vec()]
        );
        assert_eq!(t.page(1).len(), 3);
        assert!(t.page(9).is_empty());
    }

    #[test]
    fn nodes_keep_first_seen_order_across_many_pages() {
        let ids: Vec<NodeIdBytes> = (0..64).map(|i| format!("node-{i}").into_bytes()).collect();
        // 50,000 pages, three holders each, rotating through 64 nodes.
        let t = Holders(
            (0..50_000usize)
                .map(|p| {
                    (0..3)
                        .map(|r| ids[(p + r * 7) % ids.len()].clone())
                        .collect()
                })
                .collect(),
        );
        let nodes = t.nodes();
        assert_eq!(nodes.len(), ids.len());
        let mut expected = Vec::new();
        for node in t.0.iter().flatten() {
            if !expected.contains(node) {
                expected.push(node.clone());
            }
        }
        assert_eq!(nodes, expected);
        assert!(Holders::default().nodes().is_empty());
    }
}
