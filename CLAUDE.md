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

## Reporting status

When asked where things stand, or when finishing a stretch of work, open
with two blocks and nothing before them:

**Where we are** followed by what shipped, in one short paragraph.

**Open, ordered by what actually matters** followed by a table whose left
column is the reason a thing matters and whose right column is the task
numbers. Not a flat list, and not ordered by task number. Suggested
buckets, adapted to the situation: blocking a government conversation,
the next moat, competitive parity, engineering debt, business, spikes.

Then the substance. No preamble, no recap of what the person watched
happen.

## Every change gets a non-technical summary

Anything built, fixed or updated is explained in plain language before
the conversation moves on. Not a changelog line and not the commit
message reworded. What was wrong, what it means for someone who will
never read the code, and what is still not true.

The audience is an auditor, a contracting officer, or a commander. If a
sentence needs a reader to know what a Merkle tree is, rewrite it.

## Documents that must be kept current

These go stale silently, and a stale document is worse than none because
it is quoted with confidence. After any change that touches what they
assert, update all four in the same sitting:

- `~/VERDIFAX/business-records/` walkthrough, the end-to-end technical
  document. Built from `content.py` via `build.py`.
- `~/VERDIFAX/business-records/` competitive landscape. Built from
  `competitive_content.py` via `build_competitive.py`.
- `~/VERDIFAX/business-records/` mandate analysis. Built from
  `mandate_content.py` via the same builder.
- The task list, including closing what shipped and filing what the work
  exposed.

Each `.md` still needs its PDF companion, and the builders refuse to
render an em dash, so a stale document cannot be blamed on the tooling.

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
