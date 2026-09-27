//! Verified identities, signature chains, and separate page/credential AEAD domains.
pub mod aead;
pub mod certificates;
pub mod connection;
pub mod credentials;
pub mod forwarding;
pub mod identity;
pub mod keyring;
pub mod protocol;
#[cfg(test)]
mod replay;
#[cfg(test)]
mod session;
pub mod signing;
