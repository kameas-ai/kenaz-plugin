// Package memimport brings notes over from another memory tool's database, so that
// someone moving to Kenaz doesn't start empty. It reads that database read-only, takes
// what lasts (decisions, plus recent fixes and summaries), and never touches its
// prompts or its queue. What it imports is automatic, untrusted and scrubbed like
// anything else captured, and anything that looks like a credential or holds an email
// address stays behind.
package memimport

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/capture"
)

// DefaultDays is how far back fixes and summaries go. There is no fixed cap on how much
// comes over: Fleet's limits vary, the caller reads them (RoomFor), and a note over one
// simply stays on this device.
const DefaultDays = 30

// RoomFor is the budget for an import given what Fleet says is stored and allowed: half
// of what's free, leaving the rest to what comes next, since steady capture can add
// hundreds of notes a day. A limit Fleet doesn't report is no limit.
func RoomFor(records, maxRecords int, bytes, maxBytes int64) (int, int64) {
	notes, size := 0, int64(0)
	if maxRecords > 0 {
		notes = max((maxRecords-records)/2, 1)
	}
	if maxBytes > 0 {
		size = max((maxBytes-bytes)/2, 1)
	}
	return notes, size
}

// What an import takes when Kameas Fleet can't say how much room there is: half of what
// its default limits allow an empty account.
const (
	NoAnswerNotes       = 10_000
	NoAnswerBytes int64 = 25 << 20
)

// Tag and source of imported notes.
const (
	ImportedTag = "imported"

	maxTitleRunes = 200
)

// Options are one import's choices.
type Options struct {
	// DB is the database to read.
	DB string
	// Days is how far back fixes and summaries go. Decisions have no limit.
	Days int
	// MaxNotes and MaxBytes bound what is imported. Zero is no bound.
	MaxNotes int
	MaxBytes int64
	// Roots are directories whose repositories name projects: each one, and each
	// directory directly inside it.
	Roots []string
	// Home is removed from paths.
	Home string
	Now  time.Time
}

func (o *Options) defaults() {
	if o.Days <= 0 {
		o.Days = DefaultDays
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
}

// Note is one note ready to save.
type Note struct {
	Auto     memory.AutoNote
	Project  string
	Portable bool
	kind     string
}

// Plan is what an import would do. It holds counts and project names only, so that it
// can be printed.
type Plan struct {
	Decisions int   `json:"decisions"`
	Bugfixes  int   `json:"bugfixes"`
	Summaries int   `json:"summaries"`
	Bytes     int64 `json:"bytes"`
	// Before counts notes imported on an earlier run, even if deleted since. They don't
	// come over again, and aren't in the counts above.
	Before int `json:"before"`
	// Excluded counts notes left behind because they look like they hold a credential
	// or an email address; OverBudget those past the limits.
	ExcludedCredential int `json:"excluded_credential"`
	ExcludedEmail      int `json:"excluded_email"`
	// ExcludedInvalid counts rows that can't be notes: too long, or not text.
	ExcludedInvalid int `json:"excluded_invalid"`
	OverBudget      int `json:"over_budget"`
	// Projects counts notes by the name in the old database. A project with no checkout
	// found stays on this device.
	Projects map[string]ProjectPlan `json:"projects"`
	notes    []Note
}

// ProjectPlan is one project's part of a plan.
type ProjectPlan struct {
	Notes    int  `json:"notes"`
	Portable bool `json:"portable"`
	// Checkout is the folder its notes are filed under, so that a member can see a
	// wrong match. A name alone can't tell two repositories apart.
	Checkout string `json:"checkout,omitempty"`
	// Ambiguous: more than one repository's folder has this name.
	Ambiguous bool `json:"ambiguous,omitempty"`
}

// Total is how many notes the plan imports.
func (p Plan) Total() int { return p.Decisions + p.Bugfixes + p.Summaries }

// open opens the database so that it cannot be changed through this handle.
func open(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(3000)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// row is one note in the old database. The prompts table, the queue, each session's
// first prompt, the summaries' request column and the observations' raw text column
// are not read at all: a query names every column it takes.
type row struct {
	id                                                 int64
	kind, project, session                             string
	title, subtitle, narrative, facts, read, modified  string
	investigated, learned, completed, nextSteps, notes string
	epoch, tokens                                      int64
}

const (
	observationColumns = `id, type, coalesce(project,''), coalesce(memory_session_id,''), coalesce(title,''), coalesce(subtitle,''), coalesce(narrative,''), coalesce(facts,''), coalesce(files_read,''), coalesce(files_modified,''), created_at_epoch, coalesce(discovery_tokens,0)`
	summaryColumns     = `id, coalesce(project,''), coalesce(memory_session_id,''), coalesce(investigated,''), coalesce(learned,''), coalesce(completed,''), coalesce(next_steps,''), coalesce(notes,''), coalesce(files_read,''), coalesce(files_edited,''), created_at_epoch, coalesce(discovery_tokens,0)`
)

func readObservations(ctx context.Context, db *sql.DB, where string, args ...any) ([]row, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+observationColumns+` FROM observations WHERE `+where+` ORDER BY created_at_epoch DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.kind, &r.project, &r.session, &r.title, &r.subtitle, &r.narrative, &r.facts, &r.read, &r.modified, &r.epoch, &r.tokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func readSummaries(ctx context.Context, db *sql.DB, since int64) ([]row, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+summaryColumns+` FROM session_summaries WHERE created_at_epoch >= ? ORDER BY created_at_epoch DESC, id DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		r := row{kind: "summary"}
		if err := rows.Scan(&r.id, &r.project, &r.session, &r.investigated, &r.learned, &r.completed, &r.nextSteps, &r.notes, &r.read, &r.modified, &r.epoch, &r.tokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Prepare reads the database and works out what an import would do, writing nothing.
func Prepare(ctx context.Context, o Options) (Plan, error) {
	o.defaults()
	plan := Plan{Projects: map[string]ProjectPlan{}}
	db, err := open(o.DB)
	if err != nil {
		return plan, err
	}
	defer db.Close()
	since := o.Now.AddDate(0, 0, -o.Days).UnixMilli()
	// Decisions first, however old: they are what lasts. Then fixes and summaries,
	// newest first.
	first, err := readObservations(ctx, db, `type = 'decision'`)
	if err != nil {
		return plan, fmt.Errorf("this database has an unexpected layout: %w", err)
	}
	then, err := readObservations(ctx, db, `type = 'bugfix' AND created_at_epoch >= ?`, since)
	if err != nil {
		return plan, fmt.Errorf("this database has an unexpected layout: %w", err)
	}
	sums, err := readSummaries(ctx, db, since)
	if err != nil {
		return plan, fmt.Errorf("this database has an unexpected layout: %w", err)
	}
	then = append(then, sums...)
	sort.SliceStable(then, func(i, j int) bool { return then[i].epoch > then[j].epoch })
	projects := findProjects(o.Roots)
	for _, r := range append(first, then...) {
		n, why := r.note(o.Home, projects)
		switch why {
		case "credential":
			plan.ExcludedCredential++
			continue
		case "email":
			plan.ExcludedEmail++
			continue
		case "empty":
			continue
		}
		// What a save would refuse is left out now, so that the dry run tells the truth.
		check := memory.NewNote{ProjectKey: n.Auto.ProjectKey, ProjectRoot: n.Auto.ProjectRoot, Title: n.Auto.Title, Body: n.Auto.Body, Tags: n.Auto.Tags, Source: n.Auto.Source}
		if err := check.Validate(); err != nil {
			if errors.Is(err, memory.ErrLooksSecret) {
				plan.ExcludedCredential++
			} else {
				plan.ExcludedInvalid++
			}
			continue
		}
		size := int64(len(n.Auto.Title) + len(n.Auto.Body))
		if (o.MaxNotes > 0 && plan.Total() >= o.MaxNotes) || (o.MaxBytes > 0 && plan.Bytes+size > o.MaxBytes) {
			plan.OverBudget++
			continue
		}
		plan.Bytes += size
		switch r.kind {
		case "decision":
			plan.Decisions++
		case "bugfix":
			plan.Bugfixes++
		default:
			plan.Summaries++
		}
		pp := plan.Projects[r.project]
		pp.Notes++
		pp.Portable = n.Portable
		pp.Ambiguous = projects.ambiguous[r.project]
		pp.Checkout = n.Auto.ProjectRoot
		plan.Projects[r.project] = pp
		n.kind = r.kind
		plan.notes = append(plan.notes, n)
	}
	return plan, nil
}

// LeaveOutImported takes out what an earlier run imported, from the store's own record of
// that, so that the plan says what will really happen. It reads the store only.
func (p *Plan) LeaveOutImported(ctx context.Context, st *memory.Store) error {
	kept := p.notes[:0:0]
	for _, n := range p.notes {
		_, done, err := st.AppliedResult(ctx, n.Auto.RequestKey)
		if err != nil {
			return err
		}
		if !done {
			kept = append(kept, n)
			continue
		}
		p.Before++
		p.Bytes -= int64(len(n.Auto.Title) + len(n.Auto.Body))
		switch n.kind {
		case "decision":
			p.Decisions--
		case "bugfix":
			p.Bugfixes--
		default:
			p.Summaries--
		}
		if pp := p.Projects[n.Project]; pp.Notes <= 1 {
			delete(p.Projects, n.Project)
		} else {
			pp.Notes--
			p.Projects[n.Project] = pp
		}
	}
	p.notes = kept
	return nil
}

// Result is what an import did.
type Result struct {
	Saved int
	// Before counts notes imported on an earlier run, even if deleted since: an import
	// never brings back a note its member deleted.
	Before int
	// Skipped counts notes the store refused: nothing about one note stops the rest.
	Skipped int
}

// Apply saves a plan's notes.
func Apply(ctx context.Context, st *memory.Store, plan Plan) (Result, error) {
	return ApplyEach(ctx, st, plan, nil)
}

// ApplyEach is Apply, reporting its progress after each note.
func ApplyEach(ctx context.Context, st *memory.Store, plan Plan, progress func(done, total int)) (Result, error) {
	var res Result
	for i, n := range plan.notes {
		_, dup, err := st.SaveAuto(ctx, n.Auto)
		switch {
		case errors.Is(err, memory.ErrLooksSecret), errors.Is(err, memory.ErrInvalidInput):
			res.Skipped++
		case err != nil:
			return res, err
		case dup:
			res.Before++
		default:
			res.Saved++
		}
		if progress != nil {
			progress(i+1, len(plan.notes))
		}
	}
	return res, nil
}

func (r row) note(home string, projects foundProjects) (Note, string) {
	var clean scrubbed
	clean.home = home
	key, root, portable := r.projectKey(projects)
	an := memory.AutoNote{ProjectKey: key, ProjectRoot: root, Source: memory.SourceImport, CreatedAt: time.UnixMilli(r.epoch)}
	tags := []string{memory.AutoTag, ImportedTag, capture.AutoSessionTagPrefix + "import-" + short(r.session), "work:" + strconv.FormatInt(r.tokens, 10)}
	var fallback string
	if r.kind == "summary" {
		an.Kind = memory.KindSessionSummary
		an.RequestKey = "import-other:summary:" + strconv.FormatInt(r.id, 10)
		an.Body = capture.SummaryBody(clean.text(r.investigated), clean.text(r.learned), clean.text(r.completed), clean.text(r.nextSteps), clean.text(r.notes))
		tags = append(tags, "type:summary")
		fallback = firstLine(an.Body)
	} else {
		an.Kind = memory.KindObservation
		an.RequestKey = "import-other:observation:" + strconv.FormatInt(r.id, 10)
		tags = append(tags, "type:"+r.kind)
		an.Title = clean.text(r.title)
		an.Body = (capture.Observation{Type: r.kind, Subtitle: clean.text(r.subtitle), Narrative: clean.text(r.narrative),
			Facts: clean.list(r.facts), FilesRead: clean.list(r.read), FilesModified: clean.list(r.modified)}).Body()
		fallback = clean.text(r.subtitle)
	}
	an.Tags = tags
	switch {
	case clean.credential || capture.LooksSensitive(an.Title, an.Body) || memory.LooksLikeSecret(an.Title+"\n"+an.Body):
		return Note{}, "credential"
	case clean.email:
		return Note{}, "email"
	}
	if strings.TrimSpace(an.Title) == "" {
		an.Title = fallback
	}
	an.Title = clipRunes(strings.TrimSpace(an.Title), maxTitleRunes)
	if an.Title == "" || strings.TrimSpace(an.Body) == "" {
		return Note{}, "empty"
	}
	return Note{Auto: an, Project: r.project, Portable: portable}, ""
}

// scrubbed runs the same scrub as capture over each field, and notes what it finds.
type scrubbed struct {
	home              string
	credential, email bool
}

func (c *scrubbed) text(s string) string {
	if s == "" {
		return ""
	}
	if capture.HasEmail(s) {
		c.email = true
	}
	out, n := capture.Scrub(s, c.home)
	if n > 0 {
		c.credential = true
	}
	return strings.TrimSpace(out)
}

// list reads a list stored as a JSON array, or as plain text.
func (c *scrubbed) list(s string) []string {
	if s == "" {
		return nil
	}
	var items []string
	if json.Unmarshal([]byte(s), &items) != nil {
		items = []string{s}
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if t := c.text(it); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func short(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// projectKey files a note under the repository with its project's name, or, with none
// found, under a key of its own that never leaves this device.
func (r row) projectKey(projects foundProjects) (key, root string, portable bool) {
	if p, ok := projects.byName[r.project]; ok && !projects.ambiguous[r.project] {
		return p.Key, p.Root, memory.IsPortableKey(p.Key)
	}
	return memory.ProjectKey("import:" + r.project), "", false
}

// foundProjects are the repositories found under the roots, by folder name. A name two
// different repositories share says nothing about which one a note came from.
type foundProjects struct {
	byName    map[string]memory.Project
	ambiguous map[string]bool
}

// findProjects maps directory names to their projects, for the roots given: each root,
// and each directory directly inside it.
func findProjects(roots []string) foundProjects {
	out := foundProjects{byName: map[string]memory.Project{}, ambiguous: map[string]bool{}}
	add := func(dir string) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			return
		}
		p, err := memory.ProjectFor(dir)
		if err != nil {
			return
		}
		name := filepath.Base(dir)
		if had, ok := out.byName[name]; ok && had.Key != p.Key {
			out.ambiguous[name] = true
			return
		}
		out.byName[name] = p
	}
	seen := map[string]bool{}
	for _, root := range roots {
		if !seen[root] {
			seen[root] = true
			add(root)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if d := filepath.Join(root, e.Name()); e.IsDir() && !seen[d] {
				seen[d] = true
				add(d)
			}
		}
	}
	return out
}

// Counts are one database's activity since a time: counts only.
type Counts struct {
	Observations int   `json:"observations"`
	Summaries    int   `json:"summaries"`
	WorkTokens   int64 `json:"work_tokens"`
}

// CountSince counts a database's notes since a time. It reads no note's text.
func CountSince(ctx context.Context, path string, since time.Time) (Counts, error) {
	var c Counts
	db, err := open(path)
	if err != nil {
		return c, err
	}
	defer db.Close()
	ms := since.UnixMilli()
	if err := db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(discovery_tokens),0) FROM observations WHERE created_at_epoch >= ?`, ms).Scan(&c.Observations, &c.WorkTokens); err != nil {
		return c, err
	}
	var more int64
	if err := db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(discovery_tokens),0) FROM session_summaries WHERE created_at_epoch >= ?`, ms).Scan(&c.Summaries, &more); err != nil {
		return c, err
	}
	c.WorkTokens += more
	return c, nil
}
