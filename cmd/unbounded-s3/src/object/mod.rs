// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

mod error;
mod traits;

#[cfg(test)]
pub(crate) mod memory_source;

pub use error::Error;
pub use traits::ObjectSource;
