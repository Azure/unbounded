// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

fn main() -> std::io::Result<()> {
    racer_dataplane::dev_bench::run(racer_dataplane::dev_bench::Kind::TcpPage)
}
