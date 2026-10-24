package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// HLC is a hybrid logical clock value in Fleet's wire form
// "<16-digit unix ms>:<6-digit counter>:<device>". Plain string comparison
// is the total order.
type HLC string

var hlcRe = regexp.MustCompile(`^(\d{16}):(\d{6}):([A-Za-z0-9._-]{1,64})$`)

// FormatHLC renders an HLC.
func FormatHLC(wallMS int64, counter int, device string) HLC {
	return HLC(fmt.Sprintf("%016d:%06d:%s", wallMS, counter, device))
}

// Parse returns the wall milliseconds and counter of a well-formed HLC.
func (h HLC) Parse() (wall int64, counter int, ok bool) {
	m := hlcRe.FindStringSubmatch(string(h))
	if m == nil {
		return 0, 0, false
	}
	w, err1 := strconv.ParseInt(m[1], 10, 64)
	c, err2 := strconv.Atoi(m[2])
	return w, c, err1 == nil && err2 == nil
}

// MaxClockSkew is how far ahead of the local clock a remote HLC may be and
// still advance this profile's clock. Fleet rejects writes more than five
// minutes ahead of its own time (clock_in_future).
const MaxClockSkew = 5 * time.Minute

// tickHLC advances the profile clock for one local mutation inside tx and
// returns the new value. The clock never goes backwards: a later local
// mutation always orders after everything this profile has seen.
func tickHLC(ctx context.Context, tx *sql.Tx, now time.Time, device string) (HLC, error) {
	var last string
	if err := tx.QueryRowContext(ctx, `SELECT last_hlc FROM sync_state WHERE singleton = 1`).Scan(&last); err != nil {
		return "", err
	}
	wall := now.UnixMilli()
	counter := 0
	// A last value far in the future (the device clock jumped ahead and
	// back) is not kept: Fleet refused those writes (clock_in_future), so
	// they never compete, and staying monotonic past them would refuse
	// every later write too.
	if lw, lc, ok := HLC(last).Parse(); ok && lw >= wall && lw <= now.Add(MaxClockSkew).UnixMilli() {
		wall, counter = lw, lc+1
		if counter > 999999 {
			wall, counter = lw+1, 0
		}
	}
	h := FormatHLC(wall, counter, device)
	_, err := tx.ExecContext(ctx, `UPDATE sync_state SET last_hlc = ? WHERE singleton = 1`, string(h))
	return h, err
}

// observeHLC advances the profile clock past a remote HLC (pull). Values
// more than MaxClockSkew ahead of now are ignored so one device's wrong
// clock cannot drag this profile into Fleet's clock_in_future refusals.
func observeHLC(ctx context.Context, tx *sql.Tx, now time.Time, remote string) error {
	rw, rc, ok := HLC(remote).Parse()
	if !ok || rw > now.Add(MaxClockSkew).UnixMilli() {
		return nil
	}
	var last string
	if err := tx.QueryRowContext(ctx, `SELECT last_hlc FROM sync_state WHERE singleton = 1`).Scan(&last); err != nil {
		return err
	}
	lw, lc, lok := HLC(last).Parse()
	if lok && (lw > rw || (lw == rw && lc >= rc)) {
		return nil
	}
	// Keep our own device suffix: the next tick moves past (rw, rc).
	dev := "fleet"
	if m := hlcRe.FindStringSubmatch(last); m != nil {
		dev = m[3]
	}
	_, err := tx.ExecContext(ctx, `UPDATE sync_state SET last_hlc = ? WHERE singleton = 1`, string(FormatHLC(rw, rc, dev)))
	return err
}

// ContentHash is the lowercase hex SHA-256 of content (Fleet's
// content_hash).
func ContentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}
