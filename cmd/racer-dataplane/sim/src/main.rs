//! `racer-sim` command line entry point.

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let options = match racer_sim::Options::parse(&args) {
        Ok(options) => options,
        Err(err) => {
            eprintln!("racer-sim: {err}\n{}", racer_sim::USAGE);
            std::process::exit(2);
        }
    };
    let report = racer_sim::run(&options);
    let text = serde_json::to_string_pretty(&report).expect("report serializes");
    match &options.out {
        Some(path) => {
            if let Err(err) = std::fs::write(path, text + "\n") {
                eprintln!("racer-sim: write {path}: {err}");
                std::process::exit(1);
            }
        }
        None => println!("{text}"),
    }
}
