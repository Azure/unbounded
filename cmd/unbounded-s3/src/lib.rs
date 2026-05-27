// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

pub mod catalog;
pub mod object;
pub mod server;

mod storage_backend;
pub use storage_backend::BlockStoreObjectSource;

#[cfg(test)]
mod tests;
