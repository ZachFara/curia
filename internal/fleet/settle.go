package fleet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/transcript"
)

// settleAfter is how long a session must have gone without a hook event
// before Settle reads its transcript: Claude Code writes the transcript
// about 200 ms after the entries it records.
const settleAfter = 3 * time.Second

// interruptedUpdate is the LastUpdate of a settled session. Transcript text
// is never stored.
const interruptedUpdate = "interrupted"

// Settle corrects sessions a denied permission prompt or an interrupt left
// working or waiting: no hook fires for either (Ruling 77). Each of the
// given sessions that is working or waiting, whose UpdatedAt is at least 3
// seconds before now, whose transcript after its TranscriptOffset ends in a
// user-interrupt marker (transcript.Interrupted), and whose marker is
// timestamped later than its UpdatedAt (Ruling 79: a marker from before the
// last hook event, such as the UserPromptSubmit of a new prompt whose line
// is not written yet, says nothing about now) is settled: it becomes
// needs_decision when it has an open blocking ticket at now, as a Stop
// would make it, and idle otherwise, and seen, with LastUpdate
// "interrupted" and UpdatedAt and SeenAt now; nothing else changes.
//
// Each write is one store.UpdateSessionTx transaction, which counts the
// open blocking tickets, and is skipped unless the stored row still has
// the listed state and UpdatedAt and the marker is later than that
// UpdatedAt (no hook event arrived since). Sessions in other states, or too
// recent, are not read at all. One session's failure does not stop the
// others: Settle returns the ids it settled and the failures joined.
func Settle(ctx context.Context, st *store.Store, sessions []model.Session, now time.Time) (settled []string, err error) {
	var errs []error
	for _, s := range sessions {
		if !settleCandidate(s, now) {
			continue
		}
		done, err := settleOne(ctx, st, s, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("fleet: settle session %s: %w", s.ID, err))
		}
		if done {
			settled = append(settled, s.ID)
		}
	}
	return settled, errors.Join(errs...)
}

// settleCandidate reports whether s is working or waiting and has had no
// hook event for settleAfter.
func settleCandidate(s model.Session, now time.Time) bool {
	return (s.State == model.StateWorking || s.State == model.StateWaiting) && !now.Before(s.UpdatedAt.Add(settleAfter))
}

// settleOne settles the listed session if its transcript ends in an
// interrupt later than its last hook event and its stored row is still the
// listed one.
func settleOne(ctx context.Context, st *store.Store, listed model.Session, now time.Time) (bool, error) {
	at, interrupted, err := transcript.Interrupted(listed.TranscriptPath, listed.TranscriptOffset)
	if err != nil || !interrupted || !at.After(listed.UpdatedAt) {
		return false, err
	}
	done := false
	err = st.UpdateSessionTx(ctx, listed.ID, func(tx *store.Tx, cur model.Session, _ bool) (model.Session, error) {
		done = false
		// Sessions are never deleted; a missing row has an empty ID, which
		// UpdateSessionTx refuses to write.
		if cur.State != listed.State || !cur.UpdatedAt.Equal(listed.UpdatedAt) || !at.After(cur.UpdatedAt) {
			return cur, nil
		}
		open, cerr := tx.OpenBlocking(ctx, cur.ID, now)
		if cerr != nil {
			return cur, cerr
		}
		cur.State = model.StateIdle
		if open > 0 {
			cur.State = model.StateNeedsDecision // M5: the badge and markers show it
		}
		cur.LastUpdate, cur.SeenAt, cur.UpdatedAt = interruptedUpdate, now, now
		done = true
		return cur, nil
	})
	if err != nil {
		return false, err
	}
	return done, nil
}
