//! Process entry point. Configuration and application lifecycle own all resources.

use racer_dataplane::{app::Application, config::Config};

fn main() -> std::process::ExitCode {
    match run() {
        Ok(()) => std::process::ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("racer-dataplane: {error}");
            std::process::ExitCode::FAILURE
        }
    }
}

fn run() -> std::result::Result<(), Box<dyn std::error::Error>> {
    let config = Config::from_env()?;
    Application::assemble(config)?.run()?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn executable_builder_defaults_to_no_associations_and_rejects_invalid_input() {
        for value in [None, Some("[]"), Some(""), Some("not-json")] {
            let result = Config::from_lookup(|name| {
                Ok(match name {
                    "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                    "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                    "RACER_FABRIC_PORTS" => value.map(str::to_owned),
                    _ => None,
                })
            })
            .and_then(Application::assemble);
            match value {
                None => {
                    result.unwrap();
                }
                _ => assert!(matches!(
                    result,
                    Err(racer_dataplane::error::Error::InvalidConfiguration)
                )),
            }
        }
    }

    #[test]
    fn executable_builder_rejects_obsolete_associations_without_hardware() {
        for projected in [false, true] {
            let result = Config::from_lookup(|name| {
                Ok(match name {
                    "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                    "RACER_CONTROL_ENDPOINT" => Some("https://control.example:7443".into()),
                    "RACER_ENABLE_RDMA" => Some("true".into()),
                    "RACER_FABRIC_PORTS" if !projected => Some(r#"[{"fabric":"production-a","device":"mlx5_0","port":1,"gid":"fe800000000000000000000000001234"}]"#.into()),
                    "RACER_FABRIC_PORTS_FILE" if projected => Some("/etc/racer/native/ports.json".into()),
                    _ => None,
                })
            });
            assert!(result.is_err());
        }
    }
}
