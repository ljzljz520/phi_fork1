package edittool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// twoFilePayload edits a.ts and b.ts; both anchors match the fixtures written
// by writeFiles in edit_test.go.
func twoFilePayload() string {
	return "*** SM:EDIT a.ts\n*** SM:FIND\nconst endpoint = \"/v1\";\n*** SM:PUT\nconst endpoint = \"/v2\";\n" +
		"*** SM:EDIT b.ts\n*** SM:FIND\nrouter.use(\"/v1\", api);\n*** SM:PUT\nrouter.use(\"/v2\", api);\n"
}

func twoFileFixtures() map[string]string {
	return map[string]string{
		"a.ts": "const endpoint = \"/v1\";\n",
		"b.ts": "router.use(\"/v1\", api);\n",
	}
}

// journalPath exposes where a committer for this test's cwd parks its journal.
func journalPath(t *testing.T, dir string) string {
	t.Helper()
	return newCommitter(dir, commitHooks{}).journalDir()
}

// assertNoLeftovers fails if a staging temp or the journal survived the call.
func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), stagingPrefix, "staging temp left behind in %s", dir)
	}
	_, err = os.Stat(journalPath(t, dir))
	assert.True(t, os.IsNotExist(err), "edit journal should be removed after the call")
}

// A staging failure on the second file must not rename anything.
func TestEditStageFailureOnSecondFileWritesNothing(t *testing.T) {
	ctx, dir := toolContext(t)
	writeFiles(t, dir, twoFileFixtures())

	hooks := commitHooks{
		beforeStage: func(_ string, index int) error {
			if index == 1 {
				return assert.AnError
			}
			return nil
		},
	}
	_, err := runEditWith(ctx, editInput{Payload: twoFilePayload()}, hooks)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[b.ts]")
	assert.Contains(t, err.Error(), atomicityNotice)

	files := readFiles(t, dir, "a.ts", "b.ts")
	assert.Equal(t, "const endpoint = \"/v1\";\n", files["a.ts"], "first file must stay untouched")
	assert.Equal(t, "router.use(\"/v1\", api);\n", files["b.ts"])
	assertNoLeftovers(t, dir)
}

// A rename failure on the second file must roll the first file's swap back.
func TestEditRenameFailureOnSecondFileRollsBack(t *testing.T) {
	ctx, dir := toolContext(t)
	writeFiles(t, dir, twoFileFixtures())

	hooks := commitHooks{
		beforeRename: func(_ string, index int) error {
			if index == 1 {
				return assert.AnError
			}
			return nil
		},
	}
	_, err := runEditWith(ctx, editInput{Payload: twoFilePayload()}, hooks)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[b.ts]: commit failed")
	assert.Contains(t, err.Error(), "No files were modified — sections apply atomically.")

	files := readFiles(t, dir, "a.ts", "b.ts")
	assert.Equal(t, "const endpoint = \"/v1\";\n", files["a.ts"], "rolled-back first file must hold its original bytes")
	assert.Equal(t, "router.use(\"/v1\", api);\n", files["b.ts"])
	assertNoLeftovers(t, dir)
}

// A process dying after the first swap leaves a journal behind; the next edit
// call must restore the deterministic pre-edit state before doing anything.
func TestEditRecoversAfterInterruptedCommit(t *testing.T) {
	ctx, dir := toolContext(t)
	writeFiles(t, dir, twoFileFixtures())

	halted := commitHooks{
		afterRename: func(_ string, index int) error {
			if index == 0 {
				return errCommitHalted
			}
			return nil
		},
	}
	_, err := runEditWith(ctx, editInput{Payload: twoFilePayload()}, halted)
	require.ErrorIs(t, err, errCommitHalted)

	// The interrupted shape: first file swapped, journal parked on disk.
	files := readFiles(t, dir, "a.ts", "b.ts")
	assert.Equal(t, "const endpoint = \"/v2\";\n", files["a.ts"])
	assert.Equal(t, "router.use(\"/v1\", api);\n", files["b.ts"])
	_, err = os.Stat(journalPath(t, dir))
	require.NoError(t, err, "simulated crash must leave its journal behind")

	// The next tool call reconciles before applying anything and refuses to
	// stack a new edit on top of an unconfirmed state.
	_, err = runEdit(ctx, editInput{Payload: twoFilePayload()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "interrupted edit from a previous run was rolled back")
	assert.Contains(t, err.Error(), "a.ts")
	assert.Contains(t, err.Error(), atomicityNotice)

	files = readFiles(t, dir, "a.ts", "b.ts")
	assert.Equal(t, "const endpoint = \"/v1\";\n", files["a.ts"], "reconcile must restore the swapped file")
	assert.Equal(t, "router.use(\"/v1\", api);\n", files["b.ts"])
	assertNoLeftovers(t, dir)

	// Deterministic state restored: the same payload now commits as a group.
	result, err := runEdit(ctx, editInput{Payload: twoFilePayload()})
	require.NoError(t, err)
	files = readFiles(t, dir, "a.ts", "b.ts")
	assert.Equal(t, "const endpoint = \"/v2\";\n", files["a.ts"])
	assert.Equal(t, "router.use(\"/v2\", api);\n", files["b.ts"])
	assert.Equal(t, "a.ts, b.ts", result.Detail)
	assertNoLeftovers(t, dir)
}

// An external edit landing in the verify-to-swap window is preserved, not
// overwritten, while already-swapped siblings roll back.
func TestEditInFlightExternalChangeConflictsAndRollsBack(t *testing.T) {
	ctx, dir := toolContext(t)
	writeFiles(t, dir, twoFileFixtures())

	external := "router.use(\"/v9\", external);\n"
	hooks := commitHooks{
		beforeRename: func(abs string, index int) error {
			if index == 1 {
				require.NoError(t, os.WriteFile(abs, []byte(external), 0o644))
			}
			return nil
		},
	}
	_, err := runEditWith(ctx, editInput{Payload: twoFilePayload()}, hooks)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[b.ts]: file changed on disk while the commit was in flight")
	assert.Contains(t, err.Error(), "Rollback kept the externally edited file b.ts untouched")

	files := readFiles(t, dir, "a.ts", "b.ts")
	assert.Equal(t, "const endpoint = \"/v1\";\n", files["a.ts"], "swapped sibling must roll back")
	assert.Equal(t, external, files["b.ts"], "external content must survive")
	assertNoLeftovers(t, dir)
}

// The pre-flight check rejects targets that changed between the anchored read
// and the commit, before any staging file is created.
func TestCommitVerifyRejectsExternalChange(t *testing.T) {
	_, dir := toolContext(t)
	writeFiles(t, dir, map[string]string{"a.ts": "one\n"})
	target := fileTarget{
		abs:     filepath.Join(dir, "a.ts"),
		display: "a.ts",
		raw:     []byte("one\n"),
		next:    []byte("two\n"),
	}
	require.NoError(t, os.WriteFile(target.abs, []byte("external rewrite\n"), 0o644))

	err := verify([]fileTarget{target})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file changed on disk after it was read")
	assert.Contains(t, err.Error(), atomicityNotice)

	assert.Equal(t, "external rewrite\n", readFiles(t, dir, "a.ts")["a.ts"])
	assertNoLeftovers(t, dir)
}

// Different spellings of one file fold into a single target whose merged ops
// all anchor against the original content in one commit.
func TestEditNormalizesPathAliases(t *testing.T) {
	ctx, dir := toolContext(t)
	writeFiles(t, dir, map[string]string{"a.ts": "one\ntwo\nthree\nfour\n"})

	abs := filepath.Join(dir, "a.ts")
	payload := "*** SM:EDIT a.ts\n*** SM:FIND\none\n*** SM:PUT\n1\n" +
		"*** SM:EDIT ./a.ts\n*** SM:FIND\ntwo\n*** SM:PUT\n2\n" +
		"*** SM:EDIT sub/../a.ts\n*** SM:FIND\nthree\n*** SM:PUT\n3\n" +
		"*** SM:EDIT " + abs + "\n*** SM:FIND\nfour\n*** SM:PUT\n4\n"

	result, err := runEdit(ctx, editInput{Payload: payload})
	require.NoError(t, err)
	assert.Equal(t, "1\n2\n3\n4\n", readFiles(t, dir, "a.ts")["a.ts"])
	assert.Equal(t, "a.ts", result.Detail, "aliases must surface as one target")
	assertNoLeftovers(t, dir)
}

// Aliases mix with distinct files without reordering or fusing targets.
func TestEditAliasesMergeAlongsideDistinctFiles(t *testing.T) {
	ctx, dir := toolContext(t)
	writeFiles(t, dir, map[string]string{
		"a.ts": "apple\napricot\n",
		"b.ts": "banana\n",
	})

	payload := "*** SM:EDIT a.ts\n*** SM:FIND\napple\n*** SM:PUT\nAPPLE\n" +
		"*** SM:EDIT ./a.ts\n*** SM:FIND\napricot\n*** SM:PUT\nAPRICOT\n" +
		"*** SM:EDIT b.ts\n*** SM:FIND\nbanana\n*** SM:PUT\nBANANA\n"
	result, err := runEdit(ctx, editInput{Payload: payload})
	require.NoError(t, err)

	files := readFiles(t, dir, "a.ts", "b.ts")
	assert.Equal(t, "APPLE\nAPRICOT\n", files["a.ts"])
	assert.Equal(t, "BANANA\n", files["b.ts"])
	assert.Equal(t, "a.ts, b.ts", result.Detail)
}

// A failed alias payload still rejects atomically: an anchor miss in the
// second spelling writes nothing even though the first spelling matched.
func TestEditAliasAnchorFailureWritesNothing(t *testing.T) {
	ctx, dir := toolContext(t)
	writeFiles(t, dir, map[string]string{"a.ts": "one\ntwo\n"})

	payload := "*** SM:EDIT a.ts\n*** SM:FIND\none\n*** SM:PUT\n1\n" +
		"*** SM:EDIT ./a.ts\n*** SM:FIND\nmissing\n*** SM:PUT\n2\n"
	_, err := runEdit(ctx, editInput{Payload: payload})
	require.Error(t, err)
	assert.Contains(t, err.Error(), atomicityNotice)
	assert.Equal(t, "one\ntwo\n", readFiles(t, dir, "a.ts")["a.ts"])
	assertNoLeftovers(t, dir)
}

func TestJournalDirIsStablePerWorkingTree(t *testing.T) {
	dir := t.TempDir()
	first := journalPath(t, dir)
	second := journalPath(t, dir)
	assert.Equal(t, first, second, "the same cwd must always map to the same journal")
	assert.Contains(t, first, os.TempDir())

	other := journalPath(t, t.TempDir())
	assert.NotEqual(t, first, other)
}

// reconcile with no journal is a quiet no-op so ordinary calls pay nothing.
func TestReconcileWithoutJournalIsNoop(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, newCommitter(dir, commitHooks{}).reconcile())
}

// A journal directory without a journal file is pre-flush crash debris: no
// swap could have landed, so reconcile discards it instead of wedging the
// next commit on an existing directory.
func TestReconcileDiscardsDebrisWithoutJournalFile(t *testing.T) {
	dir := t.TempDir()
	c := newCommitter(dir, commitHooks{})
	jDir := c.journalDir()
	require.NoError(t, os.MkdirAll(jDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(jDir, "orig-0"), []byte("x"), 0o600))

	require.NoError(t, c.reconcile())
	_, err := os.Stat(jDir)
	assert.True(t, os.IsNotExist(err))
}

// A journal marked committed only lost its cleanup: the post-edit state is
// intended, so reconcile removes the journal and touches nothing else.
func TestReconcileCommittedJournalOnlyCleansUp(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.ts": "new\n"})
	c := newCommitter(dir, commitHooks{})
	jDir := c.journalDir()
	require.NoError(t, os.MkdirAll(jDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(jDir, "orig-0"), []byte("old\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(jDir, "next-0"), []byte("new\n"), 0o600))
	state := &journalState{
		Version:   journalVersion,
		CWD:       dir,
		Committed: true,
		Entries: []journalEntry{{
			Path: filepath.Join(dir, "a.ts"),
			Orig: filepath.Join(jDir, "orig-0"),
			Next: filepath.Join(jDir, "next-0"),
			Done: true,
		}},
	}
	require.NoError(t, writeJournal(jDir, state))

	require.NoError(t, c.reconcile())
	assert.Equal(t, "new\n", readFiles(t, dir, "a.ts")["a.ts"])
	_, err := os.Stat(jDir)
	assert.True(t, os.IsNotExist(err))
}

// reconcile restores a swapped entry from its backup, deletes an unswapped
// staging temp, and leaves externally modified targets alone while reporting
// the conflict.
func TestReconcileRestoresAndPreservesExternalEdits(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.ts": "a-new\n",
		"b.ts": "b-external\n",
	})
	c := newCommitter(dir, commitHooks{})
	jDir := c.journalDir()
	require.NoError(t, os.MkdirAll(jDir, 0o700))

	aTmp := filepath.Join(dir, stagingPrefix+"aaaa")
	bTmp := filepath.Join(dir, stagingPrefix+"bbbb")
	require.NoError(t, os.WriteFile(aTmp, []byte("a-new\n"), 0o644))
	require.NoError(t, os.WriteFile(bTmp, []byte("b-new\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(jDir, "orig-0"), []byte("a-old\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(jDir, "next-0"), []byte("a-new\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(jDir, "orig-1"), []byte("b-old\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(jDir, "next-1"), []byte("b-new\n"), 0o600))
	state := &journalState{
		Version: journalVersion,
		CWD:     dir,
		Entries: []journalEntry{
			{
				Path: filepath.Join(dir, "a.ts"),
				Tmp:  aTmp,
				Orig: filepath.Join(jDir, "orig-0"),
				Next: filepath.Join(jDir, "next-0"),
				Done: true,
			},
			{
				Path: filepath.Join(dir, "b.ts"),
				Tmp:  bTmp,
				Orig: filepath.Join(jDir, "orig-1"),
				Next: filepath.Join(jDir, "next-1"),
				Done: false,
			},
		},
	}
	require.NoError(t, writeJournal(jDir, state))

	err := c.reconcile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a.ts restored to pre-edit content")
	assert.Contains(t, err.Error(), "b.ts")
	assert.Contains(t, err.Error(), "left untouched")

	files := readFiles(t, dir, "a.ts", "b.ts")
	assert.Equal(t, "a-old\n", files["a.ts"], "swapped entry must restore its backup")
	assert.Equal(t, "b-external\n", files["b.ts"], "external rewrite must be preserved")
	_, err = os.Stat(aTmp)
	assert.True(t, os.IsNotExist(err), "staging temps must be cleaned")
	_, err = os.Stat(bTmp)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(jDir)
	assert.True(t, os.IsNotExist(err))

	// A second reconcile is a quiet no-op: the deterministic state holds.
	require.NoError(t, newCommitter(dir, commitHooks{}).reconcile())
}

// A successful group commit leaves no journal and returns success only after
// every target is swapped.
func TestEditSuccessCleansJournalAndReportsDiffs(t *testing.T) {
	ctx, dir := toolContext(t)
	writeFiles(t, dir, twoFileFixtures())

	result, err := runEdit(ctx, editInput{Payload: twoFilePayload()})
	require.NoError(t, err)
	assert.Contains(t, result.Content, "-const endpoint = \"/v1\";")
	assert.Contains(t, result.Content, "+const endpoint = \"/v2\";")
	assert.Contains(t, result.Content, "-router.use(\"/v1\", api);")
	assert.Contains(t, result.Content, "+router.use(\"/v2\", api);")
	assertNoLeftovers(t, dir)
}
