// Package edittool implements the edit tool: a sloppy anchored patch applied
// to one or more files. The payload syntax is parsed by package sloppy; this
// package holds the apply stage (matching and rewriting) and the tool wiring.
//
// # Apply stage
//
// Operations anchor against the original file text, so earlier edits never
// shift later anchors. All operations of a payload apply atomically: one
// failure and no file is written. A pattern is located by a matching ladder —
// byte-exact, then whitespace- and typography-tolerant, then fuzzy (same
// operator run, at most a few character edits) — and only a unique anchor is
// claimed unless the edit header carries " all". Failures name the fragment
// that missed, show the file region it nearly matches, and hand back a
// copy-ready payload to resend.
//
// # Commit stage
//
// Path spellings are normalized to absolute targets, so aliases (a.ts,
// ./a.ts, dir/../a.ts, an absolute path) fold into one planned file. Anchor
// validation is purely in-memory; the commit then re-checks every target for
// external edits, stages the new bytes beside each file, and swaps them with
// same-directory renames journaled under the OS temp dir. A failed swap rolls
// the whole group back, an external edit is preserved and reported as a
// conflict, and a process killed mid-swap is reconciled before the next edit
// runs, so a success response means every target of the group is on disk.
//
// # Deviations from the reference engine
//
//   - A "*** SM:PUT" replaces the whole FIND match even when the pattern
//     carries ⟪old│new⟫ selections; selections rewrite in place only when
//     there is no action header. Per-selection positional substitution is not
//     implemented.
//   - Parse-time recovery of garbled payloads (bare selections without the │
//     divider, unified-diff rewrites, ＋/－ marker lines) is rejected instead of
//     reinterpreted; package sloppy errors say so.
//   - A no-op is reported once per call; the reference escalates after three
//     identical no-ops per file and session.
package edittool
