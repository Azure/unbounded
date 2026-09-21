#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::Cell;
    #[test]
    fn generations_probes_cancellation_and_shared_scope() {
        let now = Rc::new(Cell::new(Instant::now()));
        let clock = now.clone();
        let breaker = CircuitBreaker::with_clock(Duration::from_secs(1), move || clock.get());
        let shared = breaker.clone();
        assert_eq!(breaker.status(), Status::Closed);
        let stale_success = breaker.try_acquire().unwrap();
        let stale_failure = shared.try_acquire().unwrap();
        breaker.try_acquire().unwrap().failure();
        stale_success.success();
        assert_eq!(shared.status(), Status::Open);
        assert!(shared.try_acquire().is_err());
        now.set(now.get() + Duration::from_secs(1));
        assert_eq!(shared.status(), Status::Open);
        assert!(shared.available());
        let probe = breaker.try_acquire().unwrap();
        assert_eq!(shared.status(), Status::HalfOpen);
        assert!(shared.try_acquire().is_err());
        stale_failure.failure();
        assert_eq!(shared.status(), Status::HalfOpen);
        probe.success();
        assert_eq!(shared.status(), Status::Closed);
        drop(shared.try_acquire().unwrap());
        breaker.try_acquire().unwrap().failure();
        now.set(now.get() + Duration::from_secs(1));
        drop(shared.try_acquire().unwrap());
        assert!(breaker.try_acquire().is_err());
        assert_eq!(breaker.status(), Status::Open);
        now.set(now.get() + Duration::from_secs(1));
        breaker.try_acquire().unwrap().success();
        assert!(breaker.try_acquire().is_ok());
    }
    #[test]
    fn stale_failure_cannot_reopen_recovered_breaker() {
        let breaker = CircuitBreaker::new(Duration::ZERO);
        let stale = breaker.try_acquire().unwrap();
        breaker.try_acquire().unwrap().failure();
        breaker.try_acquire().unwrap().success();
        stale.failure();
        let a = breaker.try_acquire().unwrap();
        let b = breaker.try_acquire().unwrap();
        a.success();
        b.success();
    }
}
