// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Blocking HTTP discovery for fabric endpoint candidates.

mod client;
mod server;

pub use client::fetch;
pub use server::Server;

const PATH: &str = "/v1/fabric";
const MAX_REQUEST_BYTES: usize = 8 * 1024;
const MAX_RESPONSE_BYTES: usize = 256 * 1024;

#[cfg(test)]
mod tests;
