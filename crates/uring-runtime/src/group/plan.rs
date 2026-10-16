//! Thread placement plans.

use crate::{Error, ErrorKind, Result};

/// One lane: a pinned thread that owns and drives one lane service.
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq, Hash)]
pub struct Lane {
    /// OS thread name. Linux truncates names to 15 bytes.
    pub name: String,
    /// The logical CPU the thread is pinned to, or `None` to inherit the
    /// affinity of the thread that spawns it.
    pub cpu: Option<usize>,
}

/// A helper: a pinned thread that cooperatively drives one helper service
/// (a "shard") per assigned lane.
///
/// Helpers suit work that a lane hands off but that must not run on the lane
/// thread, such as CPU-heavy computation. A helper's shard for a lane is
/// started before that lane's service is built and is fenced only after that
/// lane's first ownership fence, so a lane may hold references into its
/// helper shard for its entire lifetime.
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq, Hash)]
pub struct Helper {
    /// OS thread name. Linux truncates names to 15 bytes.
    pub name: String,
    /// The logical CPU the thread is pinned to, or `None` to inherit.
    pub cpu: Option<usize>,
    /// Indices into [`Plan::lanes`] served by this helper.
    pub lanes: Vec<usize>,
}

/// A validated description of the threads a [`Group`](super::Group) runs.
///
/// Built with [`Plan::builder`]. Lane 0 is the *coordinator*: under
/// [`Group::start`](super::Group::start) it is the thread that spawns and
/// joins every other thread, and under [`Group::run`](super::Group::run) it is
/// the calling thread itself.
///
/// ```
/// use uring_runtime::group::Plan;
///
/// let plan = Plan::builder()
///     .lane("lane-0", 0)
///     .lane("lane-1", 1)
///     .helper("helper-0", 2, [0, 1])
///     .max_threads(4)
///     .build()
///     .unwrap();
/// assert_eq!(plan.threads(), 3);
/// assert_eq!(plan.helper_of(1), Some(0));
/// ```
#[derive(Clone, Debug, Eq, PartialEq, Hash)]
pub struct Plan {
    lanes: Vec<Lane>,
    helpers: Vec<Helper>,
    max_threads: usize,
}

impl Plan {
    /// Start building a plan.
    pub fn builder() -> PlanBuilder {
        PlanBuilder::default()
    }

    /// The lanes, in index order. Never empty.
    pub fn lanes(&self) -> &[Lane] {
        &self.lanes
    }

    /// The helpers, in index order.
    pub fn helpers(&self) -> &[Helper] {
        &self.helpers
    }

    /// The thread budget, including the caller of
    /// [`Group::start`](super::Group::start).
    pub fn max_threads(&self) -> usize {
        self.max_threads
    }

    /// The number of lane and helper threads (`lanes + helpers`).
    pub fn threads(&self) -> usize {
        self.lanes.len() + self.helpers.len()
    }

    /// The index of the helper serving `lane`, if any.
    pub fn helper_of(&self, lane: usize) -> Option<usize> {
        self.helpers
            .iter()
            .position(|helper| helper.lanes.contains(&lane))
    }
}

/// Builder for [`Plan`].
#[derive(Clone, Debug, Default)]
pub struct PlanBuilder {
    lanes: Vec<Lane>,
    helpers: Vec<Helper>,
    max_threads: Option<usize>,
}

impl PlanBuilder {
    /// Add a lane. `cpu` is a CPU number or `None` for an unpinned lane.
    #[must_use]
    pub fn lane(mut self, name: impl Into<String>, cpu: impl Into<Option<usize>>) -> Self {
        self.lanes.push(Lane {
            name: name.into(),
            cpu: cpu.into(),
        });
        self
    }

    /// Add a helper serving the given lane indices.
    #[must_use]
    pub fn helper(
        mut self,
        name: impl Into<String>,
        cpu: impl Into<Option<usize>>,
        lanes: impl IntoIterator<Item = usize>,
    ) -> Self {
        self.helpers.push(Helper {
            name: name.into(),
            cpu: cpu.into(),
            lanes: lanes.into_iter().collect(),
        });
        self
    }

    /// Limit the total number of threads. [`Group::run`](super::Group::run)
    /// needs `lanes + helpers` threads (the caller is lane 0);
    /// [`Group::start`](super::Group::start) needs one more for the caller.
    /// Unlimited by default.
    #[must_use]
    pub fn max_threads(mut self, max_threads: usize) -> Self {
        self.max_threads = Some(max_threads);
        self
    }

    /// Validate and build the plan.
    ///
    /// # Errors
    ///
    /// [`ErrorKind::InvalidConfiguration`] if there are no lanes, a thread name
    /// contains a NUL byte, a helper serves no lanes, a helper refers to a lane
    /// that does not exist, a lane is served by more than one helper, or
    /// `lanes + helpers` exceeds the thread budget.
    pub fn build(self) -> Result<Plan> {
        let invalid =
            |context| Err(Error::new(ErrorKind::InvalidConfiguration).with_context(context));
        if self.lanes.is_empty() {
            return invalid("plan has no lanes");
        }
        let names = self.lanes.iter().map(|lane| &lane.name);
        if names
            .chain(self.helpers.iter().map(|helper| &helper.name))
            .any(|name| name.contains('\0'))
        {
            return invalid("thread name contains NUL");
        }
        let mut served = vec![false; self.lanes.len()];
        for helper in &self.helpers {
            if helper.lanes.is_empty() {
                return invalid("helper serves no lanes");
            }
            for &lane in &helper.lanes {
                match served.get_mut(lane) {
                    None => return invalid("helper lane index out of range"),
                    Some(true) => return invalid("lane served by more than one helper"),
                    Some(slot) => *slot = true,
                }
            }
        }
        let max_threads = self.max_threads.unwrap_or(usize::MAX);
        if self.lanes.len() + self.helpers.len() > max_threads {
            return invalid("plan exceeds max_threads");
        }
        Ok(Plan {
            lanes: self.lanes,
            helpers: self.helpers,
            max_threads,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn context(builder: PlanBuilder) -> &'static str {
        let error = builder.build().unwrap_err();
        assert_eq!(error.kind(), ErrorKind::InvalidConfiguration);
        error.context().unwrap()
    }

    #[test]
    fn valid_plans() {
        let plan = Plan::builder().lane("a", None).build().unwrap();
        assert_eq!(plan.lanes()[0].cpu, None);
        assert_eq!(plan.max_threads(), usize::MAX);
        assert_eq!(plan.helper_of(0), None);
        let plan = Plan::builder()
            .lane("a", 0)
            .lane("b", 0)
            .helper("h", 0, [1, 0])
            .max_threads(3)
            .build()
            .unwrap();
        assert_eq!(plan.threads(), 3);
        assert_eq!(plan.helpers()[0].lanes, [1, 0]);
        assert_eq!(plan.helper_of(0), Some(0));
    }

    #[test]
    fn invalid_plans_report_context() {
        assert_eq!(context(Plan::builder()), "plan has no lanes");
        assert_eq!(
            context(Plan::builder().lane("bad\0", 0)),
            "thread name contains NUL"
        );
        assert_eq!(
            context(Plan::builder().lane("a", 0).helper("bad\0", 0, [0])),
            "thread name contains NUL"
        );
        assert_eq!(
            context(Plan::builder().lane("a", 0).helper("h", 0, [])),
            "helper serves no lanes"
        );
        assert_eq!(
            context(Plan::builder().lane("a", 0).helper("h", 0, [1])),
            "helper lane index out of range"
        );
        assert_eq!(
            context(Plan::builder().lane("a", 0).helper("h", 0, [0, 0])),
            "lane served by more than one helper"
        );
        assert_eq!(
            context(
                Plan::builder()
                    .lane("a", 0)
                    .helper("h", 0, [0])
                    .helper("i", 0, [0])
            ),
            "lane served by more than one helper"
        );
        assert_eq!(
            context(Plan::builder().lane("a", 0).lane("b", 1).max_threads(1)),
            "plan exceeds max_threads"
        );
    }
}
