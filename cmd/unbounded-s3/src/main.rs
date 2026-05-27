// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::net::SocketAddr;
use std::path::PathBuf;
use std::process::ExitCode;
use std::sync::Arc;

use tracing_subscriber::EnvFilter;

const DEFAULT_LISTEN: &str = "127.0.0.1:8080";

fn main() -> ExitCode {
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info")),
        )
        .init();

    let cli = match Cli::parse(std::env::args().skip(1)) {
        Ok(CliAction::Run(c)) => c,
        Ok(CliAction::Help) => {
            print_help();
            return ExitCode::SUCCESS;
        }
        Err(e) => {
            eprintln!("{e}");
            eprintln!();
            print_help();
            return ExitCode::FAILURE;
        }
    };

    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("build tokio multi-thread runtime");

    let code = rt.block_on(async move {
        let catalog_path = cli.catalog.as_ref().map(|p| p.as_path());

        // The placeholder BlockStore: every read_page reports a miss,
        // so the daemon will not serve any object content until
        // upstream publishes a P2P-aware BlockStore. The wiring is in
        // place so swapping the implementor is a one-line change.
        let store = unbounded_storage::bufferpool::NullBlockStore::new();
        let source: Arc<dyn unbounded_s3::object::ObjectSource> =
            match unbounded_s3::BlockStoreObjectSource::new(store) {
                Ok(s) => Arc::new(s),
                Err(e) => {
                    eprintln!("storage backend init failed: {e}");
                    return ExitCode::FAILURE;
                }
            };
        tracing::warn!(
            "storage backend up: NullBlockStore - all reads will return miss \
             until upstream publishes a P2P-aware BlockStore"
        );

        if let Err(e) = unbounded_s3::server::run_server(cli.listen, catalog_path, source).await {
            eprintln!("server error: {e}");
            ExitCode::FAILURE
        } else {
            ExitCode::SUCCESS
        }
    });

    code
}

struct Cli {
    listen: SocketAddr,
    catalog: Option<PathBuf>,
}

enum CliAction {
    Run(Cli),
    Help,
}

impl Cli {
    fn parse<I: IntoIterator<Item = String>>(args: I) -> Result<CliAction, String> {
        let mut listen = DEFAULT_LISTEN.parse::<SocketAddr>().unwrap();
        let mut catalog: Option<PathBuf> = None;
        let mut it = args.into_iter();
        while let Some(arg) = it.next() {
            match arg.as_str() {
                "-h" | "--help" => return Ok(CliAction::Help),
                s if s.starts_with("--listen=") => {
                    let v = &s["--listen=".len()..];
                    listen = v
                        .parse()
                        .map_err(|e| format!("invalid --listen address {v:?}: {e}"))?;
                }
                "--listen" => {
                    let v = it.next().ok_or_else(|| "--listen requires an address")?;
                    listen = v
                        .parse()
                        .map_err(|e| format!("invalid --listen address {v:?}: {e}"))?;
                }
                s if s.starts_with("--catalog=") => {
                    let v = &s["--catalog=".len()..];
                    catalog = Some(PathBuf::from(v));
                }
                "--catalog" => {
                    let v = it.next().ok_or_else(|| "--catalog requires a path")?;
                    catalog = Some(PathBuf::from(v));
                }
                other => return Err(format!("unknown argument: {other}")),
            }
        }
        Ok(CliAction::Run(Cli { listen, catalog }))
    }
}

fn print_help() {
    eprintln!("Usage: unbounded-s3 [OPTIONS]");
    eprintln!();
    eprintln!("Options:");
    eprintln!("  --listen <IP:PORT>              Bind address. Default: {DEFAULT_LISTEN}");
    eprintln!("  --catalog <PATH>                YAML catalog file. Default: empty catalog.");
    eprintln!("  -h, --help                      Print this help and exit.");
}
