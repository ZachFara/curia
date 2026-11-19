package store

import (
	"context"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// exportSameVersion is true for a row whose fields the Obsidian note is
// built from (and that can change once a decision is decided) equal those
// of the decision written: status, scope, answer, reasoning, answered_at,
// superseded_at and superseded_by. It takes eight arguments in that order;
// IS compares NULL times. The other note fields never change after
// creation.
const exportSameVersion = `status = ? AND scope = ? AND answer_key = ? AND answer = ? AND answer_reasoning = ?
  AND answered_at IS ? AND superseded_at IS ? AND superseded_by = ?`

func exportVersionArgs(d model.Decision) []any {
	return []any{
		string(d.Status), d.Scope, d.AnswerKey, d.Answer, d.AnswerReasoning,
		formatTime(d.AnsweredAt), formatTime(d.SupersededAt), d.SupersededBy,
	}
}

// MarkExportedIf records that the note of written, a decision as it was read
// before its note was built, was written at at, but only when the stored
// decision still has exactly that version (see exportSameVersion), in one
// transaction. It reports whether it marked; false (and no error) means the
// decision changed or is gone, and it stays due. A mark also lifts the
// decision's export skip.
func (s *Store) MarkExportedIf(ctx context.Context, written model.Decision, at time.Time) (bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return false, fail("mark exported", written.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	args := append([]any{formatTime(at), written.ID}, exportVersionArgs(written)...)
	res, err := tx.ExecContext(ctx, `UPDATE decisions SET exported_at = ? WHERE id = ? AND `+exportSameVersion, args...)
	if err != nil {
		return false, fail("mark exported", written.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fail("mark exported", written.ID, err)
	}
	if n == 0 {
		return false, nil
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, exportSkipPrefix+written.ID); err != nil {
		return false, fail("mark exported", written.ID, err)
	}
	if err = tx.Commit(); err != nil {
		return false, fail("mark exported", written.ID, err)
	}
	return true, nil
}

// exportSkipPrefix starts the meta key of a decision's export skip; the
// value is the skip time (timeLayout, 30 bytes), "|" and the scope the
// decision had. No table of its own: a skip is only a hint to the sweep.
const exportSkipPrefix = "export_skip:"

// MarkExportSkipped remembers at at that the note of decision d was left
// alone because a foreign file holds its path, so ExportQueue moves past it.
// Like MarkExportedIf it acts only when the stored decision still has d's
// version, and reports whether it did. A new answer, a supersession or a
// rescope makes the decision due again; ExportQueueWith can list it anyway.
func (s *Store) MarkExportSkipped(ctx context.Context, d model.Decision, at time.Time) (bool, error) {
	args := append([]any{exportSkipPrefix + d.ID, at.UTC().Format(timeLayout), d.ID}, exportVersionArgs(d)...)
	res, err := s.db.ExecContext(ctx, `
INSERT INTO meta (key, value)
SELECT ?, ? || '|' || scope FROM decisions WHERE id = ? AND `+exportSameVersion+`
ON CONFLICT (key) DO UPDATE SET value = excluded.value`, args...)
	if err != nil {
		return false, fail("mark export skipped", d.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fail("mark export skipped", d.ID, err)
	}
	return n > 0, nil
}
