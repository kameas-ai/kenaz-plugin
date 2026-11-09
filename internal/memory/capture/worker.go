package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// Batching keeps the observer close to real time: a few events at a time, and
// whatever is waiting at the end of a turn.
const (
	batchEvents      = 5
	batchBytes       = 32 << 10
	batchMaxAge      = 30 * time.Second
	summaryMaxNotes  = 40
	retryDelay       = 5 * time.Second
	pauseAfterFails  = 3
	failurePause     = 15 * time.Minute
	workerPoll       = 5 * time.Second
	workerIdleExit   = 2 * time.Minute
	workerMaxRuntime = 6 * time.Hour
)

var errLocked = errors.New("capture worker already running")

// AutoSessionTagPrefix prefixes the tag that ties a note to its session.
const AutoSessionTagPrefix = "session:"

// Worker turns queued events into notes. It is one process at a time, kept
// so by a lock.
type Worker struct {
	Paths Paths
	// Settings reads the current settings. They are read again on each
	// pass, so turning capture off takes effect while the worker runs.
	Settings func() Settings
	// Open opens the active profile's store, and names that profile.
	Open func(ctx context.Context) (*memory.Store, string, error)
	// Offered says whether capture may run for the active profile, which it names:
	// signed in, Kameas Fleet must offer it to the organization
	// (memory_capture_kinds); signed out, only a writer that reaches no one but the
	// member's own provider, turned on signed out. Capture fails closed on it: it
	// runs only on a yes no older than offerFresh and for the profile that yes was
	// for; nil, or any other answer, stops it and clears what waits. reason is a
	// Pause code when the answer is no.
	Offered func(ctx context.Context) (offered bool, reason, profile string)
	// OrgAllows reports whether the member's organization allows capture. An off
	// that was heard stays in force until a positive answer. Never having heard
	// anything (signed out, say) leaves capture running.
	OrgAllows func(ctx context.Context) (allowed, known bool)
	// HostCLIAllowed is the remote switch for WriterHostCLI. That writer fails
	// closed: it runs only on a positive answer no older than hostCLIFresh, and an
	// off that was heard stays in force until a positive answer.
	HostCLIAllowed func(ctx context.Context) (allowed, known bool)
	// HostCLIUnverifiedOK lets the host CLI writer run without that answer: only a
	// local trial build, which cannot reach Fleet, sets it.
	HostCLIUnverifiedOK bool
	// NewSummarizer is NewSummarizer unless a test replaces it.
	NewSummarizer func(Settings) Summarizer
	Home          string
	Now           func() time.Time
	Sleep         func(ctx context.Context, d time.Duration)
	// Poll and Idle replace workerPoll and workerIdleExit in tests.
	Poll, Idle time.Duration
	// force sends every waiting event now, ready or not (Flush).
	force bool
	// asked is each session's current request, kept in memory only, for the
	// summary at the end of the turn.
	asked map[string]string
	// askedAt is when each of those was asked.
	askedAt map[string]int64
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) sleep(ctx context.Context, d time.Duration) {
	if w.Sleep != nil {
		w.Sleep(ctx, d)
		return
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// Run processes the queue until it stays empty, or ctx ends. A second worker
// returns at once. No failure in here can reach a session: the worker is a
// separate process, and what it cannot process it deletes.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.Paths.ensure(); err != nil {
		return err
	}
	release, err := tryLock(w.Paths.lock())
	if errors.Is(err, errLocked) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	poll, idle := w.Poll, w.Idle
	if poll <= 0 {
		poll = workerPoll
	}
	if idle <= 0 {
		idle = workerIdleExit
	}
	started, lastWork := w.now(), w.now()
	for ctx.Err() == nil && w.now().Sub(started) < workerMaxRuntime {
		work, pending := w.Pass(ctx)
		if !w.Settings().Background() {
			return nil // the session-agent writer only needs its policy check
		}
		if !LoadState(w.Paths).Offered(w.now()) {
			return nil // nothing runs until Fleet says it may; a later hook asks again
		}
		if work {
			lastWork = w.now()
		}
		if !pending && w.now().Sub(lastWork) >= idle {
			return nil
		}
		if !work {
			w.sleep(ctx, poll)
		}
	}
	return nil
}

// Pass makes one sweep of the queue. work reports that something was
// processed, pending that events still wait.
func (w *Worker) Pass(ctx context.Context) (work, pending bool) {
	s := w.Settings()
	if !s.Active() {
		// Turned off while running: what waits goes with it.
		_ = PurgeQueue(w.Paths)
		return false, false
	}
	now := w.now()
	st := LoadState(w.Paths)
	checked := st.SwitchesCheckedAt
	if w.checkSwitches(ctx, s, &st, now) {
		_ = PurgeQueue(w.Paths)
		_ = saveState(w.Paths, st)
		return false, false
	}
	if st.SwitchesCheckedAt != checked {
		// Kept now: a pass that finds nothing to do would lose it, and ask again.
		_ = saveState(w.Paths, st)
	}
	if !s.Background() {
		// The session's own agent writes the notes. Nothing is queued for it;
		// anything left over came from a writer the member has moved off.
		_ = PurgeQueue(w.Paths)
		return false, false
	}
	items, err := listQueue(w.Paths, now)
	if err != nil {
		return false, false
	}
	if st.Paused(now) {
		// While paused nothing is processed, and nothing raw waits for it.
		if len(items) > 0 {
			removeQueued(w.Paths, items)
			for range items {
				noteDropped(w.Paths)
			}
		}
		return false, false
	}
	if len(items) == 0 {
		return false, false
	}
	store, profile, err := w.Open(ctx)
	if err != nil {
		w.dropBatch(&st, items, FailStore, now)
		_ = saveState(w.Paths, st)
		return true, false
	}
	defer store.Close()
	if st.OfferedFor != profile {
		// The yes was for another profile: a sign-in or sign-out since. What waits
		// isn't processed, and Fleet is asked again for this one.
		removeQueued(w.Paths, items)
		for range items {
			noteDropped(w.Paths)
		}
		st.OfferedAt, st.OfferedFor, st.SwitchesCheckedAt = 0, "", 0
		_ = saveState(w.Paths, st)
		return true, false
	}
	if st.ProfileKey != "" && st.ProfileKey != profile {
		// The account changed since these were queued: they are not this
		// account's to keep.
		removeQueued(w.Paths, items)
		for range items {
			noteDropped(w.Paths)
		}
		st.ProfileKey = profile
		_ = saveState(w.Paths, st)
		return true, false
	}
	st.ProfileKey, st.Writer = profile, s.Writer
	// A forget-all clears the queue, but an event a hook was writing at that moment can
	// land afterwards. It doesn't reach a model, and neither does a request held from
	// before it.
	if items = w.dropErased(items, store.EraseCutoffNow(ctx)); len(items) == 0 {
		_ = saveState(w.Paths, st)
		return true, false
	}

	sum := w.summarizer(s)
	sessions := map[string][]queued{}
	var order []string
	for _, it := range items {
		if _, ok := sessions[it.ev.Session]; !ok {
			order = append(order, it.ev.Session)
		}
		sessions[it.ev.Session] = append(sessions[it.ev.Session], it)
	}
	for _, id := range order {
		for {
			batch, rest, stop, ready := nextBatch(sessions[id], now, w.force)
			if !ready {
				pending = pending || len(batch) > 0
				break
			}
			sessions[id] = rest
			// A forget-all can land while the last batch was with the model.
			batch = w.dropErased(batch, store.EraseCutoffNow(ctx))
			if len(batch) == 0 {
				work = true
				if len(rest) == 0 {
					break
				}
				continue
			}
			w.processBatch(ctx, sum, store, s, &st, batch, now)
			work = true
			if st.Paused(now) {
				_ = saveState(w.Paths, st)
				return true, true
			}
			if stop && !batchErased(batch, store.EraseCutoffNow(ctx)) {
				w.dropErased(nil, store.EraseCutoffNow(ctx))
				w.summarise(ctx, sum, store, s, &st, batch, now)
				if st.Paused(now) {
					_ = saveState(w.Paths, st)
					return true, true
				}
			}
			if len(rest) == 0 {
				break
			}
		}
	}
	_ = saveState(w.Paths, st)
	return work, pending
}

// dropErased removes what a forget-all covers.
func (w *Worker) dropErased(items []queued, cut memory.EraseCutoff) []queued {
	for id, t := range w.askedAt {
		if cut.Covers(time.UnixMilli(t)) {
			delete(w.asked, id)
			delete(w.askedAt, id)
		}
	}
	kept := items[:0:0]
	var gone []queued
	for _, it := range items {
		if cut.Covers(time.UnixMilli(it.ev.At)) {
			gone = append(gone, it)
		} else {
			kept = append(kept, it)
		}
	}
	if len(gone) > 0 {
		removeQueued(w.Paths, gone)
	}
	return kept
}

func (w *Worker) summarizer(s Settings) Summarizer {
	if w.NewSummarizer != nil {
		return w.NewSummarizer(s)
	}
	return NewSummarizer(s, w.Paths.work())
}

// switchCheckEvery is how often the worker asks Fleet about its off switches.
const switchCheckEvery = 10 * time.Minute

// hostCLIFresh is how old the positive answer the host CLI writer needs may be.
const hostCLIFresh = 24 * time.Hour

// checkSwitches applies the off switches and reports whether capture must stop now.
// What it hears is kept in the state file, so that it survives the worker. Only a
// positive answer clears an off that was heard.
func (w *Worker) checkSwitches(ctx context.Context, s Settings, st *State, now time.Time) (stop bool) {
	// A time in the future says nothing: the clock moved back, or the file was edited.
	if st.SwitchesCheckedAt > now.UnixMilli() {
		st.SwitchesCheckedAt = 0
	}
	if st.HostCLIVerifiedAt > now.UnixMilli() {
		st.HostCLIVerifiedAt = 0
	}
	due := now.Sub(time.UnixMilli(st.SwitchesCheckedAt)) >= switchCheckEvery
	if due {
		// Stamped for the attempt too, so that an outage costs no more calls.
		st.SwitchesCheckedAt = now.UnixMilli()
	}
	if due {
		offered, reason, profile := false, PauseUnconfirmed, ""
		if w.Offered != nil {
			offered, reason, profile = w.Offered(ctx)
		}
		if offered {
			st.OfferedAt, st.OfferCode, st.OfferedFor = now.UnixMilli(), "", profile
		} else {
			if reason == "" {
				reason = PauseUnconfirmed // not knowing is a no
			}
			st.OfferedAt, st.OfferCode, st.OfferedFor = 0, reason, ""
		}
	}
	if due && w.OrgAllows != nil {
		if allowed, known := w.OrgAllows(ctx); known {
			st.OrgOff = !allowed
		}
	}
	if s.Writer == WriterHostCLI && due && w.HostCLIAllowed != nil {
		if allowed, known := w.HostCLIAllowed(ctx); known {
			st.HostCLIOff = !allowed
			if allowed {
				st.HostCLIVerifiedAt = now.UnixMilli()
			}
		}
	}
	if r := st.NotOfferedReason(now); r != "" {
		// Until the next check, not the failure pause: a yes then starts it again.
		st.PausedUntil, st.PauseReason = max(st.SwitchesCheckedAt+switchCheckEvery.Milliseconds(), now.UnixMilli()+1), r
		return true
	}
	if offerPause(st.PauseReason) {
		st.PausedUntil, st.PauseReason = 0, ""
	}
	reason := ""
	switch {
	case st.OrgOff:
		reason = PauseOrgOff
	case s.Writer == WriterHostCLI && st.HostCLIOff:
		reason = PauseHostCLIOff
	case s.Writer == WriterHostCLI && !w.HostCLIUnverifiedOK && now.Sub(time.UnixMilli(st.HostCLIVerifiedAt)) > hostCLIFresh:
		reason = PauseHostCLIUnverified
	}
	if reason == "" {
		return false
	}
	st.PausedUntil, st.PauseReason = now.Add(failurePause).UnixMilli(), reason
	return true
}

// RecordOffered keeps Fleet's positive answer that capture is offered, heard
// outside the worker at opt-in, so the first events of the session aren't lost.
// profile is the memory profile the answer is for.
func RecordOffered(p Paths, now time.Time, profile string) error {
	st := LoadState(p)
	st.OfferedAt, st.OfferCode, st.OfferedFor = now.UnixMilli(), "", profile
	if offerPause(st.PauseReason) {
		st.PausedUntil, st.PauseReason = 0, ""
	}
	return saveState(p, st)
}

// RecordEnabled keeps the profile capture was turned on under.
func RecordEnabled(p Paths, profile string) error {
	st := LoadState(p)
	st.EnabledFor = profile
	return saveState(p, st)
}

// ResetOffer forgets the answer, and clears what waits, when the account changes:
// sign-in and sign-out. Nothing is queued or processed until the worker asks again
// for the new profile.
func ResetOffer(p Paths) error {
	if err := PurgeQueue(p); err != nil {
		return err
	}
	st := LoadState(p)
	if st.OfferedAt == 0 && st.OfferedFor == "" && st.SwitchesCheckedAt == 0 {
		return nil
	}
	st.OfferedAt, st.OfferedFor, st.SwitchesCheckedAt = 0, "", 0
	return saveState(p, st)
}

func offerPause(reason string) bool {
	return reason == PauseNotOffered || reason == PauseUnconfirmed || reason == PauseSignedOutWriter
}

// OfferCheckDue reports that the worker would ask Fleet again now. A hook uses it
// to start the worker while capture isn't offered, no more often than that.
func OfferCheckDue(p Paths, now time.Time) bool {
	st := LoadState(p)
	return st.SwitchesCheckedAt > now.UnixMilli() || now.Sub(time.UnixMilli(st.SwitchesCheckedAt)) >= switchCheckEvery
}

// RecordHostCLIAnswer keeps a positive answer heard outside the worker, at opt-in.
func RecordHostCLIAnswer(p Paths, now time.Time) error {
	st := LoadState(p)
	st.HostCLIOff, st.HostCLIVerifiedAt = false, now.UnixMilli()
	return saveState(p, st)
}

// nextBatch takes the next batch of one session, if it is ready. stop says that
// the batch ends a turn. A batch never runs past a turn's end.
func nextBatch(items []queued, now time.Time, force bool) (batch, rest []queued, stop, ready bool) {
	size, tools, cut := 0, 0, -1
	for i, it := range items {
		size += len(it.ev.Input) + len(it.ev.Output)
		switch it.ev.Kind {
		case eventTool:
			tools++
		case eventStop:
			stop = true
		}
		if stop || tools >= batchEvents || size >= batchBytes {
			cut = i + 1
			break
		}
	}
	if cut < 0 {
		if len(items) == 0 || (!force && now.Sub(time.UnixMilli(items[0].ev.At)) < batchMaxAge) {
			return items, nil, false, false
		}
		cut = len(items)
	}
	return items[:cut], items[cut:], stop, true
}

// processBatch sends one batch to the model and saves what it says. Whatever
// happens, the batch's events are deleted at the end: a failed batch is retried
// once and then lost, never kept.
func (w *Worker) processBatch(ctx context.Context, sum Summarizer, store *memory.Store, s Settings, st *State, batch []queued, now time.Time) {
	defer removeQueued(w.Paths, batch)
	tools := 0
	for _, it := range batch {
		st.Redactions += int64(it.ev.Redactions)
		switch it.ev.Kind {
		case eventTool:
			tools++
		case eventPrompt:
			if w.asked == nil {
				w.asked, w.askedAt = map[string]string{}, map[string]int64{}
			}
			w.asked[it.ev.Session], w.askedAt[it.ev.Session] = it.ev.Input, it.ev.At
		}
	}
	if tools == 0 {
		return // only prompts and turn ends: nothing to observe yet
	}
	if w.overCap(s, *st, now) {
		st.DroppedOverCap += int64(len(batch))
		w.pauseUntilTomorrow(st, now)
		return
	}
	req := Request{System: observeSystem, User: renderBatch(batch, w.Home), Model: s.Model, MaxUSD: remainingUSD(s, *st, now)}
	resp, err := w.call(ctx, sum, req, st, now)
	if err != nil {
		w.fail(st, failCode(err), now)
		return
	}
	obs, dropped, err := parseObservations(resp.Text, w.Home)
	if err != nil {
		w.fail(st, failCode(err), now)
		return
	}
	st.DroppedSecret += int64(dropped)
	first := batch[0].ev
	workTokens := int64(0)
	if len(obs) > 0 {
		workTokens = (resp.Usage.In + resp.Usage.Out) / int64(len(obs))
	}
	for i, o := range obs {
		tags := []string{memory.AutoTag, "type:" + o.Type, AutoSessionTagPrefix + first.Session, "work:" + strconv.FormatInt(workTokens, 10)}
		for _, l := range o.Labels {
			if len(tags) < memory.MaxTags {
				tags = append(tags, "label:"+l)
			}
		}
		_, dup, err := store.SaveAuto(ctx, memory.AutoNote{
			ProjectKey: first.ProjectKey, ProjectRoot: first.Root, Title: o.Title, Body: o.body(), Tags: tags,
			Source: first.Host, Kind: memory.KindObservation, RequestKey: batchKey(batch, i),
			EventsFrom: time.UnixMilli(batchStart(batch)),
		})
		if errors.Is(err, memory.ErrErasedSince) {
			return // erased while the model was writing
		}
		if errors.Is(err, memory.ErrLooksSecret) {
			st.DroppedSecret++
			continue
		}
		if err != nil {
			w.fail(st, FailStore, now)
			return
		}
		if !dup {
			st.Saved++
		}
	}
	st.Batches++
	st.ConsecutiveFailure, st.LastSuccessAt = 0, now.UnixMilli()
}

// batchErased reports a batch whose turn ended before a forget-all.
func batchErased(batch []queued, cut memory.EraseCutoff) bool {
	return cut.Covers(time.UnixMilli(batch[len(batch)-1].ev.At))
}

// batchStart is when a batch's earliest event happened.
func batchStart(batch []queued) int64 {
	first := batch[0].ev.At
	for _, it := range batch {
		if it.ev.At < first {
			first = it.ev.At
		}
	}
	return first
}

// remainingUSD is the ceiling for one call under an optional daily cap, or zero
// when there is no cap.
func remainingUSD(s Settings, st State, now time.Time) float64 {
	if s.DailyUSD <= 0 {
		return 0
	}
	return s.DailyUSD - st.Usage[dayKey(now)].USD
}

// call makes one model call and retries once. Every attempt that returned
// usage counts against the cap; so does a failed attempt, by estimate, since
// the host may have billed it.
func (w *Worker) call(ctx context.Context, sum Summarizer, req Request, st *State, now time.Time) (Response, error) {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			w.sleep(ctx, retryDelay)
			if ctx.Err() != nil {
				break
			}
		}
		resp, err := sum.Complete(ctx, req)
		if err == nil {
			st.addUsage(now, resp.Usage)
			return resp, nil
		}
		last = err
		if noRetry(failCode(err)) {
			break // retrying changes nothing, and nothing was billed
		}
		in := approxTokens(req.System + req.User)
		st.addUsage(now, Usage{In: in, USD: estimateUSD(in, 0)})
	}
	if last == nil {
		last = &Failure{Code: FailTimeout}
	}
	return Response{}, last
}

// noRetry lists failures another attempt cannot fix. Each pauses capture at once.
func noRetry(code string) bool {
	switch code {
	case FailCLIMissing, FailNeedsKey, FailRateLimited, FailRefused:
		return true
	}
	return false
}

func (w *Worker) overCap(s Settings, st State, now time.Time) bool {
	d := st.Usage[dayKey(now)]
	return (s.DailyTokens > 0 && d.Tokens() >= s.DailyTokens) || (s.DailyUSD > 0 && d.USD >= s.DailyUSD)
}

func (w *Worker) pauseUntilTomorrow(st *State, now time.Time) {
	u := now.UTC()
	tomorrow := time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
	st.PausedUntil, st.PauseReason = tomorrow.UnixMilli(), "daily_cap"
}

func (w *Worker) fail(st *State, code string, now time.Time) {
	st.FailedBatches++
	st.ConsecutiveFailure++
	st.LastFailure, st.LastFailureAt = code, now.UnixMilli()
	if noRetry(code) || st.ConsecutiveFailure >= pauseAfterFails {
		st.PausedUntil, st.PauseReason = now.Add(failurePause).UnixMilli(), code
	}
}

func (w *Worker) dropBatch(st *State, items []queued, code string, now time.Time) {
	removeQueued(w.Paths, items)
	w.fail(st, code, now)
}

// summarise writes the summary of a turn that just ended: from the turn's
// notes, what the user asked and the assistant's last message, never from the
// tool log. A turn with no notes has nothing to summarise.
func (w *Worker) summarise(ctx context.Context, sum Summarizer, store *memory.Store, s Settings, st *State, batch []queued, now time.Time) {
	stop := batch[len(batch)-1].ev
	tag := AutoSessionTagPrefix + stop.Session
	notes, err := store.TurnObservations(ctx, tag, summaryMaxNotes)
	if err != nil || len(notes) == 0 || w.overCap(s, *st, now) {
		return
	}
	titles, bodies := make([]string, len(notes)), make([]string, len(notes))
	for i, n := range notes {
		titles[i], bodies[i] = n.Title, n.Body
	}
	asked := w.asked[stop.Session]
	from := stop.At
	if t, ok := w.askedAt[stop.Session]; ok && t < from {
		from = t
	}
	delete(w.asked, stop.Session)
	delete(w.askedAt, stop.Session)
	req := Request{System: summarySystem, User: renderTurn(asked, titles, bodies, stop.Output), Model: s.Model, MaxUSD: remainingUSD(s, *st, now)}
	resp, err := w.call(ctx, sum, req, st, now)
	if err != nil {
		w.fail(st, failCode(err), now)
		return
	}
	sm, secret, err := parseSummary(resp.Text, w.Home)
	if err != nil {
		w.fail(st, failCode(err), now)
		return
	}
	if secret {
		st.DroppedSecret++
		return
	}
	_, dup, err := store.SaveAuto(ctx, memory.AutoNote{
		ProjectKey: stop.ProjectKey, ProjectRoot: stop.Root, Title: sm.Title, Body: sm.body(),
		Tags:   []string{memory.AutoTag, "type:summary", tag, "work:" + strconv.FormatInt(resp.Usage.In+resp.Usage.Out, 10)},
		Source: stop.Host, Kind: memory.KindSessionSummary,
		RequestKey: "capture-summary:" + stop.Session + ":" + strconv.FormatInt(stop.At, 10),
		EventsFrom: time.UnixMilli(from),
	})
	if err == nil && !dup {
		st.Saved++
	}
}

// batchKey identifies one note of one batch, so a batch re-run after a crash
// between saving and deleting saves nothing twice.
func batchKey(batch []queued, i int) string {
	h := sha256.New()
	for _, it := range batch {
		h.Write([]byte(it.name))
	}
	return "capture:" + hex.EncodeToString(h.Sum(nil))[:24] + ":" + strconv.Itoa(i)
}

// flushWait is how long Flush waits for a running worker to let go.
const flushWait = 10 * time.Second

// Flush writes every waiting event with the settings the member opted in to,
// before a move to another writer, so a move loses nothing. It returns once the
// queue is empty or ctx ends. What a failed call could not write is gone, as it
// would be anyway.
func (w *Worker) Flush(ctx context.Context) error {
	if err := w.Paths.ensure(); err != nil {
		return err
	}
	var release func()
	var err error
	deadline := time.Now().Add(flushWait)
	for {
		release, err = tryLock(w.Paths.lock())
		if !errors.Is(err, errLocked) || time.Now().After(deadline) {
			break
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	defer release()
	w.force = true
	defer func() { w.force = false }()
	for ctx.Err() == nil && QueueLen(w.Paths) > 0 {
		if work, _ := w.Pass(ctx); !work {
			break
		}
	}
	return ctx.Err()
}

// SpawnWorker starts a detached worker running bin with args, unless one
// already holds the lock. It costs a lock attempt per hook when a worker is
// running. The worker carries WorkerEnv, as does everything it starts.
func SpawnWorker(p Paths, start func() error) error {
	if err := p.ensure(); err != nil {
		return err
	}
	release, err := tryLock(p.lock())
	if errors.Is(err, errLocked) {
		return nil
	}
	if err != nil {
		return err
	}
	release()
	return start()
}
