#!/usr/bin/env python3

import base64
import json
import os
import shutil
import struct
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

CONFIG_PATH = "/etc/unbounded-metal/config"
CA_CERT_PATH = "/etc/kubernetes/pki/ca.crt"
KUBELET_KUBECONFIG_PATH = "/etc/kubernetes/kubelet.conf"


def read_config():
    config = {}
    with open(CONFIG_PATH) as f:
        for line in f:
            line = line.strip()
            if "=" in line:
                key, value = line.split("=", 1)
                config[key] = value
    return config


config = read_config()
SERVE_URL = config["SERVE_URL"]
NODE_NAME = config["NODE_NAME"]


def log(msg):
    print(f"unbounded-metal-attest: {msg}", file=sys.stderr, flush=True)


def main():
    # When called as ExecStartPre (not as an exec credential plugin),
    # skip attestation if kubelet already has a kubeconfig.
    is_exec_plugin = "KUBERNETES_EXEC_INFO" in os.environ
    if not is_exec_plugin and os.path.exists(KUBELET_KUBECONFIG_PATH):
        log("kubelet.conf already exists, skipping attestation")
        return

    log(f"starting (SERVE_URL={SERVE_URL}, NODE_NAME={NODE_NAME})")

    token, cluster_ca, expires_at = tpm_attest()

    if cluster_ca and not os.path.exists(CA_CERT_PATH):
        os.makedirs(os.path.dirname(CA_CERT_PATH), exist_ok=True)
        with open(CA_CERT_PATH, "wb") as f:
            f.write(cluster_ca.encode() if isinstance(cluster_ca, str) else cluster_ca)

    emit_exec_credential(token, expires_at)


def emit_exec_credential(token, expires_at):
    json.dump({
        "apiVersion": "client.authentication.k8s.io/v1",
        "kind": "ExecCredential",
        "status": {
            "token": token,
            "expirationTimestamp": time.strftime(
                "%Y-%m-%dT%H:%M:%SZ", time.gmtime(expires_at),
            ),
        },
    }, sys.stdout)


def tpm_attest():
    tmpdir = tempfile.mkdtemp(prefix="unbounded-metal-attest-")
    try:
        return _do_attest(tmpdir)
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


def request_with_retry(req, retries=5):
    """Execute an HTTP request with exponential backoff on transient errors."""
    for attempt in range(retries):
        try:
            with urllib.request.urlopen(req) as resp:
                return resp.read()
        except urllib.error.HTTPError:
            raise  # HTTP errors are not transient; let the caller handle them
        except urllib.error.URLError as e:
            if attempt == retries - 1:
                raise
            delay = min(2 ** attempt, 30)
            log(f"connection error to {req.full_url}: {e.reason} (retry in {delay}s)")
            time.sleep(delay)
            # Recreate request — data stream may have been consumed.
            req = urllib.request.Request(
                req.full_url, data=req.data, headers=dict(req.headers),
                method=req.get_method(),
            )
    raise RuntimeError("unreachable")


def _do_attest(tmpdir):
    ek_ctx = os.path.join(tmpdir, "ek.ctx")
    ek_pub = os.path.join(tmpdir, "ek.pub")
    srk_ctx = os.path.join(tmpdir, "srk.ctx")
    srk_pub = os.path.join(tmpdir, "srk.pub")
    cred_file = os.path.join(tmpdir, "cred.blob")
    decrypted_file = os.path.join(tmpdir, "decrypted.bin")
    session_ctx = os.path.join(tmpdir, "session.ctx")

    # ── Flush stale transient handles and sessions ──
    # Physical TPMs have limited transient object slots (typically 3–7)
    # and session slots (typically 3). Firmware, prior boot attempts, or
    # other TPM consumers may have left handles allocated; flush them to
    # avoid TPM_RC_OBJECT_MEMORY / TPM_RC_SESSION_MEMORY errors.
    log("flushing stale TPM transient handles and sessions...")
    for flag in ["-t", "-s"]:
        try:
            run(["tpm2_flushcontext", flag])
        except subprocess.CalledProcessError:
            pass  # OK — nothing to flush

    # ── Create EK and SRK ──
    log("creating EK...")
    run(["tpm2_createek", "-c", ek_ctx, "-G", "rsa", "-u", ek_pub])
    log(f"EK created, ek.pub size={os.path.getsize(ek_pub)}")

    log("creating default SRK...")
    run(["tpm2_createprimary", "-C", "o", "-c", srk_ctx])
    run(["tpm2_readpublic", "-c", srk_ctx, "-o", srk_pub])
    log("SRK created")

    # ── Attest: send EK + SRK, receive MakeCredential challenge + encrypted token ──
    log(f"attesting with {SERVE_URL}/attest ...")
    resp = post_json(f"{SERVE_URL}/attest", {
        "ekPub": b64_file(ek_pub),
        "srkPub": b64_file(srk_pub),
    })

    credential_blob = base64.b64decode(resp["credentialBlob"])
    encrypted_secret = base64.b64decode(resp["encryptedSecret"])

    # ── ActivateCredential to recover the AES key ──
    # tpm2_activatecredential -i expects the tpm2-tools file format:
    # 4-byte magic (0xBADCC0DE) + 4-byte version (1) followed by
    # credentialBlob and encryptedSecret (both TPM2B-encoded).
    with open(cred_file, "wb") as f:
        f.write(struct.pack(">II", 0xBADCC0DE, 1))
        f.write(credential_blob + encrypted_secret)

    # The EK requires PolicySecret(endorsement) for use. Start a policy
    # session satisfying that policy, then pass it as the credentialed-key
    # auth so tpm2_activatecredential can decrypt.
    log("activating credential...")
    run(["tpm2_startauthsession", "--policy-session", "-S", session_ctx])
    try:
        run(["tpm2_policysecret", "-S", session_ctx, "-c", "endorsement"])
        run(["tpm2_activatecredential",
             "-c", srk_ctx,
             "-C", ek_ctx,
             "-i", cred_file,
             "-o", decrypted_file,
             "--credentialkey-auth", f"session:{session_ctx}"])
    finally:
        try:
            run(["tpm2_flushcontext", session_ctx])
        except subprocess.CalledProcessError:
            pass
    log("credential activated")

    # ── Decrypt the bootstrap token with the recovered AES key ──
    with open(decrypted_file, "rb") as f:
        aes_key = f.read()

    gcm_nonce = base64.b64decode(resp["gcmNonce"])
    encrypted_token = base64.b64decode(resp["encryptedToken"])
    token = AESGCM(aes_key).decrypt(gcm_nonce, encrypted_token, None).decode()

    cluster_ca = resp.get("clusterCA", "")
    # Server issues 1-hour tokens.
    expires_at = int(time.time()) + 3600

    log("attestation complete")
    return token, cluster_ca, expires_at


def run(cmd):
    result = subprocess.run(cmd, capture_output=True)
    if result.returncode != 0:
        log(f"command failed: {' '.join(cmd)}")
        log(f"  exit code: {result.returncode}")
        if result.stdout:
            log(f"  stdout: {result.stdout.decode(errors='replace')}")
        if result.stderr:
            log(f"  stderr: {result.stderr.decode(errors='replace')}")
        raise subprocess.CalledProcessError(result.returncode, cmd)


def post_json(url, data):
    body = json.dumps(data).encode()
    req = urllib.request.Request(
        url, data=body, headers={"Content-Type": "application/json"},
    )
    try:
        return json.loads(request_with_retry(req))
    except urllib.error.HTTPError as e:
        error_body = e.read().decode(errors="replace")
        log(f"HTTP {e.code} from {url}: {error_body}")
        raise


def b64_file(path):
    with open(path, "rb") as f:
        return base64.b64encode(f.read()).decode()



if __name__ == "__main__":
    main()
