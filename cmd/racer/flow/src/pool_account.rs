//! Idle pool bytes charged to an admission class.
//!
//! [`QuotaPoolAccount`] adapts aggregate quota counters to
//! [`page_alloc::buf::PoolAccount`], so idle bytes held by a worker's
//! [`page_alloc::buf::Pool`] count against a dedicated class (the planned
//! `ResourceClass::Pool`) that admission sees. It replaced the removed flow
//! recycler's retained-bytes accounting. Charging is
//! lock-free and safe from any thread that returns a buffer.

use crate::{Policy, SharedQuotas};

/// Charges idle pool bytes to one class of a shared quota authority.
pub struct QuotaPoolAccount<P: Policy> {
    quotas: SharedQuotas<P>,

    class: P::Class,
}

impl<P: Policy> QuotaPoolAccount<P> {
    /// Charge idle bytes to `class` of `quotas`.
    pub fn new(quotas: SharedQuotas<P>, class: P::Class) -> Self {
        Self { quotas, class }
    }

    /// Idle bytes currently charged to the class by all accounts.
    pub fn used(&self) -> usize {
        self.quotas.used(self.class)
    }
}

impl<P> page_alloc::buf::PoolAccount for QuotaPoolAccount<P>
where
    P: Policy + Send + Sync,
    P::Class: Send + Sync,
{
    /// Idle bytes are admitted up to the class ceiling, even after stop:
    /// refusing only frees storage that already exists.
    fn charge_idle(&self, bytes: usize) -> bool {
        let limit = self.quotas.policy.limit(self.class);
        self.quotas
            .totals
            .counter(self.class)
            .reserve(bytes, limit)
            .is_ok()
    }

    fn release_idle(&self, bytes: usize) {
        self.quotas.totals.counter(self.class).release(bytes);
        if P::wakes(self.class) {
            self.quotas.totals.wake.wake();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{Class, Quotas, Rejection};
    use page_alloc::buf::{Pool, PoolAccount, PoolConfig};

    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    enum Resource {
        Payload,
        Pool,
    }

    impl Class for Resource {
        const COUNT: usize = 2;

        fn index(self) -> usize {
            self as usize
        }

        fn requires_wipe(self) -> bool {
            false
        }
    }

    #[derive(Clone)]
    struct Limits;

    impl Policy for Limits {
        type Class = Resource;
        type Key = u8;

        fn limit(&self, class: Resource) -> usize {
            match class {
                Resource::Payload => 1 << 20,
                Resource::Pool => 8192,
            }
        }

        fn max_keys(&self) -> usize {
            4
        }

        fn wakes(_class: Resource) -> bool {
            true
        }

        fn covers(_class: Resource) -> bool {
            true
        }

        fn rejected(&self, _rejection: Rejection<Resource>) {}
    }

    #[test]
    fn charges_and_releases_the_pool_class() {
        let quotas = Quotas::new(Limits);
        let account = QuotaPoolAccount::new(quotas.shared(), Resource::Pool);
        assert!(account.charge_idle(4096));
        assert!(account.charge_idle(4096));
        assert!(!account.charge_idle(1), "class ceiling applies");
        assert_eq!(quotas.used(Resource::Pool), 8192);
        account.release_idle(8192);
        assert_eq!(quotas.used(Resource::Pool), 0);
        assert_eq!(quotas.used(Resource::Payload), 0);
    }

    #[test]
    fn pool_idle_bytes_are_visible_to_admission() {
        let quotas = Quotas::new(Limits);
        let account = QuotaPoolAccount::new(quotas.shared(), Resource::Pool);
        let mut pool = Pool::new(
            PoolConfig {
                alignment: 512,
                max_idle_per_class: 8,
            },
            account,
        )
        .unwrap();
        let live = quotas.shared().reserve(Resource::Payload, 4096).unwrap();
        let buf = pool.take(4096, live, false).unwrap();
        assert_eq!(quotas.used(Resource::Payload), 4096);
        drop(buf);
        assert_eq!(quotas.used(Resource::Payload), 0);
        assert_eq!(quotas.used(Resource::Pool), 4096);
        let bufs: Vec<_> = (0..3)
            .map(|_| pool.take(4096, (), false).unwrap())
            .collect();
        assert_eq!(quotas.used(Resource::Pool), 0);
        drop(bufs);
        assert_eq!(
            quotas.used(Resource::Pool),
            8192,
            "third return exceeds the class"
        );
        drop(pool);
        assert_eq!(quotas.used(Resource::Pool), 0);
    }
}
