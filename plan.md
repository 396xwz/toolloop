# Plan: native `/git [revision]` REPL command

## Goal
Add a native, terminal-friendly `/git [revision]` command to the Go REPL, inspired by `../oh-my-pi`'s `/git` entry point but adapted to this project's simple line-oriented UX.

## Constraints and compatibility notes
- Keep the feature **read-only**. `oh-my-pi` opens a fullscreen Git UI with staging/commit actions, but this REPL should provide inspection only.
- **No new dependencies** and no TUI framework.
- Use the existing `git` executable via `exec.CommandContext(...)` with explicit args; **never** use `bash -c` or shell interpolation for revision input.
- Keep output **bounded** and visibly truncated when large.
- Require Git 2.30+ for safe revision parsing with `rev-parse --end-of-options`;
  report a clear version error when that option is unsupported.
- Give each Git invocation a timeout and disable text-conversion drivers for
  every patch-producing command.
- Return clear errors when:
  - `git` is unavailable
  - the current directory is not inside a Git repository
  - a supplied revision cannot be resolved
- Update REPL help/README text and add focused tests only.

## Reference behavior to preserve from `oh-my-pi`
- `/git` means “show me the current repository state”.
- `/git <revision>` means “inspect a specific historical revision”.
- Preserve that mental model, but replace the fullscreen UI with plain structured text sections.

## Proposed UX

### `/git`
Print a compact repository overview with these sections:
1. Repository root
2. Current branch / detached-HEAD summary
3. `git status --short --branch` output
4. Staged diff section
5. Unstaged diff section

Suggested command mix:
- repo detection: `git rev-parse --show-toplevel`
- branch/head summary: `git symbolic-ref --short HEAD` with detached fallback from `git rev-parse --short HEAD`
- status: `git status --short --branch --untracked-files=all`
- staged diff: `git diff --cached --stat --patch --unified=3 --no-ext-diff --no-color --no-textconv`
- unstaged diff: `git diff --stat --patch --unified=3 --no-ext-diff --no-color --no-textconv`

Behavior details:
- If the repo is clean, still show status and explicit “no staged changes” / “no unstaged changes” messages.
- Use section headers so the output remains readable in a plain terminal.

### `/git <revision>`
Print:
1. Resolved revision / commit SHA
2. Commit metadata (author, date, subject, refs if present)
3. Revision diff/stat

Suggested command mix:
- resolve input safely: `git rev-parse --verify --quiet --end-of-options <rev^{commit}>`
- metadata: `git show --no-patch --format=fuller <sha>`
- diff: `git show --stat --patch --unified=3 --no-ext-diff --no-color --no-textconv --format= <sha>`

Behavior details:
- Validate first and show a friendly error like `invalid git revision: <input>` when resolution fails.
- Resolve once, then pass the resulting SHA to later commands so downstream calls do not re-interpret raw user input.

## Output-bounding strategy
- Add a small helper for bounded Git command output instead of unbounded `CombinedOutput()`.
- Cap each major diff section and cap the final rendered response.
- Append a clear marker such as `...[truncated]` when limits are hit.
- Disable color/pagers so output is deterministic and testable.

## Implementation plan

### Step 1: Add a dedicated REPL Git helper
Target file: `cmd/toolloop/main.go`

Create a small helper layer dedicated to the REPL slash command, for example:
- `handleGitREPLCommand(ctx, revision string) (string, error)`
- `runGitLimited(ctx, dir string, maxBytes int, args ...string) (string, error)`
- optional formatting helpers for clean section rendering

Why here:
- `/git` is a REPL slash command, not a model-exposed tool.
- Keeping it in `cmd/toolloop` avoids widening the public tool surface.

### Step 2: Wire `/git` into command dispatch
Target file: `cmd/toolloop/main.go`

Update:
- startup REPL banner in `runREPL(...)`
- `/help` output in `handleREPLCommand(...)`
- slash-command switch with a new `/git` case

Command shape:
- `/git`
- `/git <revision>`
- for more than one argument token beyond the command, either join the tail as one revision string or reject with `usage: /git [revision]` (pick the simpler, testable behavior and document it)

### Step 3: Keep errors explicit and user-facing
Target file: `cmd/toolloop/main.go`

Normalize likely failures into stable messages:
- `git not available`
- `not inside a git repository`
- `invalid git revision: <rev>`

Include raw stderr only when it improves clarity and remains bounded.

### Step 4: Update docs/help text
Target file(s): `README.md`, plus inline REPL help strings in `cmd/toolloop/main.go`

Document:
- `/git` shows repo overview (branch/status + staged/unstaged diffs)
- `/git <revision>` shows commit metadata + diff
- read-only nature of the command
- bounded output expectation

Add one or two short REPL examples near the existing command examples.

### Step 5: Add focused tests
Primary target file: `cmd/toolloop/main_test.go`

Favor small integration-style tests with temporary repositories created during the test:
1. outside-repo error
2. invalid-revision error
3. `/git` on a repo with both staged and unstaged changes
4. `/git <revision>` shows metadata and diff for a committed change
5. status entries preserve the leading space for unstaged-only changes and the
   trailing space for staged-only changes
6. textconv is not invoked by overview or revision patches
7. Git subprocess timeouts have a clear error

Test notes:
- Skip when `git` is not on `PATH`
- configure local author name/email inside temp repos
- assert stable substrings, not full Git output, because exact formatting can vary slightly by Git version
- ensure no tests rely on color or a pager

## Task list
- [x] Design the plain-text `/git` output format for current-state vs revision views
- [x] Implement safe Git execution helpers in `cmd/toolloop/main.go`
- [x] Add `/git [revision]` to REPL command handling and help text
- [x] Add bounded-output/truncation behavior with readable markers
- [x] Update `README.md` examples and command documentation
- [x] Add focused tests in `cmd/toolloop/main_test.go`
- [x] Preserve Git status porcelain's two-column entries in rendered output
- [x] Disable textconv and enforce/report per-command Git timeouts
- [x] Document and detect the Git 2.30+ revision-parsing requirement
- [x] Run targeted Go tests covering the new helper/command behavior

## Acceptance criteria
- `/git` inside a repo prints branch/status plus separate staged and unstaged diff sections
- `/git <revision>` prints commit metadata plus a diff for the resolved revision
- invalid repo/revision conditions return clear errors
- output is bounded and truncation is explicit
- implementation uses no new dependencies and no unsafe shell interpolation
- help text, README, and focused tests are updated
