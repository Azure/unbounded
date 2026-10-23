//! Acquisition credits: deadline, attempts, and route links.
//!
//! Errors are racer-core errors; the dataplane converts them with `?`.

use super::error::{Error, Result};
use std::time::Instant;

/// Original acquisition budget, owned by one metadata/bootstrap operation or
/// page acquisition. Client range progress admits distinct pages with separate
/// bounded allowances under the same deadline. Retry never reconstructs an
/// acquisition budget from defaults. Fanout must partition credits;
/// route attempts must charge actual forwarded links here as well as on the wire.
/// No Clone: a replacement caller uses its own remaining credits, not fresh ones.
/// ```compile_fail
/// use racer_core::read::AcquisitionBudget;
/// fn duplicate(budget: AcquisitionBudget) { let _other = budget.clone(); }
/// ```
pub struct AcquisitionBudget {
    deadline: Instant,
    attempts: u32,
    links: u8,
    charged_links: u16,
    failed_route: bool,
}
impl AcquisitionBudget {
    pub fn new(deadline: Instant, attempts: u32, links: u8) -> Self {
        Self {
            deadline,
            attempts,
            links,
            charged_links: 0,
            failed_route: false,
        }
    }

    /// Pure budget validation/debit. The driver also checks original cancellation
    /// and candidate authority before I/O. Return the tighter original deadline.
    pub fn begin_attempt(&mut self, now: Instant, caller_deadline: Instant) -> Result<Instant> {
        let deadline = self.deadline.min(caller_deadline);
        if now >= deadline {
            return Err(Error::DeadlineExceeded);
        }
        self.attempts = self.attempts.checked_sub(1).ok_or(Error::Unavailable)?;
        Ok(deadline)
    }

    pub fn charge_links(&mut self, links: u8) -> Result<()> {
        self.links = self
            .links
            .checked_sub(links)
            .ok_or(Error::HopBudgetExhausted)?;
        self.charged_links = self.charged_links.saturating_add(u16::from(links));
        Ok(())
    }

    /// Reconcile unused links only after a verified completed route. Refunds
    /// cannot exceed prior debits; failed exchanges must retain their full debit.
    pub fn refund_links(&mut self, links: u8) -> Result<()> {
        let charged = self
            .charged_links
            .checked_sub(u16::from(links))
            .ok_or(Error::InvalidRequest)?;
        let available = self.links.checked_add(links).ok_or(Error::InvalidRequest)?;
        self.charged_links = charged;
        self.links = available;
        Ok(())
    }

    pub fn deadline(&self) -> Instant {
        self.deadline
    }
    pub fn remaining_attempts(&self) -> u32 {
        self.attempts
    }
    pub fn remaining_links(&self) -> u8 {
        self.links
    }

    /// Both normal and failure ceilings draw from credits allocated by ingress.
    /// Observing failure changes the per-attempt ceiling, never adds credits.
    pub fn route_links(&self) -> u8 {
        self.links.min(if self.failed_route { 8 } else { 4 })
    }
    pub fn note_route_failure(&mut self) {
        self.failed_route = true;
    }

    /// Consume an owned child's unused credits exactly once. Do not reconstruct a
    /// default budget or refund uncertain work after a lost remote response.
    pub fn reunite(&mut self, child: Self) -> Result<()> {
        let attempts = self
            .attempts
            .checked_add(child.attempts)
            .ok_or(Error::InvalidRequest)?;
        let links = self
            .links
            .checked_add(child.links)
            .ok_or(Error::InvalidRequest)?;
        self.deadline = self.deadline.min(child.deadline);
        self.attempts = attempts;
        self.links = links;
        self.failed_route |= child.failed_route;
        Ok(())
    }

    /// Move credits into an independently owned worker/range child. Failure is
    /// atomic; neither fanout nor a worker transfer can mint original credits.
    pub fn partition(&mut self, attempts: u32, links: u8) -> Result<Self> {
        let remaining_attempts = self
            .attempts
            .checked_sub(attempts)
            .ok_or(Error::Unavailable)?;
        let remaining_links = self
            .links
            .checked_sub(links)
            .ok_or(Error::HopBudgetExhausted)?;
        self.attempts = remaining_attempts;
        self.links = remaining_links;
        let mut child = Self::new(self.deadline, attempts, links);
        child.failed_route = self.failed_route;
        Ok(child)
    }

    pub fn transfer(&mut self) -> Self {
        let attempts = std::mem::take(&mut self.attempts);
        let links = std::mem::take(&mut self.links);
        Self {
            deadline: self.deadline,
            attempts,
            links,
            charged_links: std::mem::take(&mut self.charged_links),
            failed_route: self.failed_route,
        }
    }

    /// Atomically debit a peer route's attempt and all forwarded links before I/O.
    pub fn begin_peer_attempt(
        &mut self,
        now: Instant,
        caller_deadline: Instant,
        links: u8,
    ) -> Result<Instant> {
        if links == 0 {
            return Err(Error::InvalidRequest);
        }
        let deadline = self.deadline.min(caller_deadline);
        if now >= deadline {
            return Err(Error::DeadlineExceeded);
        }
        if self.attempts == 0 {
            return Err(Error::Unavailable);
        }
        let remaining = self
            .links
            .checked_sub(links)
            .ok_or(Error::HopBudgetExhausted)?;
        self.attempts -= 1;
        self.links = remaining;
        self.charged_links = self.charged_links.saturating_add(u16::from(links));
        Ok(deadline)
    }
}

/// Maximum links any route budget may carry (see `designs/racer-core-simulation.md`).
pub const MAX_ROUTE_LINKS: u8 = 8;

/// Links per attempt before any route failure has been observed.
pub const NORMAL_ROUTE_LINKS: u8 = 4;

/// Inherit a forwarded peer's remaining route allowance without extending its
/// deadline. `scope_deadline` is the local request deadline; the route fields
/// come from the verified forwarding budget. The caller checks cancellation.
pub fn inherited_budget(
    scope_deadline: Instant,
    route_deadline: Instant,
    remaining_attempts: u32,
    remaining_links: u8,
    visited: usize,
) -> Result<AcquisitionBudget> {
    if remaining_links > MAX_ROUTE_LINKS || remaining_links == 0 || visited == 0 {
        return Err(Error::HopBudgetExhausted);
    }
    let mut budget = AcquisitionBudget::new(
        scope_deadline.min(route_deadline),
        remaining_attempts,
        remaining_links
            .checked_sub(1)
            .ok_or(Error::HopBudgetExhausted)?,
    );
    if remaining_links > NORMAL_ROUTE_LINKS {
        budget.note_route_failure();
    }
    Ok(budget)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    #[test]
    fn begin_attempt_debits_and_respects_tighter_deadline() {
        let now = Instant::now();
        let mut budget = AcquisitionBudget::new(now + Duration::from_secs(5), 1, 8);
        let deadline: Instant = budget
            .begin_attempt(now, now + Duration::from_secs(1))
            .unwrap();
        assert_eq!(deadline, now + Duration::from_secs(1));
        assert_eq!(budget.remaining_attempts(), 0);
        assert_eq!(
            budget.begin_attempt(now, now + Duration::from_secs(1)),
            Err(Error::Unavailable)
        );
        assert_eq!(
            budget.begin_attempt(now + Duration::from_secs(5), now + Duration::from_secs(9)),
            Err(Error::DeadlineExceeded)
        );
    }

    #[test]
    fn links_charge_refund_and_never_mint() {
        let now = Instant::now();
        let mut budget = AcquisitionBudget::new(now, 3, 8);
        budget.charge_links(3).unwrap();
        assert_eq!(budget.remaining_links(), 5);
        assert_eq!(budget.refund_links(4), Err(Error::InvalidRequest));
        budget.refund_links(3).unwrap();
        assert_eq!(budget.remaining_links(), 8);
        assert_eq!(budget.charge_links(9), Err(Error::HopBudgetExhausted));
        assert_eq!(budget.route_links(), 4);
        budget.note_route_failure();
        assert_eq!(budget.route_links(), 8);
    }

    #[test]
    fn partition_and_reunite_conserve_credits() {
        let now = Instant::now();
        let mut parent = AcquisitionBudget::new(now, 4, 8);
        let child = parent.partition(3, 5).unwrap();
        assert_eq!(
            (parent.remaining_attempts(), parent.remaining_links()),
            (1, 3)
        );
        assert!(matches!(parent.partition(2, 0), Err(Error::Unavailable)));
        assert!(matches!(
            parent.partition(0, 4),
            Err(Error::HopBudgetExhausted)
        ));
        parent.reunite(child).unwrap();
        assert_eq!(
            (parent.remaining_attempts(), parent.remaining_links()),
            (4, 8)
        );
        let moved = parent.transfer();
        assert_eq!(
            (parent.remaining_attempts(), parent.remaining_links()),
            (0, 0)
        );
        assert_eq!(
            (moved.remaining_attempts(), moved.remaining_links()),
            (4, 8)
        );
    }

    #[test]
    fn peer_attempt_is_atomic() {
        let now = Instant::now();
        let later = now + Duration::from_secs(1);
        let mut budget = AcquisitionBudget::new(later, 1, 2);
        assert_eq!(
            budget.begin_peer_attempt(now, later, 0),
            Err(Error::InvalidRequest)
        );
        assert_eq!(
            budget.begin_peer_attempt(now, later, 3),
            Err(Error::HopBudgetExhausted)
        );
        assert_eq!(
            (budget.remaining_attempts(), budget.remaining_links()),
            (1, 2)
        );
        assert_eq!(budget.begin_peer_attempt(now, later, 2), Ok(later));
        assert_eq!(
            budget.begin_peer_attempt(now, later, 1),
            Err(Error::Unavailable)
        );
    }

    #[test]
    fn inherited_budget_bounds() {
        let now = Instant::now();
        let soon = now + Duration::from_secs(1);
        let later = now + Duration::from_secs(9);
        for (links, visited, ok) in [
            (0, 1, false),
            (9, 1, false),
            (3, 0, false),
            (1, 1, true),
            (8, 1, true),
        ] {
            assert_eq!(
                inherited_budget(later, soon, 2, links, visited).is_ok(),
                ok,
                "{links} {visited}"
            );
        }
        let budget = inherited_budget(later, soon, 2, 5, 1).unwrap();
        assert_eq!(budget.deadline(), soon);
        assert_eq!(budget.remaining_links(), 4);
        assert_eq!(budget.route_links(), 4);
        let budget = inherited_budget(soon, later, 2, 4, 1).unwrap();
        assert_eq!(budget.deadline(), soon);
        assert_eq!(budget.route_links(), 3);
    }
}
