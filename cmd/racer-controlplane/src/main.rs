// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use racer_controlplane::service::{self, Options};
use tokio_util::sync::CancellationToken;

#[tokio::main]
async fn main() -> Result<()> {
    let options = Options::parse(std::env::args().skip(1))?;
    if options.version {
        println!(
            "{} built {}",
            service::version(),
            option_env!("BUILD_TIME").unwrap_or("unknown")
        );
        return Ok(());
    }
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();
    let client = kube::Client::try_default().await?;
    if !options.bootstrap_node.is_empty() {
        let output = tokio::time::timeout(
            std::time::Duration::from_secs(30),
            service::bootstrap(client, &options, &std::env::var("POD_IP")?),
        )
        .await??;
        print!("{output}");
        return Ok(());
    }
    let shutdown = CancellationToken::new();
    let signal = shutdown.clone();
    tokio::spawn(async move {
        let mut terminate =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
                .expect("SIGTERM handler");
        tokio::select! { _ = tokio::signal::ctrl_c() => (), _ = terminate.recv() => () }
        signal.cancel();
    });
    service::run(client, options, shutdown).await
}
