//! Pure control identities, records, and syntax validation. No runtime policy.
use std::{collections::HashSet, num::NonZeroU32, path::PathBuf};

pub type Result<T> = std::result::Result<T, Error>;

/// Payload-free failures. Applications map these into their own error domain.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    InvalidRequest,
    IncompatibleMembership,
    Overloaded,
    Replay,
}
impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for Error {}

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct ClusterId(pub String);
/// Kubernetes ClusterCache UID, not its reusable resource name.
#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct CacheId(pub String);
/// Kubernetes Node UID, not its reusable resource name.
#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct NodeId(pub String);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct MembershipVersion(pub u64);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct KeyId(pub [u8; 16]);
impl KeyId {
    /// Construct an epoch-bound ID with a controller-selected uniqueness suffix.
    pub fn from_generation(generation: u64, suffix: u32) -> Result<Self> {
        if generation == 0 {
            return Err(Error::InvalidRequest);
        }
        let mut bytes = [0; 16];
        bytes[..4].copy_from_slice(b"RKG1");
        bytes[4..12].copy_from_slice(&generation.to_be_bytes());
        bytes[12..].copy_from_slice(&suffix.to_be_bytes());
        Ok(Self(bytes))
    }

    /// Read the nonzero creation generation of an RKG1 ID, not bundle admission policy.
    pub fn generation(self) -> Option<u64> {
        (self.0[..4] == *b"RKG1")
            .then(|| u64::from_be_bytes(self.0[4..12].try_into().expect("generation bytes")))
            .filter(|generation| *generation != 0)
    }
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct RailId(pub u16);
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RailMapping {
    pub rail: RailId,
    pub device: String,
    pub port: u8,
    pub gid: Option<[u8; 16]>,
    pub numa_node: Option<usize>,
}
/// Wire membership record, deliberately not a placement algorithm input trait.
#[derive(Clone, Debug)]
pub struct Member {
    pub node: NodeId,
    pub shares: NonZeroU32,
    pub peer_endpoint: String,
    pub rails: Vec<RailMapping>,
    pub site: String,
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CacheDefinition {
    pub id: CacheId,
    pub name: String,
    pub client_socket: PathBuf,
    pub origin_socket: PathBuf,
}

/// Canonical lowercase UUID syntax only; callers choose nil/version policy.
pub fn valid_uuid(value: &str) -> bool {
    value.len() == 36
        && value.bytes().enumerate().all(|(i, b)| {
            if matches!(i, 8 | 13 | 18 | 23) {
                b == b'-'
            } else {
                b.is_ascii_digit() || (b'a'..=b'f').contains(&b)
            }
        })
}

pub fn valid_site(value: &str) -> bool {
    value.is_empty()
        || (value.len() <= 63
            && value.as_bytes()[0].is_ascii_alphanumeric()
            && value.as_bytes()[value.len() - 1].is_ascii_alphanumeric()
            && value
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.')))
}

pub fn canonical_socket_paths(name: &str) -> Result<(PathBuf, PathBuf)> {
    if name.is_empty()
        || name.len() > 253
        || !name.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && label.bytes().enumerate().all(|(i, b)| {
                    b.is_ascii_lowercase()
                        || b.is_ascii_digit()
                        || b == b'-' && i != 0 && i + 1 != label.len()
                })
        })
    {
        return Err(Error::InvalidRequest);
    }
    let client = format!("/run/racer/{name}/client/socket");
    let origin = format!("/run/racer/{name}/origin/socket");
    if client.len() > 107 || origin.len() > 107 {
        return Err(Error::InvalidRequest);
    }
    Ok((client.into(), origin.into()))
}

pub fn validate_definitions(definitions: &[CacheDefinition]) -> Result<()> {
    let mut ids = HashSet::new();
    let mut names = HashSet::new();
    for d in definitions {
        if !crate::valid_uuid(&d.id.0) || !ids.insert(&d.id) || !names.insert(&d.name) {
            return Err(Error::InvalidRequest);
        }
        let (client, origin) = canonical_socket_paths(&d.name)?;
        // Path equality normalizes separators; the wire requires exact strings.
        if d.client_socket.as_os_str() != client.as_os_str()
            || d.origin_socket.as_os_str() != origin.as_os_str()
        {
            return Err(Error::InvalidRequest);
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn key_generation_requires_magic_and_nonzero_epoch_but_not_a_suffix_policy() {
        assert_eq!(KeyId::from_generation(0, 0), Err(Error::InvalidRequest));
        for generation in [1, 256, u64::MAX] {
            for suffix in [0, u32::MAX] {
                let id = KeyId::from_generation(generation, suffix).unwrap();
                assert_eq!(id.generation(), Some(generation));
                let mut invalid = id;
                invalid.0[0] ^= 1;
                assert_eq!(invalid.generation(), None);
                invalid = id;
                invalid.0[4..12].fill(0);
                assert_eq!(invalid.generation(), None);
            }
        }
        assert_eq!(KeyId([0; 16]).generation(), None);
    }

    #[test]
    fn uuid_syntax_rejects_aliases_without_enforcing_version_or_nil_policy() {
        for value in [
            "00000000-0000-0000-0000-000000000000",
            "ffffffff-ffff-ffff-ffff-ffffffffffff",
            "01234567-89ab-cdef-0123-456789abcdef",
        ] {
            assert!(valid_uuid(value));
        }
        for value in [
            "",
            "0123456789abcdef0123456789abcdef",
            "01234567-89AB-cdef-0123-456789abcdef",
            "01234567_89ab-cdef-0123-456789abcdef",
            "01234567-89ab-cdef-0123-456789abcdeg",
            "01234567-89ab-cdef-0123-456789abcde\n",
            "01234567-89ab-cdef-0123-456789abcdé",
        ] {
            assert!(!valid_uuid(value), "{value:?}");
        }
    }
}
