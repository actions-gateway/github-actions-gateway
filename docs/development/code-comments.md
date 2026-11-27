# Code comments

What a comment in this repo's source is for, how long it gets, and where everything else goes.
Applies to Go, Bash, Python, workflow and chart YAML, and Makefiles.
Agents apply it through the repo-local [`code-comments`](../../.claude/skills/code-comments/SKILL.md) skill, which states the rules without the reasons; this page is the reasons.

## The test

A comment is read by someone changing this file later, who has the code in front of them and nothing else: no pull request (PR), no backlog, no memory of the session that wrote it.
Keep a sentence only if that reader needs it to **read the code correctly or change it safely**.

Everything else has a better home, and the reader who wants it knows where to look: `git blame` leads to the commit, the commit to the PR, the PR to the backlog item.

## What a comment carries

- **Why the code is this way**, when the code cannot say so itself: the constraint it satisfies, the failure it avoids.
- **Precision the code cannot hold**: units, whether a bound is inclusive, what `nil` or an empty string means here, which lock must be held, what order calls must arrive in.
- **A trap for the next editor**: code that looks wrong, redundant, or simplifiable and is not.
  Say what breaks, in one clause.
- **The contract**, in a doc comment: what a caller may rely on and must provide.
- **A claim about upstream behavior**, with the version it was measured at in a parenthetical ([documentation-standards.md](documentation-standards.md#in-code-comments)).

A comment that restates the line under it carries none of these.
Delete it, or rename the thing so the name does the work.

## What goes somewhere else

| Content | Where it goes |
|---|---|
| What the code used to do, what changed, "no longer", "now" | Commit message |
| Why this approach beat the alternatives | PR description; `docs/design/` when it outlives the PR |
| The incident, flake, or review that prompted the code, and what it cost | Commit message and PR description |
| Measurements, run counts, benchmark figures, dates | PR description; `docs/` when a later decision rests on them |
| A backlog ID (`Q123`) or PR number for work that is finished | Commit message and PR description |
| A defense against a reviewer who might revert it | The PR thread |
| How a subsystem fits together, or a design walk-through | `docs/design/`, linked from one line in the comment |
| What an operator sees, sets, or does | `docs/operations/` |

Two of these need saying twice, because the repo's existing comments do both constantly.

**A finished backlog ID is a dead reference.** A completed row is deleted from `docs/queue/`, so the ID resolves to nothing in the tree.
An ID belongs in a comment in one case: a `TODO(Q123):` naming open work, which the closing PR removes.

**A link keeps a comment short; a paragraph does not.** When the reader does need the design argument, write it in `docs/design/` and leave `// See docs/design/…` under the one-line summary.
A doc has a gate on its links and a place in a reading order.
A comment paragraph has neither, and nothing fails when the code under it changes.

## Length

No style guide surveyed below sets a number, so these are this repo's own.

- **An implementation comment is one or two lines.** At four, stop and ask which sentences fail the test; at more, the content is a doc.
- **A doc comment is as long as the contract.** One sentence for most functions.
  It does not explain how the function works inside; that goes in the body, next to the line it explains, if it is needed at all.
- **A file or script header says what the file is for and how to invoke it.** Ten lines is generous.
  Design history and the story of each flag do not belong in it.
- **Say it once.** A rationale shared by several sites lives at the declaration; the other sites name the declaration.

Two kinds of comment are exempt from the length limits and from nothing else, because they are published documentation that happens to live in source: godoc on Custom Resource Definition (CRD) fields, which becomes the API description, and comments in a chart's `values.yaml`, which are what an operator reads.

## By language

**Go.** Doc comments on exported names are full sentences that open with the name ([Go Doc Comments](https://go.dev/doc/comment)).
Skip what the signature already says: not every parameter needs a sentence, and context cancellation is assumed.
Do say what a caller cannot guess: concurrency safety when it is not the obvious answer, the zero value, who closes what.
A `//nolint` names the reason on the same line.
A test's name states its scenario; a comment above it that repeats the name goes.

**Bash.** The header is the purpose and the usage line.
A function gets a comment when its arguments, globals, or outputs are not obvious from a short body.
`# shellcheck disable=` carries its justification on the line above ([bash-style.md](bash-style.md)).
An unquoted expansion that is meant to word-split says so.

**Python.** Docstrings for public functions and modules; inline comments rarely, and never to narrate a line.

**Workflow YAML.** A comment says why a step, permission, or condition exists when its name does not.
An action pinned to a commit keeps its version as a trailing comment on the same line (`@3d3c42e… # v7.0.1`), which is where the update tooling looks for it.
The history of a job's failures goes in the PR that fixed them.

**Makefile.** `target: ## description` is the help text and stays one line.
A block comment above a target explains a non-obvious prerequisite or ordering, and nothing about how the target came to exist.

## Where this repo's comments stand

Measured 2026-11-27 at `036f6ccc9`, over tracked first-party source outside `vendor/`, `testdata/`, and generated files.
Density is comment lines per non-blank code line.

| Source | Density | Comment blocks of 10 lines or more |
|---|---|---|
| Go, non-test | 0.50 | 550 |
| Go, tests | 0.19 | 182 |
| Shell, non-test | 0.65 | 320 |
| Workflow YAML | 0.69 | 88 |
| `Makefile` | 1.04 | 13 |

Comments cite a backlog ID 5,139 times, and 4,915 of those citations (96%) name an ID with no row left in `docs/queue/`.
The longest doc comment on a function is 98 lines, above `Admit` in `cmd/agc/internal/provisioner/admission.go`: three headed sections of design argument, a rejected alternative with its measurements, and eight backlog IDs.
Most of it argues for the design, which is a `docs/design/` page a caller of `Admit` could be pointed at in one line.

So **do not match the density of the file you are editing.** The surrounding comments are the thing this page exists to stop, and matching them reproduces it.
Write new comments to this page, trim a comment when you change the code it describes, and leave the rest for a change that is about comments.

## What enforces this

`make comment-provenance-check`, in `make check` and CI, fails a branch that adds a comment line carrying a backlog ID outside `TODO(Q123):`, a pull request number, a date, or the phrase "used to" in its history sense.
A date passes beside the word `measured` or a version on the same line, which is the upstream citation above; an upstream issue is written `owner/repo#123`; and a line that must keep the text carries `comment-provenance: <reason>`.
Everything else on this page is review's to hold: the length limits, other narration wording ("no longer" describes runtime state four times in five here, so it cannot be gated), and whether a sentence passes the test.

## Sources

Read 2026-11-27.
Where they disagree, the paragraph says which way this page went.

- [Go Doc Comments](https://go.dev/doc/comment), [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments), and the Google Go style guide's [decisions](https://google.github.io/styleguide/go/decisions) and [best practices](https://google.github.io/styleguide/go/best-practices): the Go rules above.
- [Google Shell Style Guide](https://google.github.io/styleguide/shellguide.html): file header, function comments, and commenting only the non-obvious parts.
- [PEP 8](https://peps.python.org/pep-0008/) and the [Google Python Style Guide](https://google.github.io/styleguide/pyguide.html): comments that restate code distract, and a comment that contradicts the code is worse than none.
- Google's engineering practices on [what reviewers look for](https://google.github.io/eng-practices/review/reviewer/looking-for.html) and [change descriptions](https://google.github.io/eng-practices/review/developer/cl-descriptions.html): a comment explains why the code exists, and the change description is the permanent record of what changed and why.
  This is the nearest a primary source comes to the table above; none of those read states "no history in comments" outright.
- [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md): every API field carries a description because it generates the reference docs, which is the CRD exemption.
- [Linux kernel coding style](https://www.kernel.org/doc/html/latest/process/coding-style.html): over-commenting is a named danger, and a function that needs comments inside its body wants splitting.
- [GitHub's secure-use reference](https://docs.github.com/en/actions/reference/security/secure-use): pin actions to a full commit.
- Ellen Spertus, [Best practices for writing code comments](https://stackoverflow.blog/2021/12/23/best-practices-for-writing-code-comments/): comments should not duplicate code or excuse unclear code.
  Her rule to reference the tracker when fixing a bug is the one this page narrows: the reference stays when it explains why present code must not be simplified, such as a workaround for an open upstream bug, and goes when it only records which item delivered the code.
- John Ousterhout, *A Philosophy of Software Design*, argues the other direction: what a reader of the code needs belongs in the code and not the commit log.
  Read from a summary, not the book.
  The test above is where the two meet: the constraint stays in the comment, the story of the change goes in the commit.
