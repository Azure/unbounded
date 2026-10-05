//! Explicit wall/monotonic clock drift observation, independent of cache policy.

use std::time::{Duration, Instant, SystemTime};

#[derive(Default)]
pub struct Observer {
    sample: Option<(SystemTime, Instant)>,
    epoch: u64,
}
impl Observer {
    pub fn epoch(&self) -> u64 {
        self.epoch
    }

    pub fn observe(&mut self, wall: SystemTime, monotonic: Instant, threshold: Duration) -> bool {
        let uncertain = self.sample.is_some_and(|(previous_wall, previous_mono)| {
            match (
                wall.duration_since(previous_wall),
                monotonic.checked_duration_since(previous_mono),
            ) {
                (Ok(wall_elapsed), Some(elapsed)) => wall_elapsed.abs_diff(elapsed) > threshold,
                _ => true,
            }
        });
        self.sample = Some((wall, monotonic));
        if uncertain {
            self.epoch = self.epoch.saturating_add(1);
        }
        uncertain
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn strict_threshold_backwards_samples_and_epoch_saturation() {
        let mut observer = Observer::default();
        let mono = Instant::now();
        let wall = SystemTime::UNIX_EPOCH + Duration::from_secs(100);
        let second = Duration::from_secs(1);
        assert!(!observer.observe(wall, mono, second));
        assert!(!observer.observe(wall + second, mono, second));
        assert!(observer.observe(wall, mono, second));
        assert!(observer.observe(wall, mono - second, second));
        assert_eq!(observer.epoch(), 2);
        observer.epoch = u64::MAX;
        assert!(observer.observe(wall + second * 3, mono, second));
        assert_eq!(observer.epoch(), u64::MAX);
    }
}
