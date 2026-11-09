package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/capture"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/memimport"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

type typedDesk struct {
	t      *testing.T
	f      *fleetapitest.MemoryFleet
	p      *memProvider
	env    Env
	mp     memoryPaths
	spawns int
	out    *bytes.Buffer
	errb   *bytes.Buffer
}

func newTypedDesk(t *testing.T) *typedDesk {
	t.Helper()
	f := fleetapitest.NewMemoryFleet()
	p := &memProvider{Provider: stub.New(), fleet: f}
	p.signIn("alice")
	d := &typedDesk{t: t, f: f, p: p, out: &bytes.Buffer{}, errb: &bytes.Buffer{}}
	d.env = devEnv(t.TempDir(), p, d.out, d.errb)
	d.env.Spawn = func(string, fleetapi.Env) error { d.spawns++; return nil }
	mp, err := d.env.memoryPaths()
	if err != nil {
		t.Fatal(err)
	}
	d.mp = mp
	return d
}

// run drives the command as if from a terminal or not, with this typed input.
func (d *typedDesk) run(terminal bool, input string, args ...string) int {
	d.t.Helper()
	oldT, oldR := memoryStdinTerminal, memoryStdin
	memoryStdinTerminal = func() bool { return terminal }
	memoryStdin = strings.NewReader(input)
	defer func() { memoryStdinTerminal, memoryStdin = oldT, oldR }()
	d.out.Reset()
	d.errb.Reset()
	return runMemory(d.env, append([]string{"sync"}, args...))
}

func (d *typedDesk) save(title string) memory.Outcome {
	d.t.Helper()
	out, err := d.mp.service(nil).Save(context.Background(), memory.NewNote{ProjectKey: memory.GlobalProject, Title: title, Body: "body of " + title, Source: memory.SourceCLI})
	if err != nil {
		d.t.Fatal(err)
	}
	return out
}

func (d *typedDesk) settings() memory.SyncSettings {
	d.t.Helper()
	p := d.mp.active(context.Background())
	st, err := memory.OpenProfile(memory.ProfilePath(d.mp.StateRoot, p), p)
	if err != nil {
		d.t.Fatal(err)
	}
	defer st.Close()
	s, err := st.SyncSettings(context.Background())
	if err != nil {
		d.t.Fatal(err)
	}
	return s
}

func (d *typedDesk) fleetConsent() fleetapi.MemorySettings {
	d.t.Helper()
	s, err := d.f.Client("org-1", "alice").Settings(d.f.ContextFor(context.Background(), "org-1", "alice"))
	if err != nil {
		d.t.Fatal(err)
	}
	return s
}

func (d *typedDesk) lane() *memorySyncLane {
	d.t.Helper()
	l := &memorySyncLane{p: d.p, stateRoot: d.mp.StateRoot, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		holder: "test-relay", caps: &capsCache{p: d.p}, kick: make(chan struct{}, 1), period: 30 * time.Millisecond}
	return l
}

// The typed phrase turns sync on through the same code an approval runs: Fleet records
// the opt-in under the notice shown, the note listed uploads, and a relay is started.
func TestTypedEnableSyncsANote(t *testing.T) {
	d := newTypedDesk(t)
	d.save("canary note")
	if code := d.run(true, "\n", "enable"); code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, d.out, d.errb)
	}
	n := notices.MemorySync()
	out := d.out.String()
	if !strings.Contains(out, n.Text) || !strings.Contains(out, "canary note") || !strings.Contains(out, "Memory sync is on") {
		t.Fatalf("output: %s", out)
	}
	if strings.Contains(out, "approv") || strings.Contains(out, "security key") {
		t.Fatalf("output talks about approval: %s", out)
	}
	if s := d.settings(); !s.Enabled || s.ConsentVersion != n.ID {
		t.Fatalf("store %+v", s)
	}
	if s := d.fleetConsent(); !s.Enabled || s.ConsentVersion != n.ID {
		t.Fatalf("fleet %+v", s)
	}
	if d.spawns == 0 {
		t.Fatal("no relay started")
	}
	if _, err := os.Stat(filepath.Join(d.mp.StateRoot, "dev", "approvals")); !os.IsNotExist(err) {
		t.Fatalf("the approval service was involved: %v", err)
	}
	l := d.lane()
	if err := l.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer l.Stop()
	if !waitUntil(t, func() bool { return len(d.f.LiveContents("org-1", "alice")) == 1 }) {
		t.Fatalf("the note never synced: %+v", l.Status())
	}
}

// Enter, y or yes means yes, in any case.
func TestEnterYOrYesTurnsItOn(t *testing.T) {
	for _, answer := range []string{"", "y", "Y", "yes", "YES", " Yes "} {
		d := newTypedDesk(t)
		if code := d.run(true, answer+"\n", "enable"); code != ExitOK || !d.settings().Enabled {
			t.Errorf("%q: exit %d: %s %s", answer, code, d.out, d.errb)
		}
	}
}

// Anything else changes nothing, says so and records nothing.
func TestTypedEnableWrongAnswerChangesNothing(t *testing.T) {
	for _, answer := range []string{"n", "no", "N", "NO", "nope", "Not now", "1", "ok"} {
		d := newTypedDesk(t)
		d.save("note")
		if code := d.run(true, answer+"\n", "enable"); code != ExitOK {
			t.Fatalf("%q exit %d: %s", answer, code, d.errb)
		}
		if !strings.Contains(d.out.String(), "Nothing changed") {
			t.Errorf("%q: %s", answer, d.out)
		}
		if s := d.settings(); s.Enabled || s.EverEnabled || s.ConsentVersion != "" {
			t.Errorf("%q: store %+v", answer, s)
		}
		if s := d.fleetConsent(); s.Enabled || s.ConsentVersion != "" {
			t.Errorf("%q: fleet %+v", answer, s)
		}
		if d.spawns != 0 {
			t.Errorf("%q: relay started", answer)
		}
	}
	// A closed input isn't an answer either.
	d := newTypedDesk(t)
	d.save("note")
	d.run(true, "", "enable")
	if s := d.settings(); s.Enabled {
		t.Fatalf("%+v", s)
	}
}

// With no terminal it refuses and names the command, even with a yes on stdin.
func TestTypedRefusedWithoutATerminal(t *testing.T) {
	d := newTypedDesk(t)
	d.save("note")
	if code := d.run(false, "\n", "enable"); code != ExitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(d.errb.String(), "memory sync enable") || d.settings().Enabled || d.fleetConsent().Enabled {
		t.Fatalf("%s %+v", d.errb, d.settings())
	}
	if code := d.run(true, "\n", "enable"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, d.errb)
	}
	if code := d.run(false, "\n", "auto", "on"); code != ExitError || !strings.Contains(d.errb.String(), "memory sync auto on") {
		t.Fatalf("exit %d: %s", code, d.errb)
	}
}

// The real check, with /dev/null and a pipe on stdin as a script or agent would have.
func TestTypedRefusesDevNullForReal(t *testing.T) {
	d := newTypedDesk(t)
	d.save("note")
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()
	if code := runMemory(d.env, []string{"sync", "enable"}); code != ExitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(d.errb.String(), "memory sync enable") || d.settings().Enabled {
		t.Fatalf("%s", d.errb)
	}
}

// What is acknowledged is the notice shown, not whatever is current by then.
func TestTypedBindsTheNotice(t *testing.T) {
	d := newTypedDesk(t)
	svc := d.mp.service(nil)
	ctx := context.Background()
	plan, err := svc.PlanTypedEnable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "memory-sync/v0"} {
		if _, err := svc.EnableSyncTyped(ctx, id, plan); err == nil {
			t.Errorf("%q accepted", id)
		}
		if _, err := svc.SetSyncAutoTyped(ctx, id); err == nil {
			t.Errorf("%q accepted for the grant", id)
		}
	}
	if d.fleetConsent().Enabled {
		t.Fatal("reached Fleet")
	}
	// A note changed after it was listed isn't uploaded.
	d.save("listed")
	plan, _ = svc.PlanTypedEnable(ctx)
	if len(plan.Records) != 1 {
		t.Fatalf("%+v", plan)
	}
	plan.Records[0].Hash = "other"
	out, err := svc.EnableSyncTyped(ctx, notices.MemorySync().ID, plan)
	if err != nil || out.Queued != 0 {
		t.Fatalf("%+v %v", out, err)
	}
}

// Fleet saying no leaves this device off too, and its text isn't printed.
func TestTypedFleetRefusalChangesNothing(t *testing.T) {
	d := newTypedDesk(t)
	d.save("note")
	d.env.Provider = noSyncPlan{d.p}
	mp, err := d.env.memoryPaths()
	if err != nil {
		t.Fatal(err)
	}
	d.mp = mp
	if code := d.run(true, "\n", "enable"); code != ExitError {
		t.Fatalf("exit %d: %s", code, d.out)
	}
	if !strings.Contains(d.errb.String(), "capability_not_in_tier") || d.settings().Enabled || d.spawns != 0 {
		t.Fatalf("%s", d.errb)
	}
}

type noSyncPlan struct{ *memProvider }

func (noSyncPlan) Me() fleetapi.Me           { return noSyncMe{} }
func (p noSyncPlan) Memory() fleetapi.Memory { return p.memProvider.Memory() }

type noSyncMe struct{ fleetapi.Me }

func (noSyncMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: map[string]bool{}}, nil
}

// The standing grant: needs sync on, the phrase, then new notes sync with no approval,
// up to the hourly cap, and only under the notice it was given under.
func TestTypedAutoOn(t *testing.T) {
	d := newTypedDesk(t)
	if code := d.run(true, "\n", "auto", "on"); code == ExitOK && !strings.Contains(d.out.String(), "memory sync enable") {
		t.Fatalf("exit %d: %s %s", code, d.out, d.errb)
	}
	d.save("first")
	d.run(true, "\n", "enable")
	if code := d.run(true, "no\n", "auto", "on"); code != ExitOK || !strings.Contains(d.out.String(), "Nothing changed") {
		t.Fatalf("exit %d: %s", code, d.out)
	}
	if st := d.status(); st.SaveGrant {
		t.Fatalf("%+v", st)
	}
	if code := d.run(true, "\n", "auto", "on"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, d.errb)
	}
	if !strings.Contains(d.out.String(), notices.MemorySync().Text) || !strings.Contains(d.out.String(), memory.SyncAutoOnMessage) {
		t.Fatalf("%s", d.out)
	}
	st := d.status()
	if !st.SaveGrant {
		t.Fatalf("%+v", st)
	}
	// A save now needs nobody, within the same cap an approved grant has.
	out, err := d.mp.service(cliSubmitter(d.env)).Save(context.Background(), memory.NewNote{ProjectKey: memory.GlobalProject, Title: "second", Body: "body", Source: memory.SourceCLI})
	if err != nil || out.Status != memory.OutcomeDone {
		t.Fatalf("%+v %v", out, err)
	}
	if out.SyncState != memory.StatePending {
		t.Fatalf("not queued: %+v", out)
	}
	p := d.mp.active(context.Background())
	store, err := memory.OpenProfile(memory.ProfilePath(d.mp.StateRoot, p), p)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	used := 0
	for {
		granted, within, err := store.SpendSaveGrant(context.Background())
		if err != nil || !granted {
			t.Fatalf("%v %v", granted, err)
		}
		if !within {
			break
		}
		if used++; used > memory.MaxGrantedSavesPerHour {
			t.Fatal("no cap")
		}
	}
	// Turning it off needs no terminal.
	if code := d.run(false, "", "auto", "off"); code != ExitOK || d.status().SaveGrant {
		t.Fatalf("exit %d: %s", code, d.errb)
	}
	if code := d.run(false, "", "disable"); code != ExitOK || d.settings().Enabled {
		t.Fatalf("exit %d: %s", code, d.errb)
	}
}

func (d *typedDesk) status() memory.SyncStatus {
	d.t.Helper()
	st, _, _, err := d.mp.service(nil).SyncStatus(context.Background())
	if err != nil {
		d.t.Fatal(err)
	}
	return st
}

// An org hold stops the lane, whichever way sync was turned on: nothing is sent.
func TestTypedEnableRespectsAHold(t *testing.T) {
	d := newTypedDesk(t)
	d.save("held note")
	d.run(true, "\n", "enable")
	l := d.lane()
	l.Hold("org_paused")
	if err := l.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer l.Stop()
	time.Sleep(150 * time.Millisecond)
	if n := len(d.f.LiveContents("org-1", "alice")); n != 0 {
		t.Fatalf("%d notes sent while held", n)
	}
	l.Resume()
	if !waitUntil(t, func() bool { return len(d.f.LiveContents("org-1", "alice")) == 1 }) {
		t.Fatal("never sent after the hold ended")
	}
}

// `+"`"+`yes | kenaz ...`+"`"+`: a real pipe on stdin, full of yes, is refused.
func TestYesPipedIsRefused(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.WriteString(strings.Repeat("y\n", 100)); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	oldR := memoryStdin
	memoryStdin = r
	defer func() { memoryStdin = oldR }()
	for _, args := range [][]string{{"sync", "enable"}, {"on"}} {
		d := newTypedDesk(t)
		d.save("note")
		if code := runMemory(d.env, args); code != ExitError || d.settings().Enabled {
			t.Errorf("%v: exit %d: %s", args, code, d.out)
		}
		if !strings.Contains(d.errb.String(), "terminal") {
			t.Errorf("%v: %s", args, d.errb)
		}
	}
}

func onDesk(t *testing.T) *typedDesk {
	t.Helper()
	d := newTypedDesk(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude-for-test"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	old := hostCLICommand
	hostCLICommand = "claude-for-test"
	t.Cleanup(func() { hostCLICommand = old })
	return d
}

func (d *typedDesk) on(terminal bool, input string) int {
	d.t.Helper()
	oldT, oldR := memoryStdinTerminal, memoryStdin
	memoryStdinTerminal = func() bool { return terminal }
	memoryStdin = strings.NewReader(input)
	defer func() { memoryStdinTerminal, memoryStdin = oldT, oldR }()
	d.out.Reset()
	d.errb.Reset()
	return runMemory(d.env, []string{"on"})
}

// One command and one Enter: sync and automatic sync on, a note on its way, one
// question asked, and one result line. The notice comes first.
func TestMemoryOnOneEnter(t *testing.T) {
	d := onDesk(t)
	d.save("earlier note")
	if code := d.on(true, "\n"); code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, d.out, d.errb)
	}
	out := d.out.String()
	if strings.Count(out, "Press Enter or y") != 1 {
		t.Fatalf("%d questions: %s", strings.Count(out, "Press Enter or y"), out)
	}
	if !strings.Contains(out, notices.MemorySync().Text) || strings.Index(out, notices.MemorySync().Text) > strings.Index(out, "Press Enter or y") {
		t.Fatalf("notice not first: %s", out)
	}
	if !strings.Contains(out, "Memory is on") {
		t.Fatalf("%s", out)
	}
	if s := d.settings(); !s.Enabled || s.ConsentVersion != notices.MemorySync().ID {
		t.Fatalf("%+v", s)
	}
	if !d.status().SaveGrant {
		t.Fatal("automatic sync is off")
	}
	l := d.lane()
	if err := l.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer l.Stop()
	if !waitUntil(t, func() bool { return len(d.f.LiveContents("org-1", "alice")) == 1 }) {
		t.Fatal("the note never synced")
	}
	// Running it again asks nothing.
	if code := d.on(true, ""); code != ExitOK || strings.Contains(d.out.String(), "Press Enter or y") && !features.MemoryCapture {
		t.Fatalf("exit %d: %s", code, d.out)
	}
}

// With capture in the build it comes on with the member's own login, and the notice names
// that login before the question.
func TestMemoryOnCapture(t *testing.T) {
	old := features.MemoryCapture
	features.MemoryCapture = true
	t.Cleanup(func() { features.MemoryCapture = old })
	if !captureAvailable() {
		t.Skip("capture isn't available on this platform")
	}
	d := onDesk(t)
	hostOn := memProviderWithCapture{d.p}
	d.env.Provider = hostOn
	mp, err := d.env.memoryPaths()
	if err != nil {
		t.Fatal(err)
	}
	d.mp = mp
	if code := d.on(true, "y\n"); code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, d.out, d.errb)
	}
	out := d.out.String()
	want := notices.MemoryCapture(captureModelLine(capture.WriterHostCLI)).Text
	if !strings.Contains(out, want) || strings.Index(out, want) > strings.Index(out, "Press Enter or y") {
		t.Fatalf("%s", out)
	}
	if s := d.mp.captureSettings(); !s.Active() || s.Writer != capture.WriterHostCLI {
		t.Fatalf("%+v", s)
	}
	if !d.settings().Enabled || !d.status().SaveGrant {
		t.Fatal("sync isn't on")
	}
	if !strings.Contains(out, "new session") {
		t.Fatalf("no restart line: %s", out)
	}
	// Off stops both, with no terminal.
	d.out.Reset()
	if code := runMemory(d.env, []string{"off"}); code != ExitOK {
		t.Fatalf("exit %d: %s", code, d.errb)
	}
	if d.mp.captureSettings().Active() || d.settings().Enabled {
		t.Fatalf("%+v %+v", d.mp.captureSettings(), d.settings())
	}
}

type memProviderWithCapture struct{ *memProvider }

func (memProviderWithCapture) Me() fleetapi.Me           { return captureMe{} }
func (p memProviderWithCapture) Memory() fleetapi.Memory { return p.memProvider.Memory() }

type captureMe struct{ fleetapi.Me }

func (captureMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: map[string]bool{
		fleetapi.CapMemorySync: true, fleetapi.CapMemoryCaptureKinds: true, fleetapi.CapMemoryProjectScope: true, fleetapi.CapMemoryCaptureHostCLIOff(capture.DefaultVendor): false,
	}}, nil
}

// Without memory_capture_kinds, `kenaz memory on` turns sync on and leaves capture off,
// saying why. The capture notice isn't shown, and no capture setting is written.
func TestMemoryOnCaptureNotOffered(t *testing.T) {
	old := features.MemoryCapture
	features.MemoryCapture = true
	t.Cleanup(func() { features.MemoryCapture = old })
	if !captureAvailable() {
		t.Skip("capture isn't available on this platform")
	}
	d := onDesk(t)
	d.env.Provider = memProviderNoCapture{d.p}
	mp, err := d.env.memoryPaths()
	if err != nil {
		t.Fatal(err)
	}
	d.mp = mp
	d.save("note")
	if code := d.on(true, "\n"); code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, d.out, d.errb)
	}
	out := d.out.String()
	if !strings.Contains(out, usertext.CaptureNotOffered) || strings.Contains(out, notices.MemoryCapture(captureModelLine(capture.WriterHostCLI)).Text) {
		t.Fatalf("%s", out)
	}
	if s := d.mp.captureSettings(); s.Active() || s.Writer != "" {
		t.Fatalf("capture came on: %+v", s)
	}
	if !d.settings().Enabled {
		t.Fatal("sync isn't on")
	}
}

type memProviderNoCapture struct{ *memProvider }

func (memProviderNoCapture) Me() fleetapi.Me           { return noCaptureMe{} }
func (p memProviderNoCapture) Memory() fleetapi.Memory { return p.memProvider.Memory() }

type noCaptureMe struct{ fleetapi.Me }

func (noCaptureMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: map[string]bool{fleetapi.CapMemorySync: true, fleetapi.CapMemoryProjectScope: true}}, nil
}

// A no changes nothing, a plan without sync says so in one sentence, and with the program
// missing capture is explained and the rest goes on.
func TestMemoryOnDeclineAndLimits(t *testing.T) {
	d := onDesk(t)
	d.save("note")
	if code := d.on(true, "no\n"); code != ExitOK || !strings.Contains(d.out.String(), "Nothing changed") {
		t.Fatalf("exit %d: %s", code, d.out)
	}
	if d.settings().Enabled || d.status().SaveGrant || d.mp.captureSettings().Active() {
		t.Fatal("changed on no")
	}
	if code := d.on(false, "y\n"); code != ExitError || !strings.Contains(d.errb.String(), "memory on") {
		t.Fatalf("exit %d: %s", code, d.errb)
	}
	d = onDesk(t)
	d.save("note")
	d.env.Provider = noSyncPlan{d.p}
	mp, err := d.env.memoryPaths()
	if err != nil {
		t.Fatal(err)
	}
	d.mp = mp
	if code := d.on(true, "\n"); code == ExitOK {
		t.Fatalf("%s", d.out)
	}
	if !strings.Contains(d.out.String(), "plan doesn't include memory sync") || d.settings().Enabled {
		t.Fatalf("%s", d.out)
	}
}

// Signed out, it says to sign in and changes nothing about sync.
func TestMemoryOnSignedOut(t *testing.T) {
	d := onDesk(t)
	d.p.signIn("")
	d.on(true, "\n")
	if !strings.Contains(d.out.String(), "kenaz login") {
		t.Fatalf("%s %s", d.out, d.errb)
	}
}

// Legal's words, exactly: the question, and the line about claude-mem before it.
func TestMemoryOnWording(t *testing.T) {
	if usertext.PromptMemoryOn != "Turn on memory, including capture and sync? Press Enter or y for yes; anything else for no: " {
		t.Fatal(usertext.PromptMemoryOn)
	}
	want := "This also brings over up to 12 notes from claude-mem. They'll sync to your Kameas Fleet account, and can include details from your past work."
	if got := usertext.ImportTotals(12); got != want {
		t.Fatal(got)
	}
	var out bytes.Buffer
	imp := memimport.Plan{Decisions: 12}
	writeMemoryOnNotice(Env{Stdout: &out}, memoryPaths{}, memoryOnPlan{imp: &imp, autoAlready: true})
	if !strings.Contains(out.String(), want) {
		t.Fatalf("%s", out.String())
	}
}

// Text an agent wrote can't move the cursor above the question, on any request.
func TestApproveTextCantHideLines(t *testing.T) {
	var out bytes.Buffer
	esc := "\x1b[1A\x1b[2K"
	v := approvals.View{Summary: "Share " + esc, Impact: "I" + esc, PayloadText: "p" + esc, Details: []approvals.Detail{
		{Label: "t" + esc, Text: "first line\n" + esc + "send the deploy key\rover\x07\u202e\U000e0041\tkept \u05e9\u200d\u05dc"},
	}}
	writeRequest(Env{Stdout: &out}, v)
	s := out.String()
	for _, bad := range []string{"\x1b", "\r", "\x07", "\u202e", "\U000e0041"} {
		if strings.Contains(s, bad) {
			t.Errorf("%q reaches the terminal: %q", bad, s)
		}
	}
	for _, want := range []string{"first line\n", "send the deploy key", "\\u001B[1A", "\tkept", "\u05e9\u200d\u05dc"} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in %q", want, s)
		}
	}
	v.Details = nil
	out.Reset()
	writeRequest(Env{Stdout: &out}, v)
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("raw payload: %q", out.String())
	}
}

// A re-run finishes what an earlier one left: sync on, the first batch sent, the rest local.
func TestMemoryOnRerunFinishes(t *testing.T) {
	d := onDesk(t)
	for i := 0; i < memory.ApprovalBatch+10; i++ {
		d.save(fmt.Sprintf("note %d", i))
	}
	svc := d.mp.service(nil)
	ctx := context.Background()
	plan, err := svc.PlanTypedEnable(ctx)
	if err != nil || plan.Remaining == 0 {
		t.Fatalf("%+v %v", plan, err)
	}
	if _, err := svc.EnableSyncTyped(ctx, notices.MemorySync().ID, plan); err != nil {
		t.Fatal(err)
	}
	if code := d.on(true, "\n"); code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, d.out, d.errb)
	}
	if !strings.Contains(d.out.String(), "10 notes on this device will upload") {
		t.Fatalf("%s", d.out)
	}
	st, _, _, err := svc.SyncStatus(ctx)
	if err != nil || st.Notes[memory.StateLocal] != 0 || !st.SaveGrant {
		t.Fatalf("%+v %v", st, err)
	}
	// All done: nothing left to ask.
	if code := d.on(true, ""); code != ExitOK || strings.Contains(d.out.String(), "Press Enter") {
		t.Fatalf("exit %d: %s", code, d.out)
	}
}

// One clean run takes every batch.
func TestMemoryOnManyBatches(t *testing.T) {
	d := onDesk(t)
	for i := 0; i < memory.ApprovalBatch*2+5; i++ {
		d.save(fmt.Sprintf("note %d", i))
	}
	if code := d.on(true, "\n"); code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, d.out, d.errb)
	}
	st, _, _, _ := d.mp.service(nil).SyncStatus(context.Background())
	if st.Notes[memory.StateLocal] != 0 {
		t.Fatalf("%v", st.Notes)
	}
}

// Off says everything it knows, needing no terminal.
func TestMemoryOffPrintsWhatIsStillGoing(t *testing.T) {
	d := onDesk(t)
	d.save("x")
	if code := d.on(true, "\n"); code != ExitOK {
		t.Fatalf("%d %s", code, d.errb)
	}
	// An automatic note whose send was tried, so Fleet may already hold it.
	ctx := context.Background()
	p := d.mp.active(ctx)
	path := memory.ProfilePath(d.mp.StateRoot, p)
	st, err := memory.OpenProfile(path, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SaveAuto(ctx, memory.AutoNote{ProjectKey: memory.GlobalProject, Kind: memory.KindObservation, RequestKey: "off-test", Title: "seen", Body: "body", Source: memory.SourceImport, Tags: []string{memory.AutoTag}}); err != nil {
		t.Fatal(err)
	}
	l, err := st.AcquireLease(ctx, "off-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := st.QueueAutoBacklog(ctx, l); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	st.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE outbox SET ever_sent = 1, attempt = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	oldT := memoryStdinTerminal
	memoryStdinTerminal = func() bool { return false }
	defer func() { memoryStdinTerminal = oldT }()
	d.out.Reset()
	if code := runMemory(d.env, []string{"off"}); code != ExitOK {
		t.Fatalf("%d %s", code, d.errb)
	}
	if !strings.Contains(d.out.String(), "may already have reached Kameas Fleet") {
		t.Errorf("%s", d.out)
	}
}

// Fleet not answering about capture isn't Fleet saying no.
func TestMemoryOnCaptureUnconfirmed(t *testing.T) {
	old := features.MemoryCapture
	features.MemoryCapture = true
	t.Cleanup(func() { features.MemoryCapture = old })
	if !captureAvailable() {
		t.Skip("capture isn't available on this platform")
	}
	d := onDesk(t)
	d.env.Provider = capsFailing{d.p}
	mp, err := d.env.memoryPaths()
	if err != nil {
		t.Fatal(err)
	}
	d.mp = mp
	writer, why := pickWriter(context.Background(), d.env, d.mp)
	if writer != "" || why != usertext.CaptureUnconfirmedForOrg {
		t.Fatalf("%q %q", writer, why)
	}
}

type capsFailing struct{ *memProvider }

func (capsFailing) Me() fleetapi.Me           { return capsFailingMe{} }
func (p capsFailing) Memory() fleetapi.Memory { return p.memProvider.Memory() }

type capsFailingMe struct{ fleetapi.Me }

func (capsFailingMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{}, errors.New("no answer")
}

// A later batch refused: sync is on, and the words say so.
func TestSyncPartlyWording(t *testing.T) {
	if s := usertext.SyncPartly; !strings.HasPrefix(s, "Sync is on") || strings.Contains(s, "didn't turn on") {
		t.Fatal(s)
	}
	got := usertext.MemoryOnResult([]string{"capture", "sync"}, 5, 2)
	if !strings.Contains(got, "5 notes brought over") || !strings.Contains(got, "2 notes couldn't be brought over") {
		t.Fatal(got)
	}
}

// fakeRelay answers as an older build, and doesn't go away when asked.
type fakeRelay struct {
	srv    *http.Server
	info   daemon.Info
	stop   atomic.Int32
	onStop func()
}

func startFakeRelay(t *testing.T, dir, version string) *fakeRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRelay{info: daemon.Info{PID: os.Getpid(), Port: ln.Addr().(*net.TCPAddr).Port, Version: version, Nonce: "nonce-" + version, StartedAt: time.Now(), Variant: variant.Current.Name}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		m := hmac.New(sha256.New, []byte(f.info.Nonce))
		m.Write([]byte(r.URL.Query().Get("challenge")))
		json.NewEncoder(w).Encode(map[string]string{"version": version, "proof": hex.EncodeToString(m.Sum(nil))})
	})
	mux.HandleFunc("POST /control/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		f.stop.Add(1)
		w.WriteHeader(204)
		if f.onStop != nil {
			f.onStop()
		}
	})
	f.srv = &http.Server{Handler: mux}
	go f.srv.Serve(ln)
	t.Cleanup(func() { f.srv.Close() })
	if err := daemon.WriteInfo(dir, f.info); err != nil {
		t.Fatal(err)
	}
	return f
}

// An older relay that stays running gets a second try, then one plain line.
func TestRefreshRelaySaysWhenAnOlderOneStays(t *testing.T) {
	d := onDesk(t)
	r, err := d.env.resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := startFakeRelay(t, r.StateDir, "0.7.0")
	line := refreshRelay(context.Background(), d.env)
	if old.stop.Load() != 2 {
		t.Errorf("%d stop requests, want 2", old.stop.Load())
	}
	if !strings.Contains(line, "0.7.0") || !strings.Contains(line, "Close your AI tools") {
		t.Fatalf("%q", line)
	}
}

// An older relay that goes when asked is replaced without a word.
func TestRefreshRelayReplacesAnOlderOne(t *testing.T) {
	d := onDesk(t)
	r, err := d.env.resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := startFakeRelay(t, r.StateDir, "0.7.0")
	var next *fakeRelay
	old.onStop = func() { go old.srv.Close() }
	d.env.Spawn = func(string, fleetapi.Env) error {
		next = startFakeRelay(t, r.StateDir, Version)
		return nil
	}
	if line := refreshRelay(context.Background(), d.env); line != "" || next == nil {
		t.Fatalf("%q", line)
	}
}

// A note body keeps its codes all the way into the approval text; printing is what stops them.
func TestApproveEscapesThroughARealRender(t *testing.T) {
	d := onDesk(t)
	n := memory.NewNote{ProjectKey: memory.GlobalProject, Title: "t", Source: memory.SourceCodex, Body: "ok\n\x1b[1A\x1b[2Kother"}
	raw, err := json.Marshal(map[string]any{"Profile": d.mp.active(context.Background()), "Note": n})
	if err != nil {
		t.Fatal(err)
	}
	view, err := renderMemory(memory.OpSave)(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writeRequest(Env{Stdout: &out}, approvals.View{Summary: view.Summary, Impact: view.Impact, Details: view.Details})
	if len(view.Details) == 0 {
		t.Fatal("no details")
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("%q", out.String())
	}
}

// Off a terminal the progress line appears once, so a log doesn't fill up.
func TestProgressLineOffATerminal(t *testing.T) {
	var out bytes.Buffer
	line := newProgressLine(Env{Stdout: &out})
	for i := 1; i <= 3; i++ {
		line.update(usertext.ImportingNotes(i*100, 300))
	}
	line.finish()
	if got := out.String(); got != usertext.ImportingNotes(100, 300)+"\n" {
		t.Fatalf("%q", got)
	}
}

// On a terminal it rewrites one line and ends with a line break.
func TestProgressLineOnATerminal(t *testing.T) {
	var out bytes.Buffer
	line := &progressLine{w: &out, tty: true}
	line.update("a")
	line.update("b")
	line.finish()
	if got := out.String(); got != "\ra\x1b[K\rb\x1b[K\n" {
		t.Fatalf("%q", got)
	}
}

// Stop means nothing has the database open: the supervisor has finished too.
func TestLaneStopWaitsForItsSupervisor(t *testing.T) {
	d := newTypedDesk(t)
	l := d.lane()
	if err := l.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.Stop()
	select {
	case <-l.sdone:
	default:
		t.Fatal("the supervisor is still running after Stop")
	}
}

// Another build's relay is left alone, and the line says so.
func TestRefreshRelayLeavesAnotherBuildAlone(t *testing.T) {
	d := onDesk(t)
	r, err := d.env.resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := startFakeRelay(t, r.StateDir, Version)
	other.info.Variant = "another-build"
	if err := daemon.WriteInfo(r.StateDir, other.info); err != nil {
		t.Fatal(err)
	}
	if line := refreshRelay(context.Background(), d.env); line != usertext.RelayOtherBuild || other.stop.Load() != 0 {
		t.Fatalf("%q, %d stop requests", line, other.stop.Load())
	}
}

// Imported notes past the hourly cap all reach Kameas Fleet, and `sync status` counts what waits.
func TestImportedNotesUploadPastTheCap(t *testing.T) {
	d := onDesk(t)
	if code := d.on(true, "\n"); code != ExitOK {
		t.Fatalf("%d %s", code, d.errb)
	}
	ctx := context.Background()
	p := d.mp.active(ctx)
	st, err := memory.OpenProfile(memory.ProfilePath(d.mp.StateRoot, p), p)
	if err != nil {
		t.Fatal(err)
	}
	const notes = 450
	for i := 0; i < notes; i++ {
		if _, _, err := st.SaveAuto(ctx, memory.AutoNote{ProjectKey: memory.GlobalProject, Kind: memory.KindObservation, RequestKey: fmt.Sprintf("import-%d", i), Title: fmt.Sprintf("n%d", i), Body: "imported body", Source: memory.SourceImport, Tags: []string{memory.AutoTag}}); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	d.out.Reset()
	if code := d.run(false, "", "status"); code != ExitOK || !strings.Contains(d.out.String(), usertext.AutoWaitingToUpload(notes)) {
		t.Fatalf("%d %s", code, d.out)
	}
	// Already on, so nothing to ask, and it's a person's own command.
	if code := d.on(true, ""); code != ExitOK {
		t.Fatalf("%d %s", code, d.errb)
	}
	l := d.lane()
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer l.Stop()
	end := time.Now().Add(30 * time.Second)
	for {
		s, _, _, err := d.mp.service(nil).SyncStatus(ctx)
		if err == nil && s.Notes[memory.StateLocal] == 0 && s.Notes[memory.StatePending] == 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("%+v %v", s.Notes, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An import with no terminal gets no pass from the cap: this is what an agent's shell looks like.
func TestImportWithoutATerminalKeepsTheCap(t *testing.T) {
	old := features.MemoryCapture
	features.MemoryCapture = true
	t.Cleanup(func() { features.MemoryCapture = old })
	if !captureAvailable() {
		t.Skip("capture isn't available on this platform")
	}
	d := onDesk(t)
	d.env.Provider = memProviderWithCapture{d.p}
	mp, err := d.env.memoryPaths()
	if err != nil {
		t.Fatal(err)
	}
	d.mp = mp
	if code := d.on(true, "\n"); code != ExitOK {
		t.Fatalf("%d %s", code, d.errb)
	}
	oldT := memoryStdinTerminal
	t.Cleanup(func() { memoryStdinTerminal = oldT })
	ctx := context.Background()
	p := d.mp.active(ctx)
	path := memory.ProfilePath(d.mp.StateRoot, p)
	through := func() string {
		t.Helper()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var v string
		if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'import_upload_through'`).Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return v
	}
	other := claudeMemFixture(t)
	memoryStdinTerminal = func() bool { return false }
	if code := runMemoryImport(ctx, d.env, d.mp, []string{"claude-mem", "--yes", "--db", other}); code != ExitOK {
		t.Fatalf("%d %s", code, d.errb)
	}
	if got := through(); got != "" {
		t.Fatalf("no terminal, and the notes got a pass: %s", got)
	}
	memoryStdinTerminal = func() bool { return true }
	if code := runMemoryImport(ctx, d.env, d.mp, []string{"claude-mem", "--yes", "--db", other}); code != ExitOK {
		t.Fatalf("%d %s", code, d.errb)
	}
	if through() == "" {
		t.Fatal("a person's own import got no pass")
	}
}

// claudeMemFixture is a small database in the other tool's shape, with one decision in it.
func claudeMemFixture(t *testing.T) string {
	t.Helper()
	return claudeMemFixtureWith(t, 1)
}

// claudeMemFixtureWith holds n decisions.
func claudeMemFixtureWith(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE sdk_sessions (id INTEGER PRIMARY KEY, content_session_id TEXT, memory_session_id TEXT, project TEXT, user_prompt TEXT)`,
		`CREATE TABLE user_prompts (id INTEGER PRIMARY KEY, content_session_id TEXT, prompt_number INTEGER, prompt_text TEXT, created_at TEXT, created_at_epoch INTEGER)`,
		`CREATE TABLE pending_messages (id INTEGER PRIMARY KEY, session_db_id INTEGER, content_session_id TEXT, message_type TEXT, tool_name TEXT, tool_input TEXT, tool_response TEXT, cwd TEXT, last_user_message TEXT, last_assistant_message TEXT, created_at_epoch INTEGER)`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT, text TEXT, type TEXT, title TEXT, subtitle TEXT, facts TEXT, narrative TEXT, concepts TEXT, files_read TEXT, files_modified TEXT, prompt_number INTEGER, discovery_tokens INTEGER, created_at TEXT, created_at_epoch INTEGER)`,
		`CREATE TABLE session_summaries (id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT, request TEXT, investigated TEXT, learned TEXT, completed TEXT, next_steps TEXT, files_read TEXT, files_edited TEXT, notes TEXT, prompt_number INTEGER, discovery_tokens INTEGER, created_at TEXT, created_at_epoch INTEGER)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(`INSERT INTO observations(memory_session_id, project, text, type, title, subtitle, facts, narrative, concepts, files_read, files_modified, discovery_tokens, created_at, created_at_epoch)
VALUES ('m1','app','raw','decision',?,'sub','["a fact"]','why','[]','[]','[]',1,'x',?)`, fmt.Sprintf("decision %d", i), time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every field approve prints, including after approving.
func TestApprovePrintsNoControlCodes(t *testing.T) {
	bad := "x\x1b[2K\r\x08\x07\x7f\u009b1A\u2028\u202e\U000e0041\xff\x9b"
	var out bytes.Buffer
	writeRequest(Env{Stdout: &out}, approvals.View{Summary: bad, Impact: bad, PayloadText: bad})
	writeRequest(Env{Stdout: &out}, approvals.View{Summary: bad, Details: []approvals.Detail{{Label: bad, Text: bad}}})
	for _, st := range []approvals.Status{approvals.StatusDone, approvals.StatusDenied} {
		writeApproved(context.Background(), Env{Stdout: &out, Stderr: &out}, resultLister{[]byte(bad)}, "id", st)
	}
	for _, c := range []string{"\x1b", "\r", "\x08", "\x07", "\x7f", "\u009b", "\u2028", "\u202e", "\U000e0041", "\xff"} {
		if strings.Contains(out.String(), c) {
			t.Errorf("%q reached the terminal", c)
		}
	}
}

type resultLister struct{ res []byte }

func (r resultLister) List(context.Context) (approvals.WireList, error) {
	return approvals.WireList{}, nil
}
func (r resultLister) Status(context.Context, string) (approvals.Status, []byte, error) {
	return approvals.StatusDone, r.res, nil
}

// A wall of blank lines can't push the first part off the screen, and is named. Short runs stay.
func TestApproveCollapsesBlankRuns(t *testing.T) {
	var out bytes.Buffer
	wall := strings.Repeat("\n", 200) + strings.Repeat(" \t\n", 300)
	writeRequest(Env{Stdout: &out}, approvals.View{Summary: "s", Details: []approvals.Detail{{Label: "l", Text: "send the key\n" + wall + "harmless"}}})
	got := out.String()
	if n := strings.Count(got, "\n"); n > 12 {
		t.Fatalf("%d lines: %q", n, got)
	}
	if !strings.Contains(got, "send the key") || !strings.Contains(got, "harmless") || !strings.Contains(got, usertext.BlankLines(500)) {
		t.Fatalf("%q", got)
	}
	if s := approvalText("a\n\n\nb"); s != "a\n\n\nb" {
		t.Fatalf("%q", s)
	}
}

// memory on says what it left out, right with what it brings over.
func TestMemoryOnSaysWhatItLeavesOut(t *testing.T) {
	d := onDesk(t)
	var out bytes.Buffer
	d.env.Stdout = &out
	writeMemoryOnNotice(d.env, d.mp, memoryOnPlan{autoAlready: true, imp: &memimport.Plan{OverBudget: 7}})
	if !strings.Contains(out.String(), usertext.ImportLeftOut(7)) {
		t.Fatalf("%s", out.String())
	}
	out.Reset()
	writeMemoryOnNotice(d.env, d.mp, memoryOnPlan{autoAlready: true, imp: &memimport.Plan{}})
	if strings.Contains(out.String(), "left out") {
		t.Fatalf("%s", out.String())
	}
}

// Fleet not answering about room keeps a fixed budget, not the device's whole limit.
func TestImportBudgetWhenFleetCantSay(t *testing.T) {
	d := onDesk(t)
	d.p.fleet.Fault = func(string, bool) error { return errors.New("no answer") }
	t.Cleanup(func() { d.p.fleet.Fault = nil })
	mp := d.mp
	if _, ok := fleetUsage(context.Background(), mp); ok {
		t.Fatal("the fake answered")
	}
	path := claudeMemFixtureWith(t, memimport.NoAnswerNotes+3)
	plan, err := prepareImport(context.Background(), d.env, mp, path, memimport.DefaultDays, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Total() != memimport.NoAnswerNotes || plan.OverBudget != 3 {
		t.Fatalf("%d planned, %d left out", plan.Total(), plan.OverBudget)
	}
}

// In a build whose sync includes project notes, memory-sync/v4 is shown and recorded
// only while Fleet's capabilities carry memory_project_scope. Without it, every way to
// turn sync on is refused before the notice, and nothing is recorded here or on Fleet.
func TestProjectNoticeNeedsTheCapability(t *testing.T) {
	oldF, oldN, oldM := features.MemoryProjectScope, notices.ProjectScopeRelease, memory.ProjectScopeRelease
	features.MemoryProjectScope, notices.ProjectScopeRelease, memory.ProjectScopeRelease = true, true, true
	t.Cleanup(func() {
		features.MemoryProjectScope, notices.ProjectScopeRelease, memory.ProjectScopeRelease = oldF, oldN, oldM
	})
	for _, c := range []struct {
		name string
		me   fleetapi.Me
	}{
		{"absent", noProjectMe{}},
		{"read fails", capsFailingMe{}},
	} {
		d := newTypedDesk(t)
		d.env.Provider = memProviderMe{d.p, c.me}
		mp, err := d.env.memoryPaths()
		if err != nil {
			t.Fatal(err)
		}
		d.mp = mp
		d.save("note")
		if code := d.run(true, "y\n", "enable"); code != ExitError || strings.Contains(d.out.String(), notices.MemorySyncV4) ||
			!strings.Contains(d.errb.String(), "doesn't sync for your organization yet") {
			t.Errorf("%s: enable: %d %s %s", c.name, code, d.out, d.errb)
		}
		if code := d.on(true, "y\n"); strings.Contains(d.out.String(), notices.MemorySyncV4) || !strings.Contains(d.out.String(), usertext.ProjectNotesNotOffered) {
			t.Errorf("%s: on: %d %s %s", c.name, code, d.out, d.errb)
		}
		if _, err := d.mp.service(nil).EnableSync(context.Background()); !errors.Is(err, memory.ErrProjectScopeNotOffered) {
			t.Errorf("%s: the approval route: %v", c.name, err)
		}
		if d.settings().Enabled {
			t.Errorf("%s: sync came on", c.name)
		}
		if s, err := d.f.SessionClient(func(context.Context) (fleetapi.Session, error) {
			return fleetapi.Session{Env: fleetapi.EnvDev, OrgID: "org-1", UserID: "alice"}, nil
		}).Settings(fleetapi.WithExpectedSession(context.Background(), fleetapi.ExpectedSession{Env: fleetapi.EnvDev, OrgID: "org-1", Subject: "alice"})); err == nil && s.ConsentVersion != "" {
			t.Errorf("%s: Fleet recorded %s", c.name, s.ConsentVersion)
		}
	}
	// With the key, v4 is shown and taken.
	d := newTypedDesk(t)
	d.save("note")
	if code := d.run(true, "y\n", "enable"); code != ExitOK || !strings.Contains(d.out.String(), notices.MemorySyncV4) {
		t.Fatalf("offered: %d %s %s", code, d.out, d.errb)
	}
	if s := d.settings(); !s.Enabled || s.ConsentVersion != notices.MemorySyncV4ID {
		t.Fatalf("%+v", s)
	}
}

type memProviderMe struct {
	*memProvider
	me fleetapi.Me
}

func (p memProviderMe) Me() fleetapi.Me         { return p.me }
func (p memProviderMe) Memory() fleetapi.Memory { return p.memProvider.Memory() }

type noProjectMe struct{ fleetapi.Me }

func (noProjectMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: map[string]bool{fleetapi.CapMemorySync: true}}, nil
}
