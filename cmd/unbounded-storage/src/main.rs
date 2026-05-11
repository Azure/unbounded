// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::thread;
use std::time::Duration;

fn main() {
    println!("hello world");

    loop {
        thread::sleep(Duration::from_secs(u64::MAX));
    }
}
