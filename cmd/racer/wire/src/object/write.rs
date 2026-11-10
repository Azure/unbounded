//! Client write request semantics and response heads, independent of I/O.
//!
//! The write value types ([`UploadMode`], [`Durability`], [`UploadState`]) live
//! here because racer-core depends on racer-wire; `racer_core::write`
//! re-exports them.

/// When a written object is uploaded to the origin, from `Racer-Upload`.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum UploadMode {
    Auto,
    Manual,
    Never,
}

impl UploadMode {
    /// The header token.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Auto => "auto",
            Self::Manual => "manual",
            Self::Never => "never",
        }
    }

    /// Parse the exact lowercase header token.
    pub fn parse(value: &[u8]) -> Option<Self> {
        match value {
            b"auto" => Some(Self::Auto),
            b"manual" => Some(Self::Manual),
            b"never" => Some(Self::Never),
            _ => None,
        }
    }

    /// Stable one-byte code used by binary encodings.
    pub fn code(self) -> u8 {
        match self {
            Self::Auto => 0,
            Self::Manual => 1,
            Self::Never => 2,
        }
    }

    /// Inverse of [`Self::code`].
    pub fn from_code(code: u8) -> Option<Self> {
        match code {
            0 => Some(Self::Auto),
            1 => Some(Self::Manual),
            2 => Some(Self::Never),
            _ => None,
        }
    }
}

/// Smallest and largest `replicas=N` value.
pub const MIN_REPLICAS: u8 = 1;
/// Matches `topology::placement::REPLICAS`.
pub const MAX_REPLICAS: u8 = 3;

/// When a write is acknowledged, from `Racer-Durability`.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum Durability {
    /// After this many durable owner copies, `1..=MAX_REPLICAS`.
    Replicas(u8),
    /// After the origin commit.
    Origin,
}

impl Durability {
    /// Parse `replicas=1..=3` or `origin`.
    pub fn parse(value: &[u8]) -> Option<Self> {
        match value {
            b"origin" => Some(Self::Origin),
            b"replicas=1" => Some(Self::Replicas(1)),
            b"replicas=2" => Some(Self::Replicas(2)),
            b"replicas=3" => Some(Self::Replicas(3)),
            _ => None,
        }
    }

    /// The header value.
    pub fn to_header(self) -> String {
        match self {
            Self::Replicas(n) => format!("replicas={n}"),
            Self::Origin => "origin".into(),
        }
    }
}

/// Upload progress of a written object, reported as `Racer-Upload-State`.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum UploadState {
    Pending,
    Manual,
    Uploading,
    Committed,
    Failed,
    Never,
}

impl UploadState {
    /// The header token.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Pending => "pending",
            Self::Manual => "manual",
            Self::Uploading => "uploading",
            Self::Committed => "committed",
            Self::Failed => "failed",
            Self::Never => "never",
        }
    }

    /// Parse the exact header token.
    pub fn parse(value: &[u8]) -> Option<Self> {
        Some(match value {
            b"pending" => Self::Pending,
            b"manual" => Self::Manual,
            b"uploading" => Self::Uploading,
            b"committed" => Self::Committed,
            b"failed" => Self::Failed,
            b"never" => Self::Never,
            _ => return None,
        })
    }

    /// Stable one-byte code used by binary encodings.
    pub fn code(self) -> u8 {
        match self {
            Self::Pending => 0,
            Self::Manual => 1,
            Self::Uploading => 2,
            Self::Committed => 3,
            Self::Failed => 4,
            Self::Never => 5,
        }
    }

    /// Inverse of [`Self::code`].
    pub fn from_code(code: u8) -> Option<Self> {
        Some(match code {
            0 => Self::Pending,
            1 => Self::Manual,
            2 => Self::Uploading,
            3 => Self::Committed,
            4 => Self::Failed,
            5 => Self::Never,
            _ => return None,
        })
    }

    /// The state a freshly committed object starts in.
    pub fn initial(upload: UploadMode) -> Self {
        match upload {
            UploadMode::Auto => Self::Pending,
            UploadMode::Manual => Self::Manual,
            UploadMode::Never => Self::Never,
        }
    }
}

#[cfg(test)]
mod value_tests {
    use super::*;

    #[test]
    fn tokens_round_trip() {
        for mode in [UploadMode::Auto, UploadMode::Manual, UploadMode::Never] {
            assert_eq!(UploadMode::parse(mode.as_str().as_bytes()), Some(mode));
            assert_eq!(UploadMode::from_code(mode.code()), Some(mode));
        }
        assert_eq!(UploadMode::parse(b"Auto"), None);
        assert_eq!(UploadMode::from_code(3), None);
        for state in [
            UploadState::Pending,
            UploadState::Manual,
            UploadState::Uploading,
            UploadState::Committed,
            UploadState::Failed,
            UploadState::Never,
        ] {
            assert_eq!(UploadState::parse(state.as_str().as_bytes()), Some(state));
            assert_eq!(UploadState::from_code(state.code()), Some(state));
        }
        assert_eq!(UploadState::from_code(6), None);
        for d in [
            Durability::Replicas(1),
            Durability::Replicas(2),
            Durability::Replicas(3),
            Durability::Origin,
        ] {
            assert_eq!(Durability::parse(d.to_header().as_bytes()), Some(d));
        }
        for bad in [
            &b"replicas=0"[..],
            b"replicas=4",
            b"replicas=02",
            b"",
            b"Origin",
        ] {
            assert_eq!(Durability::parse(bad), None);
        }
    }
}
