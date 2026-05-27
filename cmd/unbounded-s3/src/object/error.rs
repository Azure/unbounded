// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::fmt;
use std::sync::Arc;

/// Errors produced by the object source and streamed back through
/// the S3 response path.
#[derive(Debug, Clone)]
pub enum Error {
    Io(Arc<std::io::Error>),
    Transport(String),
    Internal(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Io(e) => write!(f, "io: {e}"),
            Error::Transport(s) => write!(f, "transport: {s}"),
            Error::Internal(s) => write!(f, "internal: {s}"),
        }
    }
}

impl std::error::Error for Error {}

impl From<unbounded_storage::bufferpool::Error> for Error {
    fn from(e: unbounded_storage::bufferpool::Error) -> Self {
        Error::Transport(e.to_string())
    }
}

impl From<std::io::Error> for Error {
    fn from(e: std::io::Error) -> Self {
        Error::Io(Arc::new(e))
    }
}
