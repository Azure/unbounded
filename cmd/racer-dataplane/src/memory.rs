//! Distinct accounting and lifetimes for plaintext, ciphertext, pipes, and kernel I/O.
//! Cache lookups retain the original encrypted page and immutable version metadata.
//! Eviction releases idle leases; retirement hides entries without revoking owners.
pub mod cache;
pub mod delivery;
pub mod page;
pub mod pipe;
pub mod pool;
