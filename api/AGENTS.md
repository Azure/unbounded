# API doc comments

Comments on types and fields in `api/` are the published API documentation.
controller-gen copies them into the CRD descriptions under `deploy/*/crd/`,
which `kubectl explain` and the reference docs show. Write them for someone
using the API, not for someone maintaining the controllers.

## Write

- What the type or field is and what it is for.
- Who sets it: the user or the system.
- Valid values, the default, and units.
- How it interacts with other fields, and which setting wins.
- Behavior a user will notice and must plan around, such as a field that does
  nothing yet.

## Keep it short and plain

- Start with one sentence that says what the type or field is. Most comments
  need one to three sentences.
- Do not restate the name. "Type is the update strategy type" tells the reader
  nothing.
- Use everyday words: "set" not "populated", "newest" not "highest", "completes
  or fails" not "reaches a terminal phase", "runs" not "executes", "reinstall"
  not "reprovision", "one per cluster" not "singleton".
- Keep project terms that appear in the API itself, such as "repave" (the
  `RepavePending` condition and the `kubectl unbounded machine repave`
  command), and make the sentence say when it happens.
- Say each fact once. When a field's comment explains its enum values, keep
  each constant's comment to one line.
- Say what a field is, not why the controller stores it.
- Keep the text self-contained. Do not reference repository paths or link to
  other docs: `kubectl explain` users cannot follow them. Put longer detail in
  `docs/content/reference/` instead.

## Do not write

- How a controller does its work: the commands it runs, the order of its
  checks, or the functions and files involved.
- Which component reads or writes a field, unless the user must install or
  configure that component. Name the component; do not describe how it works.
- Comparisons with other types or components, such as "like X and unlike the
  others". Describe only the thing being documented.
- Details that can change without an API change, such as how the node is hosted
  today.

Test each sentence: would it still be true if the controllers were rewritten
with the same API behavior? If not, it belongs in `docs/`, a design document or
an issue.

Code is still the source of truth for checking a claim (see "Sources of Truth"
in the root `AGENTS.md`). Check every statement against the code, but describe
what the API does, not how the code does it.

## Behavior the API describes but the code does not do yet

Say so plainly, using one of these notes so they are easy to find. Keep each
note on one line of the comment, so a search for the exact phrase finds it:

- "Not used yet." for a spec field or value the system ignores.
- "Not set yet." for a status field, condition, phase, stage or annotation the
  system never writes.
- "Not checked yet." for a rule nothing enforces.
- For behavior that only partly works, say what does not happen yet, such as
  "Not used at first install yet."

Add at most one more sentence when the user needs to know the consequence. When
an issue tracks the fix, reference it in a TODO separated from the doc comment
by a blank line, so controller-gen leaves it out of the CRD:

```go
	// TODO(#1041): drop the "Not set yet" note once host keys are recorded.

	// Fingerprint is meant to hold the SSH host key seen on first connection.
	// Not set yet.
	Fingerprint string `json:"fingerprint,omitempty"`
```

For a type, put the TODO above the type's `+kubebuilder` marker block, also
separated by a blank line, so the marker block stays directly above the doc
comment.

When you fix the gap, search for its `TODO(#N)` and update the comment in the
same change.

## Enum values

controller-gen does not copy comments on constants into the CRD. Describe each
enum value briefly in the comment on the field that uses it. Constant comments
are for Go readers and follow the same rules.

## Field and type comments

For a field whose type is a struct, controller-gen uses the field's comment as
the CRD description and ignores the type's comment. The type's comment reaches
the CRD only where a field of that type has no comment. Put user-facing text on
the field.

## Before you finish

- Run `make generate` and read the description changes under `deploy/*/crd/`.
  That text is what users see.
- Keep the matching page under `docs/content/reference/` consistent with the
  comment.
