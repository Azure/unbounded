//! Historical challenge-discovery regression names, exercising the replacement
//! mutual connection handshake and the bounded canonical HTTP/signature codecs.
mod tests {
    use super::super::connection::tests::*;

    #[test]
    fn probe_is_fresh_exact_and_binds_both_node_identities() {
        signed_challenges_bind_both_identities_protocol_and_fresh_randomness();
    }
    #[test]
    fn certificate_verified_discovery_rejects_tampering_wrong_probe_and_wrong_receiver() {
        signed_challenges_bind_both_identities_protocol_and_fresh_randomness();
        loopback_mutual_authentication_pool_reuse_and_fresh_reconnect();
    }
    #[test]
    fn reply_codec_rejects_truncation_overflow_extra_bytes_and_chain_bombs() {
        super::super::connection::tests::handshake_codec_bounds();
    }
}
