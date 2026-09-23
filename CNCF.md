# CNCF Sandbox Readiness

Unbounded Kubernetes is preparing a proposed CNCF Sandbox application. Public
coordination and the evidence-backed application worksheet are maintained in
[GitHub issue #555](https://github.com/Azure/unbounded/issues/555).

This preparation does not mean the project has been submitted, contributed, or
accepted. The project must not be represented as an official CNCF project unless
the CNCF Technical Oversight Committee approves the application and the
applicable Contribution Agreement is signed.

## Public Project Evidence

- [Project overview](README.md)
- [Roadmap](ROADMAP.md)
- [Contributing guide](CONTRIBUTING.md)
- [Code of Conduct](CODE_OF_CONDUCT.md)
- [Maintainers](MAINTAINERS.md)
- [Governance](GOVERNANCE.md)
- [Security policy](SECURITY.md)
- [License](LICENSE)
- [Third-party notices](NOTICE)

The proposed contribution scope is this repository. Maintainers must confirm
that scope before submission.

## Repository Readiness

| Area | Public evidence | Status |
|---|---|---|
| Project direction | [ROADMAP.md](ROADMAP.md) | Published; maintainer approval required |
| Maintainers | [MAINTAINERS.md](MAINTAINERS.md) | Team authority published; application representatives require consent |
| Governance | [GOVERNANCE.md](GOVERNANCE.md) | Published; maintainer approval required |
| Security reporting | [SECURITY.md](SECURITY.md) | Published through the Microsoft security response process |
| Contribution process | [CONTRIBUTING.md](CONTRIBUTING.md) | Published |
| Dependency notices | [NOTICE](NOTICE) | Generated from direct Go, npm, and Cargo dependencies plus pinned native dependencies |
| Adopters | GitHub issue #555 | Optional; no claims without adopter approval |
| Application worksheet | [GitHub issue #555](https://github.com/Azure/unbounded/issues/555) | Proposed technical wording awaits maintainer approval |

The application should describe the CRDs as project implementation APIs unless
maintainers approve a separate standards scope. It should list integrations as
supported only where public documentation and tests demonstrate them, and it
should distinguish planned integrations from shipped functionality.

## Submission Gates

The following decisions require approval outside normal repository review and
remain tracked in issue #555:

- Official project and trademark name.
- Authorization to pursue the CNCF contribution.
- Core project licensing path under the CNCF IP Policy.
- Product or service separation statement.
- Ownership and transferability of project trademarks, domains, repositories,
  registries, and accounts.
- Contributing or sponsoring entity and authorized signatory.
- Consenting application contacts and any other named individuals.
- Maintainer approval of the complete public application.

Adopters are not inferred from repository activity. An adopter reference should
be published only when the adopting organization has approved the statement;
otherwise the optional application field should remain blank.
