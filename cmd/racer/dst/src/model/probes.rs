//! Silence pings, failed probes and probe replies.

use super::*;

impl<'s> Model<'s> {
    /// Send `from`'s due silence pings, at most [`MAX_PINGS_IN_FLIGHT`] at once (production
    /// `CandidatePolicy::poll_pings` in `src/read/candidates.rs`). A ping
    /// is a peer request (`FetchMode::Ping`) routed over the mesh like any other, on a fresh
    /// grant: no remote attempts, [`NOTICE_LINKS`] links and the peer attempt timeout
    /// (production `CandidatePolicy::ping` through `request_traced`).
    pub(super) fn poll_pings(&mut self, from: u32) {
        let now = self.now();
        let at = self.instant(now);
        let n = &mut self.nodes[from as usize];
        let room = MAX_PINGS_IN_FLIGHT.saturating_sub(n.pings);
        if room == 0 {
            return;
        }
        for (to, page) in n.silence.due_pings(at, room) {
            self.nodes[from as usize].pings += 1;
            self.report.counters.silence_pings += 1;
            self.next_msg += 1;
            let mid = self.next_msg;
            self.pings.insert(mid, (from, to));
            let timeout = now + ATTEMPT_TIMEOUT;
            let version = self.versions.borrow().adopted(from).number;
            let msg = Msg {
                from,
                worker: 0,
                read: ReadId(0),
                id: EffectId::default(),
                dest: to,
                page,
                route: Route {
                    version,
                    copy_only: true,
                    deadline: timeout,
                    attempts: 0,
                    links: NOTICE_LINKS,
                    charged: NOTICE_LINKS,
                },
                path: vec![from],
                at: from,
                abandon: timeout,
                ping: Some(mid),
            };
            self.msgs.insert(mid, msg);
            self.timeouts_queued += 1;
            self.engine.at(timeout, Ev::PingTimeout { ping: mid });
            self.forward(mid, from, None);
        }
    }

    /// Ping `ping` ends at `at` with the verdict on `answer` (`ping_verdict`).
    pub(super) fn end_ping(&mut self, ping: u64, at: Nanos, answer: Result<(bool, bool), bool>) {
        let answered = racer_core::liveness::ping_verdict(answer);
        self.engine.at(at, Ev::PingEnd { ping, answered });
    }

    /// `from`'s ping to `to` ended (production `LinkHealth::ping_outcome`).
    pub(super) fn ping_done(&mut self, from: u32, to: u32, answered: Option<bool>) {
        let at = self.instant(self.now());
        let n = &mut self.nodes[from as usize];
        n.pings -= 1;
        if answered.is_some() {
            n.silence.ping_result(&to, answered, at);
            if n.silence.silent(&to, at) {
                // Ask again after the hold to learn that it recovered.
                let hold = n.silence.hold().as_nanos() as Nanos;
                self.engine.after(hold, Ev::PollPings { from });
            }
        }
        self.poll_pings(from);
    }

    /// The serving read on `to` failed without a peer response: a transport failure.
    pub(super) fn fail_probe(&mut self, asker: &Asker, to: u32) {
        self.release_relays(asker.msg);
        let now = self.now();
        self.answering(asker, to, now);
        let route = Self::back(asker, to);
        if !self.alive(&route) {
            return;
        }
        let at = self.message_back(now, &route);
        if let Some(ping) = asker.ping {
            // A transport failure proves nothing.
            return self.end_ping(ping, at, Err(false));
        }
        self.engine.at(
            at,
            Ev::Input {
                node: asker.from,
                worker: asker.worker,
                read: asker.read,
                input: Input::Replied {
                    id: asker.id,
                    reply: Reply::Failed {
                        error: Error::Unavailable,
                        sent: true,
                        scope_live: true,
                    },
                    answer: None,
                },
            },
        );
    }

    pub(super) fn reply(&mut self, asker: &Asker, to: u32, kind: ResponseKind) {
        let own = self.answering(asker, to, self.now());
        let route = Self::back(asker, to);
        if !self.alive(&route) {
            self.release_relays(asker.msg);
            return;
        }
        let ready = self.sign(to, asker.msg);
        let at = self.message_back(ready, &route);
        self.answered_msgs.insert(asker.msg);
        // Production keeps each relay's `Relay` reservation until it has written the answer
        // upstream (`peer/server.rs` `send_http_response` clears `relay.reservation`).
        self.release_relays_at(asker.msg, at);
        if let Some(ping) = asker.ping {
            // A signed answer: alive when the peer itself answered, silent when a relay
            // answered `Unavailable` (production `CandidatePolicy::ping`).
            return self.end_ping(ping, at, Ok((own, kind == ResponseKind::Unavailable)));
        }
        if kind == ResponseKind::StaleMembership {
            // Production `request_traced`: the answering node could not resolve the request's
            // membership, so the remote credits it carried were never used, and neither were the
            // links past the hops that carried it there.
            let refund = Ev::Refund {
                node: asker.from,
                worker: asker.worker,
                read: asker.read,
                remote: asker.attempts,
                links: stale_unused_links(asker.links, asker.path.len()),
            };
            self.engine.at(at, refund);
        }
        self.engine.at(
            at,
            Ev::Input {
                node: asker.from,
                worker: asker.worker,
                read: asker.read,
                input: Input::Replied {
                    id: asker.id,
                    // A relay's answer is not the destination's signed answer.
                    reply: Reply::Answer { kind, signed: own },
                    answer: None,
                },
            },
        );
    }
}
