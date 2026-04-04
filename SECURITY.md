# Security Policy

## Supported Versions

Security patches are applied to the latest release. Older releases are not supported.

| Version | Supported |
| ------- | --------- |
| Latest  | Yes       |
| Older   | No        |

## Reporting a Vulnerability

**Do not report security vulnerabilities through public GitHub issues.**

Please report security vulnerabilities using [GitHub Security Advisories](https://github.com/project-unbounded/unbounded-kube/security/advisories/new).
This keeps the initial disclosure private and gives maintainers time to investigate and prepare a fix before public disclosure.

Include as much of the following information as possible:

- The type of issue (e.g. remote code execution, privilege escalation, credential exposure)
- Full paths to the relevant source file(s)
- Steps to reproduce or proof-of-concept code
- Impact assessment: which component is affected and what an attacker could achieve

## Response Timeline

We aim to:

- Acknowledge the report within **3 business days**
- Provide an initial assessment within **7 business days**
- Release a patch within **30 days** for confirmed critical issues (timeline may vary for complex issues)

You will be credited in the release notes unless you prefer otherwise.

## Scope

The following are considered in-scope security issues:

- Vulnerabilities in `unbounded-agent`, `machina`, `metalman`, or `kubectl-unbounded`
- Weaknesses in TPM attestation, certificate pinning, or SSH fingerprint verification
- Privilege escalation via Kubernetes RBAC misconfigurations in the provided manifests
- Credential or secret exposure in code, manifests, or container images

The following are out of scope:

- Vulnerabilities in third-party dependencies (please report these upstream)
- Issues requiring physical access to hardware
- Denial-of-service attacks against the control plane

## Disclosure Policy

We follow coordinated disclosure. Please allow us to release a patch before publicly disclosing any vulnerability.
