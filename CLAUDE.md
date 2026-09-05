# Working rules for this repository

Read automatically at the start of every session. These are standing
instructions, not suggestions, and several exist because they were
violated repeatedly before being written down.

## Writing

**Never use an em dash (—) or an en dash (–). Anywhere. No exceptions in
prose.** Not in customer-facing output, not in reports, not in commit
messages, not in code comments, not in chat. Use a comma, a semicolon, a
colon, a full stop, or parentheses. If a sentence seems to need a dash,
it needs to be two sentences.

This rule was broken across an entire working day, including inside an
auditor-facing PDF, after being raised more than once. `backend/cmd/
mesedi-verify/no_emdash_test.go` now fails the build if one reaches
rendered output, because a rule that depends on remembering is not a
rule.

**THE ONE EXCEPTION IS NOT PROSE AND MUST NOT BE "FIXED".**
`backend/cmd/mesedi-verify/rekorproof.go` declares:

    const sigLinePrefix = "— "

That em dash is the signature-line prefix of the c2sp.org/signed-note
format Sigstore signs. Replacing it makes every Rekor checkpoint
signature fail to parse. The code still compiles, most tests still pass,
and offline verification silently stops working. A sanitizer pass has
already done this once. `TestTheCheckpointSignaturePrefixIsLeftAlone`
guards it. The same applies to any test fixture deliberately containing
non-ASCII text to prove non-ASCII is handled, such as the unicode case in
`internal/api/list_search_helpers_test.go`.

Also avoid: "delve", "leverage", "robust", "seamless", and opening a
reply by restating the question.

## Commits

Subject line, then at most three lines of body. Not more. Explain why,
not what; the diff shows what.

## Working style

- One command at a time. Wait for the result before sending the next.
- Label every command for the repository it belongs to before sending it.
- Never `git add -A` and never an unscoped `gofmt -w .`. Stage explicit
  paths only.
- Do not filter the output of tests, audits, or verification runs through
  `head`, `tail`, or `grep`. Choosing which parts of a safety report get
  seen is the same instinct as skipping the check.
- Give the push command on its own, after everything else has passed.
- Every document produced as `.md` needs a PDF companion.
- Business-sensitive material goes in `~/VERDIFAX/business-records/`,
  outside git.

## Order of operations for anything an auditor will read

Commit, push, then build, then run, then generate the PDF. Building from
a dirty tree stamps the report "UNCOMMITTED CHANGES", which correctly
tells the reader the result cannot be reproduced by anyone else.

## Verification

Assume a green test suite proves nothing until the change has run against
real production data. Every serious defect found in this codebase so far
was invisible to a passing suite and obvious within minutes of real data.
When a test is written for a bug, mutate the fix and confirm the test
actually fails before trusting it.
