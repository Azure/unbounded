//! Process entry point. Configuration and application lifecycle own all resources.

use racer_dataplane::{app::Application, config::Config, error::Result};

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
    let config = Config::from_env()?;
    Application::assemble(config)?.run()
}
