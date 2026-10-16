//! Process entry point. Configuration and application lifecycle own all resources.

use racer_dataplane::app::Application;
use racer_dataplane::config::Config;

fn main() -> std::process::ExitCode {
    match run() {
        Ok(()) => std::process::ExitCode::SUCCESS,
        Err((stage, error)) => {
            // Workers log their own failing stage first; this names the lifecycle phase.
            eprintln!("racer-dataplane: fatal stage={stage} error={error}");
            std::process::ExitCode::FAILURE
        }
    }
}

fn run() -> std::result::Result<(), (&'static str, racer_dataplane::error::Error)> {
    let config = Config::from_env().map_err(|error| ("config", error))?;
    let application = Application::assemble(config).map_err(|error| ("assemble", error))?;
    application.run().map_err(|error| ("run", error))
}
