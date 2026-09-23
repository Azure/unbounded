# racer-object

Linux object adapter for explicitly named, permanently immutable model weights:

```text
vLLM / Run:ai -> loopback S3 HTTP -> splice-only frontend
    -> Racer cache Unix socket -> distributed cache
    -> origin Unix socket -> Azure Blob backend (HTTPS)
```

`make racer-object` builds and tests `bin/racer-object`.
`make image-racer-object-local` builds its container image from the root context.
No new Go dependencies are required.

## Object mapping and processes

Share exactly the same JSON configuration between frontends and backends:

```json
{
  "azure_endpoint": "https://ACCOUNT.blob.core.windows.net",
  "objects": [
    {"bucket":"models", "key":"model.safetensors",
     "container":"weights", "blob":"immutable/model.safetensors"}
  ]
}
```

```sh
bin/racer-object backend --config objects.json --socket /dev/racer/weights/origin
bin/racer-object frontend --config objects.json --socket /dev/racer/weights/cache
```

Run one backend on every participating cache node. Run the frontend in the vLLM
pod/network namespace; its default address is `127.0.0.1:8000` and non-loopback
bindings are rejected. The existing P2PCache supplies the sockets' parent
directory. The backend creates mode `0660` and never removes an existing socket
on startup. Configure the directory's shared group and mode `2770` so Racer and
the origin can both access it. A stale socket requires operator investigation.

Each Azure endpoint/container/blob identity is hashed with SHA-256 and a versioned
domain into both the internal Racer target and a canonical representation ETag.
S3 aliases of one backing blob share cache identity. Credentials and signatures
are excluded. **Names must never change bytes, including deletion/recreation.**
Publish changed weights under new blob names. Azure's native ETag separately pins
every range download with `If-Match`; it is never used as the Racer representation ID.
Metadata is coalesced and retained for the backend process lifetime; Racer receives
a 24-hour metadata TTL. A changed Azure ETag fails cold reads rather than silently
mixing versions. Cached bytes still depend on the permanent-immutability contract.

## S3 subset and transport

The frontend supports path-style GET and HEAD, full objects, single bounded,
open-ended and suffix ranges, `If-Match`, `If-None-Match`, and `If-Range`.
It returns S3-shaped XML errors. Listing, writes, multipart ranges, versionId,
partNumber, date preconditions and other S3 APIs are unsupported. Dummy AWS
signatures are accepted without verification: this endpoint is for trusted local
processes, not a public S3 service. No credentials are forwarded to origins.

Object payloads move only through `splice(2)`: Unix socket -> nonblocking kernel
pipe -> TCP socket. HTTP headers are read with an end-delimiter bound that cannot
read or peek at body bytes. Unsupported splice fails the connection; there is no
buffered fallback. This guarantee covers successful object bodies in the frontend,
not Azure HTTPS processing, generated XML errors, or the consuming model loader.
Chunked/encoded upstream bodies, incorrect version/range/length, and short bodies
fail closed. HTTP/1.1 connections are persistent with ordered requests and one
active response per connection. Run:ai can use parallel connections.

`--concurrency` (default 64) bounds frontend connections/pipes or backend sources.
Backend payload memory is at most concurrency times 4 MiB, plus SDK/HTTP overhead;
page-aligned read-ahead avoids issuing one Azure request per 32 KiB origin copy.
Idle buffers are retained for reuse. `--timeout` (default 5m) bounds each request,
including body transfer and backend credential/range operations. Azure requests
and interrupted bodies each have at most three SDK retries. Shutdown closes
blocked sockets; transfer failures are logged and shutdown reports spliced bytes.

## AKS Workload Identity

Default authentication is an explicit `WorkloadIdentityCredential`, with no fallback
to the node identity. Enable the AKS OIDC issuer and workload identity webhook.
Create a user-assigned identity and federated credential with:

- issuer: the cluster's exact OIDC issuer URL;
- subject: `system:serviceaccount:racer-weights:azure-origin`;
- audience: `api://AzureADTokenExchange`.

Grant that identity **Storage Blob Data Reader** on the container or storage
account. Annotate its Kubernetes ServiceAccount with
`azure.workload.identity/client-id`, set the pod label
`azure.workload.identity/use: "true"`, and use that ServiceAccount. The webhook
injects `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_FEDERATED_TOKEN_FILE` and the
projected token. The Azure SDK caches access tokens and reloads projected tokens
as needed. Set `AZURE_AUTHORITY_HOST` for the appropriate authority when required.

See [`deploy/racer-object/example.yaml`](../../deploy/racer-object/example.yaml).
Replace placeholders and align node placement and socket groups with your Racer
deployment before applying. The frontend needs no Azure identity.

Development modes are explicit: `--azure-auth=default` uses DefaultAzureCredential;
`shared-key` reads `AZURE_STORAGE_ACCOUNT`/`AZURE_STORAGE_KEY`; `anonymous` supports
public blobs and local fixtures. Keep secrets out of the mapping file.

## vLLM without discovery

Install the pinned plugin in every vLLM worker image:

```sh
pip install ./cmd/racer-object/vllm
export AWS_ENDPOINT_URL=http://127.0.0.1:8000
export RUNAI_STREAMER_S3_ENDPOINT="$AWS_ENDPOINT_URL"
export AWS_ACCESS_KEY_ID=local AWS_SECRET_ACCESS_KEY=local AWS_DEFAULT_REGION=us-east-1
export AWS_EC2_METADATA_DISABLED=true
vllm serve /models/local-config --load-format racer_object \
  --model-loader-extra-config '{"files":["s3://models/model.safetensors"],"concurrency":16}'
```

The plugin uses vLLM 0.29.0's loader registration and Run:ai 0.16.1, overriding
weight preparation to return only the explicit list. Supply model config and
tokenizer locally; do not point `--model` or `--model-weights` at a remote prefix.
The ordinary `runai_streamer` loader discovers files and is unsuitable here.
If `VLLM_PLUGINS` is restricted, include `racer_object`.

## Verification and performance

```sh
TMPDIR="$PWD/tmp" make racer-object-test
TMPDIR="$PWD/tmp" go test ./cmd/racer-object -run '^$' -bench BenchmarkFrontendSplice -benchmem
```

The tests cover real TCP/Unix sockets, exact range bytes, keep-alive, metadata
coalescing, bounded read-ahead, Azure framing failures and required identity config.
Additional opt-in integration commands and syscall checks are in
[`TESTING.md`](TESTING.md). Benchmark numbers describe their fixture and host,
not an Azure or production-cluster throughput guarantee.
