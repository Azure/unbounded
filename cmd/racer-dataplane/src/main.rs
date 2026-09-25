//! Process entry point. Configuration and application lifecycle own all resources.

use racer_dataplane::{app::Application, config::Config, error::Result, rdma::device::FabricPort};

fn main() -> std::process::ExitCode {
    match run() {
        Ok(()) => std::process::ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("racer-dataplane: {error}");
            std::process::ExitCode::FAILURE
        }
    }
}

fn run() -> Result<()> {
    let (config, fabric_ports) = Config::from_env_with_fabric_ports()?;
    assemble(config, fabric_ports)?.run()
}

fn assemble(config: Config, fabric_ports: Vec<FabricPort>) -> Result<Application> {
    Application::assemble(config)?.with_fabric_ports(fabric_ports)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn executable_builder_defaults_to_no_associations_and_rejects_invalid_input() {
        for value in [None, Some("[]"), Some(""), Some("not-json")] {
            let result = Config::from_lookup_with_fabric_loader(
                |name| {
                    Ok(match name {
                        "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                        "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                        "RACER_FABRIC_PORTS" => value.map(str::to_owned),
                        _ => None,
                    })
                },
                |_| panic!("no projected source selected"),
            )
            .and_then(|(config, ports)| assemble(config, ports));
            match value {
                None | Some("[]") => assert!(result.unwrap().fabric_ports().is_empty()),
                _ => assert!(matches!(
                    result,
                    Err(racer_dataplane::error::Error::InvalidConfiguration)
                )),
            }
        }
    }

    #[test]
    fn executable_builder_receives_trusted_associations_without_hardware() {
        for projected in [false, true] {
            let (config, ports) = Config::from_lookup_with_fabric_loader(
                |name| Ok(match name {
                    "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                    "RACER_CONTROL_ENDPOINT" => Some("https://control.example:7443".into()),
                    "RACER_ENABLE_RDMA" => Some("true".into()),
                    "RACER_FABRIC_PORTS" if !projected => Some(r#"[{"fabric":"production-a","device":"mlx5_0","port":1,"gid":"fe800000000000000000000000001234"}]"#.into()),
                    "RACER_FABRIC_PORTS_FILE" if projected => Some("/etc/racer/native/ports.json".into()),
                    _ => None,
                }),
                |path| {
                    assert_eq!(path, std::path::Path::new("/etc/racer/native/ports.json"));
                    Ok(vec![FabricPort {
                        fabric: "production-a".into(), device: "mlx5_0".into(), port: 1,
                        gid: Some("fe80::1234".parse::<std::net::Ipv6Addr>().unwrap().octets()),
                    }])
                },
            ).unwrap();
            let app = assemble(config, ports).unwrap();
            assert_eq!(app.fabric_ports().len(), 1);
            let port = &app.fabric_ports()[0];
            assert_eq!(port.fabric, "production-a");
            assert_eq!(port.device, "mlx5_0");
            assert_eq!(port.port, 1);
            assert_eq!(
                port.gid,
                Some("fe80::1234".parse::<std::net::Ipv6Addr>().unwrap().octets())
            );
        }
    }
}
