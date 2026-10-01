//! Complete the meeting layer, carrying source first-hop bitsets, not all paths.
//!
//! A complete preceding source layer has already propagated every equal-depth
//! first-hop bit before its children expand. Complete the intersecting layer too:
//! stopping at its first intersection would retain v2's sorted-UID bias. Before
//! that layer the two balls are disjoint, so each intersection has minimum total
//! distance. Store one meeting per first hop (at most 64), not one per full path.
//! Reconstruct one canonical witness per bit. Relays reselect at their own hop.
use super::{MAX_DEGREE, RoutingAlgorithm, neighbor_positions_for, paths::PathKey};
use crate::{
    error::{Error, Result},
    runtime::deadline::Deadline,
};
use std::collections::{BTreeMap, VecDeque};

struct Visit {
    depth: u8,
    parent: usize,
    first: u64,
}

struct Wave {
    visits: BTreeMap<usize, Visit>,
    queue: VecDeque<usize>,
}

impl Wave {
    fn new(root: usize) -> Self {
        Self {
            visits: BTreeMap::from([(
                root,
                Visit {
                    depth: 0,
                    parent: root,
                    first: 0,
                },
            )]),
            queue: VecDeque::from([root]),
        }
    }
}

pub(super) struct EqualCostSearch {
    count: usize,
    algorithm: RoutingAlgorithm,
    key: PathKey,
    neighbors: Vec<usize>,
    waves: [Wave; 2],
    side: usize,
    remaining: usize,
    layers: u8,
    meetings: Vec<Option<usize>>,
    done: bool,
    #[cfg(test)]
    pub expansions: usize,
    #[cfg(test)]
    pub edges: usize,
}

impl EqualCostSearch {
    #[cfg(test)]
    pub(super) fn visited_entries(&self) -> usize {
        self.waves.iter().map(|wave| wave.visits.len()).sum()
    }

    pub(super) fn new(count: usize, key: &PathKey, algorithm: RoutingAlgorithm) -> Self {
        let neighbors = neighbor_positions_for(count, key.from, algorithm);
        assert!(neighbors.len() <= MAX_DEGREE);
        Self {
            count,
            algorithm,
            key: key.clone(),
            meetings: vec![None; neighbors.len()],
            neighbors,
            waves: [Wave::new(key.from), Wave::new(key.to)],
            side: 0,
            remaining: 1,
            layers: 0,
            done: key.from == key.to,
            #[cfg(test)]
            expansions: 0,
            #[cfg(test)]
            edges: 0,
        }
    }

    pub(super) fn step(&mut self, quantum: usize, deadline: Deadline) -> Result<bool> {
        for _ in 0..quantum {
            if crate::runtime::environment::now() >= deadline.0 {
                return Err(Error::DeadlineExceeded);
            }
            if self.done {
                return Ok(true);
            }
            let Some(node) = self.waves[self.side].queue.pop_front() else {
                self.done = true;
                return Ok(true);
            };
            #[cfg(test)]
            {
                self.expansions += 1;
            }
            let depth = self.waves[self.side].visits[&node].depth + 1;
            let first = self.waves[self.side].visits[&node].first;
            for next in neighbor_positions_for(self.count, node, self.algorithm) {
                #[cfg(test)]
                {
                    self.edges += 1;
                }
                if self.key.visited.binary_search(&next).is_ok()
                    || (node == self.key.from && self.key.failed.binary_search(&next).is_ok())
                    || (next == self.key.from && self.key.failed.binary_search(&node).is_ok())
                {
                    continue;
                }
                let bits = if self.side == 0 && node == self.key.from {
                    1u64 << self.neighbors.binary_search(&next).unwrap()
                } else {
                    first
                };
                let wave = &mut self.waves[self.side];
                let visit = wave.visits.entry(next).or_insert_with(|| {
                    wave.queue.push_back(next);
                    Visit {
                        depth,
                        parent: node,
                        first: 0,
                    }
                });
                if visit.depth != depth {
                    continue;
                }
                visit.first |= bits;
                if let Some(opposite) = self.waves[1 - self.side].visits.get(&next) {
                    if depth + opposite.depth != self.layers + 1 {
                        continue;
                    }
                    let mut mask = self.waves[0].visits[&next].first;
                    while mask != 0 {
                        let bit = mask.trailing_zeros() as usize;
                        self.meetings[bit].get_or_insert(next);
                        mask &= mask - 1;
                    }
                }
            }
            self.remaining -= 1;
            if self.remaining == 0 {
                self.layers += 1;
                self.done = self.meetings.iter().any(Option::is_some)
                    || self.layers == self.key.links
                    || self.waves[self.side].queue.is_empty();
                self.side = 1 - self.side;
                self.remaining = self.waves[self.side].queue.len();
            }
        }
        Ok(self.done)
    }

    pub(super) fn finish(self) -> Result<Vec<Vec<usize>>> {
        if self.key.from == self.key.to {
            return Ok(vec![vec![self.key.from]]);
        }
        let mut routes = Vec::new();
        for (bit, meeting) in self.meetings.iter().enumerate() {
            let Some(meeting) = *meeting else {
                continue;
            };
            let mut nodes = vec![meeting];
            let mut current = meeting;
            while current != self.key.from {
                let depth = self.waves[0].visits[&current].depth;
                current = if depth == 1 {
                    self.key.from
                } else {
                    neighbor_positions_for(self.count, current, self.algorithm)
                        .into_iter()
                        .find(|candidate| {
                            self.waves[0].visits.get(candidate).is_some_and(|v| {
                                v.depth + 1 == depth && v.first & (1u64 << bit) != 0
                            })
                        })
                        .ok_or(Error::Internal)?
                };
                nodes.push(current);
            }
            nodes.reverse();
            current = meeting;
            while current != self.key.to {
                current = self.waves[1].visits[&current].parent;
                nodes.push(current);
            }
            routes.push(nodes);
        }
        if routes.is_empty() {
            return Err(Error::Unavailable);
        }
        Ok(routes)
    }
}
