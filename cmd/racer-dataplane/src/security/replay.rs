//! Regression names retained from node-global replay admission. The contract now
//! belongs to exclusive connection sessions, with no node-wide replay allocation.
mod tests {
    use super::super::connection::tests::*;

    #[test]
    fn duplicate_epoch_binding_and_saturation_do_not_evict_live_entries() {
        replay_and_binding_checks();
        more_than_4096_frames_at_fixed_time_have_constant_session_storage();
    }
    #[test]
    fn exact_expiry_future_skew_and_clock_rollback_fail_closed() {
        super::super::signing::tests::malformed_fields_unknown_algorithm_and_historical_expiry();
        reconnect_restart_expiry_and_counter_overflow_fail_closed();
    }
    #[test]
    fn atomic_across_workers_and_node_capacity_not_per_handle() {
        let workers: Vec<_> = (0..4)
            .map(|_| {
                std::thread::spawn(|| {
                    loopback_mutual_authentication_pool_reuse_and_fresh_reconnect();
                })
            })
            .collect();
        for worker in workers {
            worker.join().unwrap();
        }
    }
    #[test]
    fn challenges_are_shared_random_and_restart_invalidates_old_envelopes() {
        signed_challenges_bind_both_identities_protocol_and_fresh_randomness();
        reconnect_restart_expiry_and_counter_overflow_fail_closed();
    }
    #[test]
    fn rejects_malformed_identity_epoch_time_and_zero_capacity() {
        replay_and_binding_checks();
        super::super::signing::tests::malformed_fields_unknown_algorithm_and_historical_expiry();
    }
}
