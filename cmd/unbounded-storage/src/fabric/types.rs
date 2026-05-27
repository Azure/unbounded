// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Public-facing fabric value types. Kept thin so later phases can
//! attach transport metadata without touching the bufferpool surface.

use crate::bufferpool::{PageRef, PeerId};

pub type ConnectionId = PeerId;

#[derive(Clone, Debug)]
pub struct ConnectionSpec {
    pub peer: ConnectionId,
    pub wire_addr: String,
    pub hca_numa: Option<u16>,
}

/// Newtype around a buffer-pool page so the fabric layer can attach
/// transport-specific metadata in later phases without churning the
/// bufferpool types.
#[derive(Copy, Clone, Debug)]
pub struct Page(pub PageRef);

impl From<PageRef> for Page {
    fn from(p: PageRef) -> Self {
        Page(p)
    }
}

impl From<Page> for PageRef {
    fn from(p: Page) -> Self {
        p.0
    }
}
