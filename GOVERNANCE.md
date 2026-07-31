# Governance

## Scope

This document describes how Project Unbounded makes technical and community
decisions. It applies to the source, documentation, releases, and project policy
maintained in this repository.

## Roles

### Contributors

Contributors participate by opening issues, proposing designs, reviewing work,
writing documentation, or submitting code. Anyone who follows the Code of
Conduct and contribution requirements may contribute.

### Maintainers

Maintainers are members of the team identified in [MAINTAINERS.md](MAINTAINERS.md).
They are expected to:

- Actively review and merge changes.
- Maintain project quality, security, and release practices.
- Explain significant technical decisions in public issues, pull requests, or
  design documents.
- Apply project policies consistently and disclose relevant conflicts of
  interest.
- Help contributors become reviewers and maintainers.

### Project leads

The maintainer team may designate project leads to coordinate cross-component
direction, releases, or external project relationships. Project leads do not
have unilateral authority over technical decisions. Their decisions remain
subject to the same consensus and conflict-resolution process as other
maintainer decisions.

## Decision Making

Routine changes are decided through public pull-request review. A maintainer
approval and passing required checks are sufficient unless another maintainer
raises a substantive objection.

Changes with broad architectural, compatibility, security, governance, or
project-scope impact should begin with a public issue or design document. The
maintainers seek lazy consensus: a proposal may proceed when relevant concerns
have been addressed and no unresolved maintainer objection remains after a
reasonable opportunity for review.

When consensus cannot be reached, the participating maintainers may call a
vote. Each non-conflicted maintainer has one vote. A proposal passes with at
least two affirmative votes and a simple majority of votes cast. Governance
changes and maintainer removals require a two-thirds majority of non-conflicted
maintainers. The result and rationale must be recorded publicly.

## Maintainer Lifecycle

Any contributor may be nominated as a maintainer by an existing maintainer.
Candidates should demonstrate sustained, constructive participation, sound
technical judgment, reliable reviews, and commitment to project governance and
the Code of Conduct. Addition requires approval by two-thirds of the current
non-conflicted maintainers.

Maintainers may step down at any time. A maintainer who has been inactive for
six months may be moved to emeritus status after a public attempt to contact
them. Emeritus maintainers retain recognition but not approval or voting
authority and may request reinstatement through the normal nomination process.

A maintainer may be removed for repeated failure to meet maintainer
responsibilities, unresolved conflicts of interest, or Code of Conduct
violations. Removal follows the voting rule above. Sensitive conduct details
must not be published when doing so would violate privacy or the applicable
Code of Conduct process.

## Conflicts Of Interest

Maintainers must disclose interests that could reasonably affect their judgment
on a decision. A conflicted maintainer may provide factual context but must not
approve, block, or vote on that decision. Employment alone does not require
recusal, but a direct financial, reporting, or product-specific interest may.

## Disputes And Conduct

Technical disagreements should first be resolved through the public issue or
pull request, with maintainers documenting the alternatives and tradeoffs. If
that fails, the voting process above provides a final project decision.

Conduct concerns follow [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), not the
technical voting process. Maintainers must not use governance decisions to
override or disclose confidential conduct handling.

## Amendments

Governance changes are proposed by pull request and use the governance voting
threshold defined above. The approving maintainers and decision rationale must
be recorded in the pull request.
