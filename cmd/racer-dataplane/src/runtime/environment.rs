//! Shared runtime clock/entropy with the Racer TLS time adapter.
pub use uring_runtime::environment::*;

pub fn unix_time() -> rustls::pki_types::UnixTime {
    rustls::pki_types::UnixTime::since_unix_epoch(
        wall_now()
            .duration_since(std::time::SystemTime::UNIX_EPOCH)
            .unwrap_or_default(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{error::Error, model::RequestId, runtime::deadline::RequestScope};
    use std::time::Duration;

    #[test]
    fn replay_and_nested_worlds_preserve_time_entropy_and_deadlines() {
        let clock = SimulationClock::new(7);
        let role = clock.environment(11);
        let _guard = role.enter();
        let start = now();
        let wall = wall_now();
        let scope = RequestScope::new(RequestId([1; 16]), start + Duration::from_secs(2)).unwrap();
        let wire = crate::security::protocol::encode_deadline(scope.deadline).unwrap();
        let mut first = [0; 64];
        fill_random(&mut first[..3]).unwrap();
        fill_random(&mut first[3..]).unwrap();
        {
            let replay = SimulationClock::new(7);
            let _nested = replay.environment(11).enter();
            let mut bytes = [0; 64];
            fill_random(&mut bytes).unwrap();
            assert_eq!(bytes, first);
            assert_eq!(wall_now(), wall);
            replay.advance(Duration::from_secs(500));
        }
        assert_eq!(now(), start);
        clock.advance(Duration::from_secs(2));
        assert_eq!(scope.check(), Err(Error::DeadlineExceeded));
        clock.set_wall_time(wall - Duration::from_secs(60));
        assert_eq!(now(), start + Duration::from_secs(2));
        assert_eq!(
            crate::security::protocol::encode_deadline(scope.deadline).unwrap(),
            wire
        );
        assert_eq!(
            crate::security::protocol::decode_deadline(wire).unwrap().0,
            scope.deadline.0
        );
    }
}
