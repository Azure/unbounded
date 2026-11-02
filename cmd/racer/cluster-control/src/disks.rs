//! Raw cache disk approval and per-disk status.
//!
//! An administrator approves disks per Node in the
//! [`DISKS`](crate::annotations::DISKS) annotation, as a comma-separated list of
//! `/dev/disk/by-id` basenames. Approving a disk is consent for Racer to wipe
//! it. The dataplane reports what it found for every disk in the Pod report
//! annotation; see [`DiskStatus`].

use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

/// Largest accepted [`DISKS`](crate::annotations::DISKS) annotation.
pub const MAX_DISKS_ANNOTATION_BYTES: usize = 4096;

/// Parse a disk approval annotation value.
///
/// Entries are trimmed and empty entries (such as after a trailing comma) are
/// ignored. Duplicates are removed while keeping first-seen order. Returns
/// `None` when the value is too large or any entry is not a valid by-id
/// basename; the caller treats that as no approved disks.
pub fn parse_disks(value: &str) -> Option<Vec<String>> {
    if value.len() > MAX_DISKS_ANNOTATION_BYTES {
        return None;
    }
    let mut disks: Vec<String> = Vec::new();
    for entry in value.split(',').map(str::trim).filter(|e| !e.is_empty()) {
        if !racer_control_wire::valid_disk_id(entry) {
            return None;
        }
        if !disks.iter().any(|d| d == entry) {
            disks.push(entry.to_owned());
        }
    }
    (disks.len() <= racer_control_wire::MAX_DISKS).then_some(disks)
}

/// Approved disks from Node annotations. An invalid annotation approves none.
pub fn approved(annotations: &BTreeMap<String, String>) -> Vec<String> {
    let Some(value) = annotations.get(crate::annotations::DISKS) else {
        return Vec::new();
    };
    parse_disks(value).unwrap_or_else(|| {
        eprintln!(
            "racer-control: ignoring invalid {} annotation; approving no disks",
            crate::annotations::DISKS
        );
        Vec::new()
    })
}

/// Whether the retired regex selector annotation is still set.
pub fn has_legacy_selector(annotations: &BTreeMap<String, String>) -> bool {
    annotations.contains_key(crate::annotations::LEGACY_BLOCK_DEVICES)
}

/// What Racer did with one disk.
#[derive(Clone, Copy, Debug, Deserialize, Eq, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum DiskState {
    /// Approved and in use as cache storage.
    Claimed,
    /// Not approved, and could be approved.
    Available,
    /// Not approved, and could not be used if it were.
    Unavailable,
    /// Approved, but Racer could not use it.
    Rejected,
    /// Approved, but no disk with this ID is present.
    Missing,
}

/// Why a disk is unavailable or rejected.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum DiskReason {
    /// The disk or one of its partitions is mounted.
    Mounted,
    /// The disk or one of its partitions is held by another device (dm, md, LVM).
    Held,
    /// The disk is read-only.
    ReadOnly,
    /// The disk cannot hold its guards and one segment.
    TooSmall,
    /// Another process holds the disk open exclusively.
    Busy,
    /// The approved ID names a partition, not a whole disk.
    NotWholeDisk,
    /// An I/O error occurred while opening or validating the disk.
    IoError,
    /// Zeroing the guard regions failed.
    WipeFailed,
    /// Another approved disk is the same physical disk.
    Duplicate,
}

/// One disk in the Pod report annotation and `/debug/hardware`.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct DiskStatus {
    /// Preferred stable `/dev/disk/by-id` basename.
    pub id: String,
    /// All by-id basenames of the disk, sorted.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub aliases: Vec<String>,
    /// Kernel device name, such as `nvme0n1`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub dev: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub size_bytes: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub serial: Option<String>,
    pub state: DiskState,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<DiskReason>,
}

impl DiskStatus {
    /// An approved ID with no matching disk.
    pub fn missing(id: impl Into<String>) -> Self {
        Self {
            id: id.into(),
            aliases: Vec::new(),
            dev: None,
            size_bytes: None,
            model: None,
            serial: None,
            state: DiskState::Missing,
            reason: None,
        }
    }

    /// Whether truncation should keep this entry before others.
    pub fn important(&self) -> bool {
        matches!(
            self.state,
            DiskState::Claimed | DiskState::Rejected | DiskState::Missing
        )
    }
}

/// Rank of a by-id name as a stable disk ID; lower is preferred.
fn preference(name: &str) -> u8 {
    if name.starts_with("wwn-") {
        0
    } else if name.starts_with("nvme-eui.") {
        1
    } else if name.starts_with("nvme-") {
        2
    } else if name.starts_with("scsi-") {
        3
    } else if name.starts_with("ata-") {
        4
    } else {
        5
    }
}

/// Preferred stable ID among a disk's by-id names: `wwn-*`, then
/// `nvme-eui.*`, then other `nvme-*`, `scsi-*`, and `ata-*` names, with
/// lexical order breaking ties.
pub fn preferred_id<'a>(aliases: impl IntoIterator<Item = &'a String>) -> Option<&'a String> {
    aliases
        .into_iter()
        .min_by(|a, b| (preference(a), a.as_str()).cmp(&(preference(b), b.as_str())))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_trimmed_entries_and_rejects_invalid_annotations() {
        assert_eq!(
            parse_disks(" wwn-1 , nvme-eui.2,,wwn-1, ").unwrap(),
            ["wwn-1", "nvme-eui.2"]
        );
        assert_eq!(parse_disks("").unwrap(), Vec::<String>::new());
        assert_eq!(parse_disks(" , ,").unwrap(), Vec::<String>::new());
        for bad in ["a,../b", "a b", "a,.", "..", "a/b", "nvme-.*|x\u{e9}"] {
            assert_eq!(parse_disks(bad), None, "{bad}");
        }
        assert_eq!(
            parse_disks(&"a".repeat(MAX_DISKS_ANNOTATION_BYTES + 1)),
            None
        );
        let mut a = BTreeMap::new();
        assert!(approved(&a).is_empty());
        a.insert(crate::annotations::DISKS.into(), "wwn-1,wwn-2".into());
        assert_eq!(approved(&a), ["wwn-1", "wwn-2"]);
        a.insert(crate::annotations::DISKS.into(), "wwn-1,bad id".into());
        assert!(approved(&a).is_empty());
        assert!(!has_legacy_selector(&a));
        a.insert(
            crate::annotations::LEGACY_BLOCK_DEVICES.into(),
            "nvme".into(),
        );
        assert!(has_legacy_selector(&a));
    }

    #[test]
    fn preferred_id_orders_by_kind_then_name() {
        let names = |v: &[&str]| v.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        let pick = |v: &[&str]| preferred_id(&names(v)).cloned();
        assert_eq!(
            pick(&["nvme-Samsung_1", "nvme-eui.2", "wwn-0x2", "wwn-0x1"]).as_deref(),
            Some("wwn-0x1")
        );
        assert_eq!(
            pick(&["nvme-Samsung_1", "nvme-eui.2"]).as_deref(),
            Some("nvme-eui.2")
        );
        assert_eq!(
            pick(&["ata-b", "scsi-a", "usb-x"]).as_deref(),
            Some("scsi-a")
        );
        assert_eq!(pick(&["usb-b", "usb-a"]).as_deref(), Some("usb-a"));
        assert_eq!(pick(&[]), None);
    }
}
