package edittool

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pulseaiclub/phi/internal/tools/tooldef"
)

// Staged commit: anchor validation only builds in-memory content, so a write
// failure used to leave earlier files of a multi-file payload already
// overwritten. The committer stages every result next to its target, swaps
// the files with same-directory renames, and journals the swap set in the OS
// temp dir. A failed swap rolls the group back; a process killed mid-swap is
// finished by reconcile() before the next edit reads anything.

const (
	journalVersion = 1
	journalName    = "journal.json"
	stagingPrefix  = ".phi-edit-staging-"
	origPrefix     = "orig-"
	nextPrefix     = "next-"
)

// errCommitHalted ends a commit without rollback or cleanup. Only test hooks
// return it: the on-disk shape left behind is what a killed process leaves.
var errCommitHalted = errors.New("edit commit halted before completion")

// errNoJournal means no interrupted commit is parked for this working tree.
var errNoJournal = errors.New("no edit journal")

// fileTarget is one file whose new content is ready for the commit phase.
type fileTarget struct {
	abs     string // absolute cleaned target
	display string // model-facing, cwd-relative when possible
	raw     []byte // bytes on disk when the anchors were read
	next    []byte // bytes to swap in, line-ending convention restored
	result  Result // diff and notes surfaced after the group commits
}

// journalEntry records one swap and the backups needed to undo it.
type journalEntry struct {
	Path string `json:"path"` // absolute target
	Tmp  string `json:"tmp"`  // same-directory staging file swapped over Path
	Orig string `json:"orig"` // journal-dir copy of the pre-edit bytes
	Next string `json:"next"` // journal-dir copy of the post-edit bytes
	Done bool   `json:"done"` // the rename into Path landed
}

type journalState struct {
	Version   int            `json:"version"`
	CWD       string         `json:"cwd"`
	Committed bool           `json:"committed"` // every swap landed
	Entries   []journalEntry `json:"entries"`
}

// commitHooks inject faults at commit boundaries. Production leaves every
// hook nil; tests use them to force write failures and simulated crashes.
type commitHooks struct {
	// beforeStage runs before each same-directory staging file is written.
	beforeStage func(abs string, index int) error
	// beforeRename runs before each staging file is swapped over its target.
	beforeRename func(abs string, index int) error
	// afterRename runs after each swap and its journal flip. Returning
	// errCommitHalted stops immediately, leaving disk and journal untouched —
	// reproducing a process dying between two swaps.
	afterRename func(abs string, index int) error
}

type committer struct {
	cwd   string
	hooks commitHooks
}

func newCommitter(cwd string, hooks commitHooks) *committer {
	return &committer{cwd: filepath.Clean(cwd), hooks: hooks}
}

// journalDir is the crash-recovery journal for one working tree. It is keyed
// by a hash of the cwd so the path is stable across processes — recovery has
// to find it without any in-memory state — while keeping the workspace clean.
func (c *committer) journalDir() string {
	sum := sha256.Sum256([]byte(c.cwd))
	return filepath.Join(os.TempDir(), "phi-edit-"+hex.EncodeToString(sum[:8]))
}

// reconcile finishes or discards an interrupted commit left by an earlier
// process before the current edit touches anything. A rolled-back interrupt
// returns an error: a new payload must not silently apply on top of a state
// the model never saw confirmed.
func (c *committer) reconcile() error {
	dir := c.journalDir()
	state, err := readJournal(dir)
	if errors.Is(err, errNoJournal) {
		// A directory without a journal is staging debris from a crash before
		// the first flush: no swap could have landed, so discard it.
		if _, statErr := os.Stat(dir); statErr == nil {
			_ = os.RemoveAll(dir)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if state.Committed {
		// Every swap landed; only the cleanup was interrupted. The intended
		// post-edit state is already on disk, so nothing needs restoring.
		_ = os.RemoveAll(dir)
		return nil
	}

	var restored, conflicts, failures []string
	for _, entry := range state.Entries {
		conflict, err := restoreEntry(entry)
		display := tooldef.RelTo(c.cwd, entry.Path)
		switch {
		case err != nil:
			failures = append(failures, display)
		case conflict:
			conflicts = append(conflicts, display)
		default:
			restored = append(restored, display)
		}
	}
	if len(failures) > 0 {
		return failf(
			"an interrupted edit could not be rolled back for %s; fix the underlying file error and re-run the edit",
			strings.Join(failures, ", "))
	}
	_ = os.RemoveAll(dir)
	if len(restored) == 0 && len(conflicts) == 0 {
		return nil
	}

	var message strings.Builder
	message.WriteString(atomicityNotice + "\n")
	if len(restored) > 0 {
		fmt.Fprintf(
			&message,
			"An interrupted edit from a previous run was rolled back; %s restored to pre-edit content. Re-send the payload.\n",
			strings.Join(restored, ", "),
		)
	}
	if len(conflicts) > 0 {
		fmt.Fprintf(
			&message,
			"These files were changed outside phi after the interrupt and were left untouched: %s. Re-read them before re-sending.\n",
			strings.Join(conflicts, ", "),
		)
	}
	return applyError{strings.TrimRight(message.String(), "\n")}
}

// restoreEntry puts one journaled target back to its pre-edit content. It
// decides from the bytes on disk rather than the Done flag, because a crash
// between the rename and the journal flip must not restore a file that never
// swapped. conflict=true means an external writer changed the target after
// the swap; that content is preserved untouched.
func restoreEntry(entry journalEntry) (conflict bool, err error) {
	if _, statErr := os.Stat(entry.Tmp); statErr == nil {
		_ = os.Remove(entry.Tmp) // staging file left behind by an unswapped entry
	}
	current, err := os.ReadFile(entry.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil // the target vanished externally; preserve that outcome
		}
		return false, err
	}
	next, err := os.ReadFile(entry.Next)
	if err != nil {
		return false, fmt.Errorf("missing staged content for %s: %w", entry.Path, err)
	}
	orig, err := os.ReadFile(entry.Orig)
	if err != nil {
		return false, fmt.Errorf("missing pre-edit backup for %s: %w", entry.Path, err)
	}
	if bytes.Equal(current, next) {
		return false, writeAtomic(entry.Path, orig, 0o644)
	}
	return !bytes.Equal(current, orig), nil
}

// verify re-reads every target before staging. Bytes that no longer match the
// snapshot the anchors matched mean an external writer won the race, and the
// commit must refuse to overwrite that work.
func verify(targets []fileTarget) error {
	var changed []string
	for _, target := range targets {
		current, err := os.ReadFile(target.abs)
		if err != nil {
			return failf(
				"[%s]: cannot re-read the file before committing: %v.\nNo files were modified — sections apply atomically.",
				target.display,
				err,
			)
		}
		if !bytes.Equal(current, target.raw) {
			changed = append(changed, target.display)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	return applyError{atomicityNotice + "\n" + fmt.Sprintf(
		"[%s]: file changed on disk after it was read; refusing to overwrite external edits. "+
			"Re-read it and re-send the payload against the current content.\n"+
			"No files were modified — sections apply atomically.",
		strings.Join(changed, "], ["))}
}

// commit stages and swaps the whole group. Success means every target holds
// its new bytes; any failure rolls the swaps back before the error returns.
func (c *committer) commit(targets []fileTarget) error {
	if len(targets) == 0 {
		return nil
	}
	dir := c.journalDir()
	if err := os.Mkdir(dir, 0o700); err != nil {
		if os.IsExist(err) {
			// reconcile() runs first, so existence means a concurrent committer.
			return failf("an edit commit for %s is already in progress; retry after it finishes", c.cwd)
		}
		return fmt.Errorf("failed to create edit staging area: %w", err)
	}

	state := &journalState{Version: journalVersion, CWD: c.cwd}
	for i, target := range targets {
		entry := journalEntry{
			Path: target.abs,
			Orig: filepath.Join(dir, fmt.Sprintf("%s%d", origPrefix, i)),
			Next: filepath.Join(dir, fmt.Sprintf("%s%d", nextPrefix, i)),
			Tmp:  filepath.Join(filepath.Dir(target.abs), stagingPrefix+randSuffix()),
		}
		if err := os.WriteFile(entry.Orig, target.raw, 0o600); err != nil {
			os.RemoveAll(dir)
			return stageFailure(target.display, err)
		}
		if err := os.WriteFile(entry.Next, target.next, 0o600); err != nil {
			os.RemoveAll(dir)
			return stageFailure(target.display, err)
		}
		state.Entries = append(state.Entries, entry)
	}
	if err := writeJournal(dir, state); err != nil {
		os.RemoveAll(dir)
		return err
	}

	// Stage next to each target so the final swap is same-filesystem and
	// atomic; nothing has been renamed at this point.
	for i, target := range targets {
		if c.hooks.beforeStage != nil {
			if err := c.hooks.beforeStage(target.abs, i); err != nil {
				return c.abort(dir, state, target.display, err)
			}
		}
		//nolint:gosec // G306: source files stay world-readable, matching write
		if err := os.WriteFile(state.Entries[i].Tmp, target.next, 0o644); err != nil {
			return c.abort(dir, state, target.display, err)
		}
	}

	for i := range targets {
		target := &targets[i]
		entry := &state.Entries[i]
		if c.hooks.beforeRename != nil {
			if err := c.hooks.beforeRename(entry.Path, i); err != nil {
				return c.abort(dir, state, target.display, err)
			}
		}
		// Cover the verify-to-swap race window: an external edit landing here
		// must survive instead of being replaced by the staged rename.
		current, err := os.ReadFile(entry.Path)
		if err != nil {
			return c.abort(dir, state, target.display, err)
		}
		if !bytes.Equal(current, target.raw) {
			return c.abortConflict(dir, state, target.display)
		}
		if err := os.Rename(entry.Tmp, entry.Path); err != nil {
			return c.abort(dir, state, target.display, err)
		}
		entry.Done = true
		_ = writeJournal(dir, state) // best effort: reconcile is content-aware
		if c.hooks.afterRename != nil {
			if err := c.hooks.afterRename(entry.Path, i); err != nil {
				if errors.Is(err, errCommitHalted) {
					return err // deliberate hard stop; journal stays on disk
				}
				return c.abort(dir, state, target.display, err)
			}
		}
	}

	state.Committed = true
	_ = writeJournal(dir, state)
	_ = os.RemoveAll(dir)
	return nil
}

// abort undoes every swap already made and reports the file that stopped the
// group. Rollback is content-aware, so an external edit landing after a swap
// is preserved and named rather than overwritten with the backup.
func (c *committer) abort(dir string, state *journalState, display string, cause error) error {
	conflicts, failures := c.rollback(state)
	_ = os.RemoveAll(dir)

	var message strings.Builder
	message.WriteString(atomicityNotice + "\n")
	fmt.Fprintf(&message, "[%s]: commit failed: %v.\n", display, cause)
	for _, conflict := range conflicts {
		fmt.Fprintf(&message, "Rollback kept the externally edited file %s untouched.\n", conflict)
	}
	for _, failure := range failures {
		fmt.Fprintf(&message, "Rollback could not restore %s — recover it from version control.\n", failure)
	}
	if len(failures) == 0 {
		message.WriteString("No files were modified — sections apply atomically.")
	}
	return applyError{strings.TrimRight(message.String(), "\n")}
}

// abortConflict is abort for the race-window check: no swap failed, the target
// simply changed underneath the commit.
func (c *committer) abortConflict(dir string, state *journalState, display string) error {
	conflicts, failures := c.rollback(state)
	_ = os.RemoveAll(dir)

	var message strings.Builder
	message.WriteString(atomicityNotice + "\n")
	fmt.Fprintf(&message,
		"[%s]: file changed on disk while the commit was in flight; refusing to overwrite external edits. "+
			"Re-read it and re-send the payload.\n", display)
	for _, conflict := range conflicts {
		fmt.Fprintf(&message, "Rollback kept the externally edited file %s untouched.\n", conflict)
	}
	for _, failure := range failures {
		fmt.Fprintf(&message, "Rollback could not restore %s — recover it from version control.\n", failure)
	}
	if len(failures) == 0 {
		message.WriteString("No files were modified — sections apply atomically.")
	}
	return applyError{strings.TrimRight(message.String(), "\n")}
}

// rollback restores entries in reverse swap order, reporting files whose
// current bytes are neither the staged result nor the backup.
func (c *committer) rollback(state *journalState) (conflicts, failures []string) {
	for _, entry := range slices.Backward(state.Entries) {
		conflict, err := restoreEntry(entry)
		display := tooldef.RelTo(c.cwd, entry.Path)
		switch {
		case err != nil:
			failures = append(failures, display)
		case conflict:
			conflicts = append(conflicts, display)
		}
	}
	return conflicts, failures
}

func stageFailure(display string, err error) error {
	return applyError{atomicityNotice + "\n" + fmt.Sprintf(
		"[%s]: failed to stage the edit: %v.\nNo files were modified — sections apply atomically.",
		display, err)}
}

func readJournal(dir string) (*journalState, error) {
	raw, err := os.ReadFile(filepath.Join(dir, journalName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNoJournal
		}
		return nil, err
	}
	var state journalState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, failf("interrupted edit journal at %s is corrupt; inspect it before editing again", dir)
	}
	return &state, nil
}

// writeAtomic replaces path via a same-directory temp file plus rename, so
// readers never observe a half-written file and the swap never crosses
// filesystems.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), stagingPrefix+randSuffix())
	//nolint:gosec // G306: the caller chooses the permission bits
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeJournal(dir string, state *journalState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, journalName), data, 0o600)
}

func randSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback" // crypto/rand has no realistic failure path
	}
	return hex.EncodeToString(b[:])
}
