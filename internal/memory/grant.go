package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// The standing grant for saves. A member says yes once, at a terminal,
// and from then on saving a note that syncs needs no approval of its own. Update,
// forget and forget-all are not covered. Turning sync off or forget-all clears
// the grant; the member can revoke it at any time.
const (
	metaSaveGrant       = "save_grant_at"
	metaSaveGrantNotice = "save_grant_notice"
	metaSaveGrantHour   = "save_grant_hour"
	metaSaveGrantCount  = "save_grant_count"

	// metaImportUploadThrough is the last row a person let upload outside the hourly cap.
	metaImportUploadThrough = "import_upload_through"

	// MaxGrantedSavesPerHour bounds how many notes a runaway agent can send
	// under the grant. Saves past it wait, on this device, for an approval.
	MaxGrantedSavesPerHour = 300
)

// ErrSyncOffForGrant refuses a grant approved after sync was turned off.
var ErrSyncOffForGrant = errors.New("memory sync is off, so there is nothing for this to apply to")

// Wording for the grant.
const (
	SyncAutoSummary    = "Let new notes sync to your Kameas Fleet account without asking each time"
	SyncAutoImpact     = "Saved notes sync without a separate approval, up to 300 an hour. Edits and deletions still ask. Turn this off with `kenaz memory sync auto off`; turning memory sync off clears it too."
	SyncAutoOnMessage  = "New notes now sync without asking each time. Edits and deletions still ask."
	SyncAutoOffMessage = "New notes go back to asking before they sync."
	SyncAutoNeedsSync  = "Turn on memory sync first (`kenaz memory sync enable`)."
	SaveCapMessage     = "Over the hourly limit for automatic sync: it waits for your approval (`kenaz memory sync approve`)."
)

// AutomaticNotesBuild marks a build with automatic capture, whose grant also covers
// those notes. The app sets it from the build.
var AutomaticNotesBuild = false

// SyncAutoImpactText is what the grant covers.
func SyncAutoImpactText() string {
	if AutomaticNotesBuild {
		return SyncAutoImpact + " Automatic notes sync the same way, including ones already on this device."
	}
	return SyncAutoImpact
}

// GrantSaves records the grant. key is the approval's request key.
func (s *Store) GrantSaves(ctx context.Context, key string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if key != "" {
			if _, ok, err := appliedResult(ctx, tx, key); err != nil || ok {
				return err
			}
		}
		set, err := loadSettings(ctx, tx)
		if err != nil {
			return err
		}
		if !set.Enabled {
			return ErrSyncOffForGrant
		}
		if err := setMeta(ctx, tx, metaSaveGrant, strconv.FormatInt(s.now().UnixMilli(), 10)); err != nil {
			return err
		}
		// The grant holds under this notice only.
		if err := setMeta(ctx, tx, metaSaveGrantNotice, set.ConsentVersion); err != nil {
			return err
		}
		if key != "" {
			return recordApplied(ctx, tx, key, OpSyncAuto, Outcome{Status: OutcomeDone, Profile: "account", Message: SyncAutoOnMessage}, s.now().UnixMilli())
		}
		return nil
	})
}

// AllowImportUpload lets the imported notes already on this device upload without
// counting against the hourly cap. Only a command a person ran in a terminal calls it:
// no tool reaches it, and a note imported afterward has a later row, so it isn't covered.
// Row numbers start over once rows go, so the mark ends with the grant, with a forget-all
// and with a reset.
func (s *Store) AllowImportUpload(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var last sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT max(rowid) FROM records WHERE source = ?`, SourceImport).Scan(&last); err != nil || !last.Valid {
			return err
		}
		return setMeta(ctx, tx, metaImportUploadThrough, strconv.FormatInt(last.Int64, 10))
	})
}

// RevokeSaveGrant ends the grant.
func (s *Store) RevokeSaveGrant(ctx context.Context) error {
	_, err := s.EndSaveGrant(ctx)
	return err
}

// EndSaveGrant ends the grant, and returns how many notes stay queued because Fleet may
// already hold them.
func (s *Store) EndSaveGrant(ctx context.Context) (int, error) {
	var unknown int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		unknown, err = endSaveGrant(ctx, tx)
		return err
	})
	return unknown, err
}

func clearSaveGrant(ctx context.Context, tx *sql.Tx) error {
	_, err := endSaveGrant(ctx, tx)
	return err
}

func endSaveGrant(ctx context.Context, tx *sql.Tx) (int, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key IN (?, ?, ?, ?, ?)`, metaSaveGrant, metaSaveGrantNotice, metaSaveGrantHour, metaSaveGrantCount, metaImportUploadThrough); err != nil {
		return 0, err
	}
	return unqueueAuto(ctx, tx)
}

// StillSendingMessage is added when ending the grant, with sync on, leaves notes queued
// because Fleet may already hold them. It is "" when there are none.
func StillSendingMessage(n int) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1 note may already have reached Kameas Fleet, so it'll finish sending. You can delete it with `kenaz memory forget`."
	}
	return fmt.Sprintf("%d notes may already have reached Kameas Fleet, so they'll finish sending. You can delete any of them with `kenaz memory forget`.", n)
}

// StillQueuedMessage is the same when sync itself stops: nothing more goes until it's back.
func StillQueuedMessage(n int) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1 note may already have reached Kameas Fleet. It'll finish sending if you turn sync back on. You can delete it with `kenaz memory forget`."
	}
	return fmt.Sprintf("%d notes may already have reached Kameas Fleet. They'll finish sending if you turn sync back on. You can delete any of them with `kenaz memory forget`.", n)
}

func setMeta(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SpendSaveGrant counts one save against the hour's cap. granted says the grant
// exists; within says this save is inside the cap.
func (s *Store) SpendSaveGrant(ctx context.Context) (granted, within bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		v, err := readMeta(ctx, tx, metaSaveGrant)
		if err != nil || v == "" {
			return err
		}
		set, err := loadSettings(ctx, tx)
		if err != nil {
			return err
		}
		notice, err := readMeta(ctx, tx, metaSaveGrantNotice)
		if err != nil {
			return err
		}
		if notice != set.ConsentVersion {
			// Given under another notice: it no longer stands.
			return clearSaveGrant(ctx, tx)
		}
		granted = true
		hour := strconv.FormatInt(s.now().Unix()/3600, 10)
		h, err := readMeta(ctx, tx, metaSaveGrantHour)
		if err != nil {
			return err
		}
		n := 0
		if h == hour {
			c, err := readMeta(ctx, tx, metaSaveGrantCount)
			if err != nil {
				return err
			}
			n, _ = strconv.Atoi(c)
		}
		if n >= MaxGrantedSavesPerHour {
			return nil
		}
		within = true
		if err := setMeta(ctx, tx, metaSaveGrantHour, hour); err != nil {
			return err
		}
		return setMeta(ctx, tx, metaSaveGrantCount, strconv.Itoa(n+1))
	})
	return granted, within, err
}
