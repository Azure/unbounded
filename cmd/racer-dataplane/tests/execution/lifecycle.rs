// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lifecycle_settings_have_bounded_validated_defaults() {
        let defaults = Config::from_lookup(|_| Err(std::env::VarError::NotPresent)).unwrap();
        assert_eq!(
            (
                defaults.startup.as_secs(),
                defaults.stall.as_secs(),
                defaults.drain.as_secs(),
                defaults.quiesce.as_secs()
            ),
            (90, 5, 20, 5)
        );
        for name in [
            "RACER_STARTUP_SECONDS",
            "RACER_STALL_SECONDS",
            "RACER_DRAIN_SECONDS",
            "RACER_QUIESCE_SECONDS",
        ] {
            for value in ["", "0", "3601", "-1", "abc", "1.5"] {
                assert!(
                    Config::from_lookup(|key| if key == name {
                        Ok(value.into())
                    } else {
                        Err(std::env::VarError::NotPresent)
                    })
                    .is_err()
                );
            }
            for value in ["1", "3600"] {
                assert!(
                    Config::from_lookup(|key| if key == name {
                        Ok(value.into())
                    } else {
                        Err(std::env::VarError::NotPresent)
                    })
                    .is_ok()
                );
            }
        }
    }

    #[test]
    fn startup_freshness_idle_stall_and_absolute_shutdown_deadlines() {
        let life = Lifecycle::new(Config::default());
        let t = life.began;
        life.configure_workers(2);
        assert!(!life.fresh_at(t));
        life.progress_at(0, t);
        assert!(!life.fresh_at(t));
        life.progress_at(1, t);
        assert!(life.fresh_at(t));
        assert_eq!(life.deadlines(t), (false, false));
        // Zero traffic is healthy when completion-loop heartbeats continue.
        for n in 1..=100 {
            let now = t + Duration::from_secs(n);
            life.progress_at(0, now);
            life.progress_at(1, now);
            assert_eq!(life.deadlines(now), (false, false));
        }
        let stalled = t + Duration::from_secs(105);
        life.progress_at(0, stalled);
        assert!(!life.fresh_at(stalled));
        assert_eq!(life.deadlines(stalled), (false, false));
        assert!(life.draining());
        life.begin_at(stalled + Duration::from_secs(19));
        assert_eq!(
            life.deadlines(stalled + Duration::from_secs(20)),
            (true, false)
        );
        assert_eq!(
            life.deadlines(stalled + Duration::from_secs(25)),
            (true, true)
        );

        let life = Lifecycle::new(Config::default());
        assert_eq!(
            life.deadlines(life.began + Duration::from_secs(90)),
            (true, false)
        );
        assert_eq!(
            life.deadlines(life.began + Duration::from_secs(95)),
            (true, true)
        );
    }

    #[test]
    fn exporter_reports_startup_stall_and_draining_despite_responding_to_metrics() {
        use std::{
            io::{Read, Write},
            net::TcpStream,
        };
        let life = Arc::new(Lifecycle::new(Config {
            stall: Duration::from_millis(250),
            ..Default::default()
        }));
        life.configure_workers(1);
        let updates = Arc::new(crate::control::Updates::default());
        updates.subscribe(Arc::new(crate::uring::Wake::new().unwrap()));
        let (trust, config) = crate::control::tests::fixture();
        updates
            .publish(crate::control::tests::prepare_snapshot(&trust, config))
            .unwrap();
        updates.staged(1, 0, true);
        updates.activated(1, 0);
        let registry =
            Arc::new(crate::metrics::Registry::new(1, updates).with_lifecycle(life.clone()));
        let exporter =
            crate::metrics::Exporter::start("127.0.0.1:0".parse().unwrap(), registry).unwrap();
        let get = |path| {
            let mut s = TcpStream::connect(exporter.address()).unwrap();
            s.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
            write!(s, "GET {path} HTTP/1.1\r\nHost: localhost\r\n\r\n").unwrap();
            let mut text = String::new();
            s.read_to_string(&mut text).unwrap();
            text
        };
        assert!(get("/startupz").starts_with("HTTP/1.1 503"));
        assert!(get("/readyz").starts_with("HTTP/1.1 503")); // activation alone is insufficient
        life.progress(0);
        assert!(get("/livez").starts_with("HTTP/1.1 200"));
        assert!(get("/readyz").starts_with("HTTP/1.1 200"));
        thread::sleep(Duration::from_millis(260));
        assert!(get("/livez").starts_with("HTTP/1.1 503"));
        assert!(get("/readyz").starts_with("HTTP/1.1 503"));
        assert!(get("/metrics").starts_with("HTTP/1.1 200"));
        life.progress(0);
        assert!(get("/readyz").starts_with("HTTP/1.1 200"));
        life.begin_shutdown();
        assert!(get("/livez").starts_with("HTTP/1.1 503"));
        assert!(get("/status").contains("\"draining\":true"));
        assert!(get("/metrics").starts_with("HTTP/1.1 200"));
    }
}
