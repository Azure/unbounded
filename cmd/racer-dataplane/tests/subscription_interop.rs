//! Go launches this exact test selector and drives the production library over UDS.
#[test]
#[ignore = "run RACER_SUBSCRIPTION_INTEROP=1 go test ./pkg/racersdk -run '^TestRustSubscriptionInterop$' -timeout=5m under external timeout"]
fn go_sdk_subscription_server() {
    racer_dataplane::run_subscription_interop_fixture();
}
