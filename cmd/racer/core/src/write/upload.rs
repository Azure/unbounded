//! Azure Blob block IDs, the Put Block List body, and holder assignment for uploads.

use super::holders::{Holders, NodeIdBytes};

/// Base64-encoded block ID length. Azure requires one length per blob.
pub const BLOCK_ID_BYTES: usize = 64;
/// Default bound on pages per `WriteUpload` batch.
pub const MAX_UPLOAD_BATCH: usize = 16;

const TABLE: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

/// Fixed-length block ID: standard base64 of the 48 ASCII bytes
/// `"{version_hex32}{page:016x}"`. 48 bytes encode to exactly 64 characters
/// without padding. Callers URL-escape it in the Put Block query (`+`, `/`).
pub fn block_id(version_hex: &[u8; 32], page: u64) -> [u8; BLOCK_ID_BYTES] {
    let mut raw = [0u8; 48];
    raw[..32].copy_from_slice(version_hex);
    const HEX: &[u8; 16] = b"0123456789abcdef";
    for i in 0..16 {
        raw[32 + i] = HEX[((page >> (60 - 4 * i)) & 0xf) as usize];
    }
    let mut out = [0u8; BLOCK_ID_BYTES];
    for (chunk, dst) in raw.chunks_exact(3).zip(out.chunks_exact_mut(4)) {
        let n = (u32::from(chunk[0]) << 16) | (u32::from(chunk[1]) << 8) | u32::from(chunk[2]);
        for (j, d) in dst.iter_mut().enumerate() {
            *d = TABLE[((n >> (18 - 6 * j)) & 0x3f) as usize];
        }
    }
    out
}

/// Put Block List body committing pages `0..pages` in order:
/// `<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>id</Latest>...</BlockList>`.
/// Block IDs contain only base64 characters, so no XML escaping is needed.
pub fn block_list_xml(version_hex: &[u8; 32], pages: u64) -> Vec<u8> {
    const OPEN: &[u8] = b"<?xml version=\"1.0\" encoding=\"utf-8\"?><BlockList>";
    const CLOSE: &[u8] = b"</BlockList>";
    let per = b"<Latest></Latest>".len() + BLOCK_ID_BYTES;
    let mut out = Vec::with_capacity(OPEN.len() + CLOSE.len() + per * pages as usize);
    out.extend_from_slice(OPEN);
    for page in 0..pages {
        out.extend_from_slice(b"<Latest>");
        out.extend_from_slice(&block_id(version_hex, page));
        out.extend_from_slice(b"</Latest>");
    }
    out.extend_from_slice(CLOSE);
    out
}

/// Pages assigned to holders for upload.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct UploadPlan {
    /// One entry per `WriteUpload`: a holder and at most `max_batch`
    /// ascending pages. Contiguous pages on the same holder share a batch.
    pub batches: Vec<(NodeIdBytes, Vec<u64>)>,

    /// Pages with no reachable holder, in ascending order.
    pub unassigned: Vec<u64>,
}

impl UploadPlan {
    /// Assign each page in `pages` (ascending) to its first reachable holder.
    pub fn new(
        holders: &Holders,
        pages: impl IntoIterator<Item = u64>,
        reachable: impl Fn(&[u8]) -> bool,
        max_batch: usize,
    ) -> Self {
        let max_batch = max_batch.max(1);
        let mut plan = Self::default();
        let mut last: Option<u64> = None;
        for page in pages {
            let Some(node) = holders.page(page).iter().find(|n| reachable(n)) else {
                plan.unassigned.push(page);
                last = None;
                continue;
            };
            match plan.batches.last_mut() {
                Some((current, batch))
                    if current == node
                        && batch.len() < max_batch
                        && last.is_some_and(|l| l + 1 == page) =>
                {
                    batch.push(page);
                }
                _ => plan.batches.push((node.clone(), vec![page])),
            }
            last = Some(page);
        }
        plan
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const VERSION: &[u8; 32] = b"0123456789abcdef0123456789abcdef";

    fn decode64(s: &[u8]) -> Vec<u8> {
        let mut out = Vec::new();
        for chunk in s.chunks(4) {
            let mut n = 0u32;
            for &c in chunk {
                n = (n << 6) | TABLE.iter().position(|&t| t == c).unwrap() as u32;
            }
            out.extend_from_slice(&[(n >> 16) as u8, (n >> 8) as u8, n as u8]);
        }
        out
    }

    #[test]
    fn block_ids_are_fixed_length_and_decode() {
        for page in [0, 1, 49_999, u64::MAX] {
            let id = block_id(VERSION, page);
            assert_eq!(id.len(), 64);
            assert!(id.iter().all(|c| TABLE.contains(c)));
            let raw = decode64(&id);
            assert_eq!(&raw[..32], VERSION);
            assert_eq!(raw[32..], *format!("{page:016x}").as_bytes());
        }
        assert_ne!(block_id(VERSION, 0), block_id(VERSION, 1));
        assert_eq!(
            &block_id(VERSION, 0),
            b"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWYwMDAwMDAwMDAwMDAwMDAw"
        );
    }

    #[test]
    fn xml_golden() {
        let xml = String::from_utf8(block_list_xml(VERSION, 2)).unwrap();
        let id0 = String::from_utf8(block_id(VERSION, 0).to_vec()).unwrap();
        let id1 = String::from_utf8(block_id(VERSION, 1).to_vec()).unwrap();
        assert_eq!(
            xml,
            format!(
                "<?xml version=\"1.0\" encoding=\"utf-8\"?><BlockList><Latest>{id0}</Latest><Latest>{id1}</Latest></BlockList>"
            )
        );
        assert_eq!(
            block_list_xml(VERSION, 0),
            b"<?xml version=\"1.0\" encoding=\"utf-8\"?><BlockList></BlockList>"
        );
    }

    #[test]
    fn plan_batches_contiguous_pages_per_first_reachable_holder() {
        let n = |s: &str| s.as_bytes().to_vec();
        let holders = Holders(vec![
            vec![n("a"), n("b")],
            vec![n("a"), n("b")],
            vec![n("a"), n("b")],
            vec![n("b"), n("a")],
            vec![n("a")],
            vec![n("x")],
            vec![n("a")],
        ]);
        let plan = UploadPlan::new(&holders, 0..7, |node| node != b"x", 2);
        assert_eq!(
            plan.batches,
            vec![
                (n("a"), vec![0, 1]),
                (n("a"), vec![2]),
                (n("b"), vec![3]),
                (n("a"), vec![4]),
                (n("a"), vec![6]),
            ]
        );
        assert_eq!(plan.unassigned, vec![5]);
        let down = UploadPlan::new(&holders, 0..4, |node| node != b"a", 16);
        assert_eq!(down.batches, vec![(n("b"), vec![0, 1, 2, 3])]);
        let zero = UploadPlan::new(&holders, [0, 1], |_| true, 0);
        assert_eq!(zero.batches.len(), 2);
        let sparse = UploadPlan::new(&holders, [0, 2], |_| true, 16);
        assert_eq!(sparse.batches.len(), 2);
    }
}
