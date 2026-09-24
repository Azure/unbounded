// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use serde::{Deserialize, Serialize};

use crate::{Error, Result, proto};

pub const DEFAULT_BYTES: u64 = 10 << 30;
pub const MIN_BYTES: u64 = 512 << 20;
pub const ALIGNMENT: u64 = 64 << 20;
pub const MAX_BYTES: u64 = i64::MAX as u64 / ALIGNMENT * ALIGNMENT;

/// Parse decimal, exponent, and binary Kubernetes quantity spellings exactly.
/// No floating point, rounding of fractional bytes, or saturation on overflow.
pub fn parse_cache_size(value: &str) -> Result<u64> {
    let invalid = || {
        Error(format!(
            "invalid cache size {value:?}: requires whole bytes between 512Mi and 8589934591.9375Gi"
        ))
    };
    let value = value.strip_prefix('+').unwrap_or(value);
    let split = value
        .bytes()
        .position(|b| !b.is_ascii_digit() && b != b'.')
        .unwrap_or(value.len());
    let (number, suffix) = value.split_at(split);
    let mut digits = String::new();
    let mut fractional = 0i64;
    let mut dot = false;
    for b in number.bytes() {
        if b == b'.' {
            if dot {
                return Err(invalid());
            }
            dot = true;
        } else {
            digits.push(b as char);
            if dot {
                fractional += 1;
            }
        }
    }
    if digits.is_empty() {
        return Err(invalid());
    }
    let (exponent, multiplier) = match suffix {
        "" => (0i64, 1u64),
        "n" => (-9, 1),
        "u" => (-6, 1),
        "m" => (-3, 1),
        "k" => (3, 1),
        "M" => (6, 1),
        "G" => (9, 1),
        "T" => (12, 1),
        "P" => (15, 1),
        "E" => (18, 1),
        "Ki" => (0, 1 << 10),
        "Mi" => (0, 1 << 20),
        "Gi" => (0, 1 << 30),
        "Ti" => (0, 1 << 40),
        "Pi" => (0, 1 << 50),
        "Ei" => (0, 1 << 60),
        s if s.starts_with(['e', 'E']) => {
            let exp = &s[1..];
            let unsigned = exp.strip_prefix(['+', '-']).unwrap_or(exp);
            if unsigned.is_empty() || !unsigned.bytes().all(|b| b.is_ascii_digit()) {
                return Err(invalid());
            }
            (exp.parse::<i64>().map_err(|_| invalid())?, 1)
        }
        _ => return Err(invalid()),
    };
    // Decimal long multiplication handles arbitrarily precise mantissas without
    // accepting a rounded near-integer. Only the final byte value is bounded.
    let mut product = Vec::with_capacity(digits.len() + 20);
    let mut carry = 0u128;
    for b in digits.bytes().rev() {
        carry += u128::from(b - b'0') * u128::from(multiplier);
        product.push((carry % 10) as u8);
        carry /= 10;
    }
    while carry > 0 {
        product.push((carry % 10) as u8);
        carry /= 10;
    }
    while product.last() == Some(&0) {
        product.pop();
    }
    if product.is_empty() {
        return Err(invalid());
    }
    let zeros = product.iter().take_while(|&&d| d == 0).count();
    let scale = exponent
        .checked_sub(fractional)
        .and_then(|s| s.checked_add(zeros as i64))
        .ok_or_else(invalid)?;
    if !(0..=19).contains(&scale) || product.len() - zeros + scale as usize > 19 {
        return Err(invalid());
    }
    let mut bytes = 0u64;
    for &d in product[zeros..].iter().rev() {
        bytes = bytes
            .checked_mul(10)
            .and_then(|v| v.checked_add(u64::from(d)))
            .ok_or_else(invalid)?;
    }
    bytes = bytes
        .checked_mul(10u64.pow(scale as u32))
        .ok_or_else(invalid)?;
    if !(MIN_BYTES..=MAX_BYTES).contains(&bytes) {
        return Err(invalid());
    }
    Ok(bytes.div_ceil(ALIGNMENT) * ALIGNMENT)
}

/// Only absence permits inheritance; an invalid present override is an error.
pub fn resolve_cache_size(node: Option<&str>, site: Option<&str>) -> Result<u64> {
    match node.or(site) {
        Some(value) => parse_cache_size(value),
        None => Ok(DEFAULT_BYTES),
    }
}

/// In-memory per-Node-UID intent. Runtime assigns ordered versions and revisions
/// from its reserved range before offering changed capacity.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct StoragePolicy {
    pub node: String,
    pub universe: String,
    /// Domain-separated identity derived from the immutable Kubernetes Node UID.
    pub identity: [u8; 32],
    pub revision: u64,
    pub version: u64,
    pub desired_bytes: u64,
    pub validation_error: Option<String>,
}

impl StoragePolicy {
    pub fn for_node(uid: &str) -> Self {
        Self::new(
            crate::model::identity("node", uid),
            crate::model::identity_bytes("storage", uid),
        )
    }

    pub fn new(node: String, identity: [u8; 32]) -> Self {
        Self {
            node,
            universe: String::new(),
            identity,
            revision: 0,
            version: 0,
            desired_bytes: 0,
            validation_error: None,
        }
    }

    /// Pure in-memory last-good update. Invalid intent records diagnostics but
    /// never offers the retained capacity as a replacement authoritative policy.
    pub fn resolve(&self, universe: &str, node: Option<&str>, site: Option<&str>) -> Result<Self> {
        let mut next = self.clone();
        next.universe = universe.into();
        match resolve_cache_size(node, site) {
            Ok(bytes) => {
                next.validation_error = None;
                if bytes != next.desired_bytes {
                    next.version = next
                        .version
                        .checked_add(1)
                        .ok_or_else(|| Error("storage version exhausted".into()))?;
                    next.desired_bytes = bytes;
                }
            }
            Err(error) => {
                next.validation_error = Some(error.to_string().chars().take(1024).collect())
            }
        }
        Ok(next)
    }

    pub fn wire(&self) -> Option<proto::StoragePolicy> {
        (self.version != 0 && self.validation_error.is_none()).then(|| proto::StoragePolicy {
            identity: self.identity.to_vec(),
            version: self.version,
            desired_bytes: self.desired_bytes,
        })
    }
}

impl crate::publication::Versioned for StoragePolicy {
    fn validate(&self) -> Result<()> {
        if self.node.len() != 64
            || hex::decode(&self.node).is_err()
            || (self.version == 0) != (self.desired_bytes == 0)
            || (self.version != 0
                && (!(MIN_BYTES..=MAX_BYTES).contains(&self.desired_bytes)
                    || !self.desired_bytes.is_multiple_of(ALIGNMENT)))
        {
            return Err(Error("invalid storage policy".into()));
        }
        Ok(())
    }
    fn revision(&self) -> u64 {
        self.revision
    }
    fn set_revision(&mut self, revision: u64) {
        self.revision = revision;
    }
    fn same_record(&self, other: &Self) -> bool {
        self.node == other.node && self.identity == other.identity
    }
}
