package installed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
)

// otel replays the host's synthetic OTLP export (internal/fixtures: every
// content-bearing attribute carries a canary) into the installed relay's
// loopback receiver, then runs the projection check as a Go test.
//
// The unofficial build's stub provider can never be signed in, so the
// installed relay refuses the export and the projected output cannot be
// observed outside Go tests. What the installed binary can show is that it
// refuses, queues nothing and keeps no planted content on disk. The
// allowlist check of the projected output is therefore the repository's Go
// test of the real receiver and pipeline against an in-process fake Fleet,
// invoked here and recorded as such.
func (j *journey) otel(ctx context.Context) {
	type export struct {
		path string
		msg  proto.Message
	}
	var exports []export
	if j.h.cli == "claude" {
		exports = []export{{"/v1/logs", fixtures.ClaudeLogs()}, {"/v1/metrics", fixtures.ClaudeSessionStartMetric()}, {"/v1/traces", fixtures.ClaudeTraces()}}
	} else {
		exports = []export{{"/v1/logs", fixtures.CodexLogs(true)}, {"/v1/traces", fixtures.CodexTraces()}}
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", j.port)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	var problems, paths []string
	for _, e := range exports {
		paths = append(paths, e.path)
		body, err := proto.Marshal(e.msg)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+e.path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := client.Do(req)
		if err != nil {
			problems = append(problems, "POST "+e.path+": the relay's receiver did not answer")
			continue
		}
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(text), "not signed in") {
			problems = append(problems, fmt.Sprintf("POST %s: status %d, want 503 not signed in", e.path, resp.StatusCode))
		}
	}
	j.check("otel-receiver-signed-out", "POST "+strings.Join(paths, ", ")+" to the relay's loopback receiver (synthetic "+j.h.clientKind+" export)", "the installed relay's receiver answers on loopback and refuses the export with 503 not signed in (an official build accepts it once a member is signed in and opted in)", problems)

	// Nothing queued, and no planted content anywhere under the state root.
	problems = nil
	out, code := j.s.kenaz(ctx, "status", "--json")
	var st struct {
		Flavor    string `json:"flavor"`
		Reporting struct {
			SignedIn *bool `json:"signed_in"`
			Queued   *int  `json:"queued"`
		} `json:"reporting"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &st) != nil {
		problems = append(problems, "kenaz status --json failed or does not parse")
	} else {
		if st.Reporting.SignedIn == nil || *st.Reporting.SignedIn {
			problems = append(problems, "status does not report signed_in false")
		}
		if st.Reporting.Queued == nil || *st.Reporting.Queued != 0 {
			problems = append(problems, "status reports queued telemetry")
		}
	}
	files, leaked := 0, 0
	_ = filepath.WalkDir(j.s.kenazHome, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		files++
		for _, canary := range fixtures.Canaries {
			if bytes.Contains(b, []byte(canary)) {
				leaked++
				break
			}
		}
		return nil
	})
	if leaked > 0 {
		problems = append(problems, fmt.Sprintf("%d file(s) under the relay's state hold planted content", leaked))
	}
	if entries, err := os.ReadDir(filepath.Join(j.stateDir(), "spool")); err == nil && len(entries) > 0 {
		problems = append(problems, "the spool holds queued data after a refused export")
	}
	j.check("otel-nothing-retained", "kenaz status --json; scan of the relay's state for planted content", fmt.Sprintf("signed out, nothing is queued and none of the %d files under the relay's state holds planted content", files), problems)

	// The projection, as a Go test (see the function comment).
	run := "^TestConformanceOTelJourney$/^" + j.h.clientKind + "$"
	args := []string{"test", "-count=1", "-v", "-tags", j.o.Tags, "-run", run, "./app"}
	command := "go test -count=1 -tags '" + j.o.Tags + "' -run '" + run + "' ./app"
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", args...)
	cmd.Dir = j.o.Root
	tout, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(tout, []byte("--- PASS: TestConformanceOTelJourney/"+j.h.clientKind)) {
		j.fail("otel-projection-go-test", command, "the Go test failed or did not run: "+testFailure(string(tout)))
		return
	}
	j.pass("otel-projection-go-test", command, "Go test, not the installed binary: the same synthetic export through the real receiver and pipeline to an in-process fake Fleet yields only allowlisted fields, attributed to "+j.h.clientKind+", with no planted content")
}

var testFailureRe = regexp.MustCompile(`_test\.go:\d+: |\.go:\d+:\d+: |^panic: |^--- FAIL|cannot |no required module|build failed`)

// testFailure picks the lines of a failed `go test` run that say why (the
// test's own messages are about synthetic fixtures; nothing else is kept).
func testFailure(out string) string {
	var why []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); testFailureRe.MatchString(line) && len(why) < 4 {
			why = append(why, line)
		}
	}
	if len(why) == 0 {
		return lastLine(out)
	}
	return strings.Join(why, " | ")
}
