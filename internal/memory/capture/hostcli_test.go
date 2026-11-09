//go:build !windows

package capture

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLI writes a shell script that records its arguments, environment and
// stdin, and prints reply.
func fakeCLI(t *testing.T, reply string, exit int) (path, dir string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "fake-host")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"" + dir + "/args\"\n" +
		"env > \"" + dir + "/env\"\n" +
		"cat > \"" + dir + "/stdin\"\n" +
		"pwd > \"" + dir + "/pwd\"\n" +
		"cat <<'JSON'\n" + reply + "\nJSON\n" +
		"echo 'stderr may quote captured text' >&2\n"
	if exit != 0 {
		script += "exit 3\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const fakeHostReply = `{"type":"result","is_error":false,"result":"{\"observations\":[]}","total_cost_usd":0.0012,"usage":{"input_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":40}}`

func environ(extra ...string) func() []string {
	return func() []string {
		return append([]string{"PATH=/usr/bin:/bin", "HOME=/h",
			"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:1", "CLAUDE_CODE_ENABLE_TELEMETRY=1", "OTEL_METRICS_EXPORTER=otlp",
			"UNRELATED_SECRET=x"}, extra...)
	}
}

// allowedChildEnv is everything any writer's child may receive from Kenaz. The test
// environment below holds far more. A real subset check: a new name fails here.
var allowedChildEnv = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true, "TEMP": true, "TMP": true, "LANG": true,
	"USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true, "PROGRAMDATA": true, "SYSTEMROOT": true, "SystemRoot": true, "COMSPEC": true, "PATHEXT": true,
	"CLAUDE_CONFIG_DIR": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "SSL_CERT_FILE": true, "NODE_EXTRA_CA_CERTS": true,
	WorkerEnv: true, "CLAUDE_CODE_ENABLE_TELEMETRY": true, "OTEL_SDK_DISABLED": true,
}

const vendorSecret = "VENDOR-CREDENTIAL-VALUE"

// loadedEnviron holds credentials and settings of every kind, each with the marker.
func loadedEnviron(extra ...string) func() []string {
	names := []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_SESSION_ACCESS_TOKEN", "CLAUDE_SESSION_TOKEN",
		"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_AUTH", "GH_TOKEN", "GITHUB_TOKEN", "KENAZ_TOKEN", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"GOOGLE_APPLICATION_CREDENTIALS", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "ZZ_SOMETHING_NEW",
	}
	base := environ()()
	for _, n := range names {
		base = append(base, n+"="+vendorSecret)
	}
	return func() []string { return append(base, extra...) }
}

// checkChildEnv fails on any name outside the allowlist and on the marker anywhere.
func checkChildEnv(t *testing.T, label, dir string, also ...string) {
	t.Helper()
	allow := map[string]bool{}
	for k := range allowedChildEnv {
		allow[k] = true
	}
	for _, k := range also {
		allow[k] = true
	}
	for _, line := range strings.Split(read(t, dir+"/env"), "\n") {
		k, _, ok := strings.Cut(line, "=")
		if !ok || k == "PWD" || k == "SHLVL" || k == "_" || k == "OLDPWD" {
			continue // the fake's own shell
		}
		if !allow[k] {
			t.Errorf("%s: %s reached the child", label, k)
		}
	}
	for _, f := range []string{"env", "args", "stdin"} {
		if strings.Contains(read(t, dir+"/"+f), vendorSecret) {
			t.Errorf("%s: a credential reached the child through %s", label, f)
		}
	}
}

// Kenaz never reads, stores, copies or forwards a vendor's credentials or session
// tokens. Under any writer, none reaches the child through Kenaz: the CLI finds its
// own login.
func TestHostCLIForwardsNoCredentials(t *testing.T) {
	path, dir := fakeCLI(t, fakeHostReply, 0)
	h := &HostCLI{Path: path, WorkDir: t.TempDir(), Environ: loadedEnviron()}
	if _, err := h.Complete(context.Background(), Request{System: "s", User: "u", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	checkChildEnv(t, "host-cli", dir)

	// The local writer gets the same allowlist.
	path, dir = fakeCLI(t, `{"observations":[]}`, 0)
	c := &Command{Line: path, Environ: loadedEnviron()}
	if _, err := c.Complete(context.Background(), Request{System: "S", User: "U"}); err != nil {
		t.Fatal(err)
	}
	checkChildEnv(t, "local", dir)

	// Bare mode with a key: that one key, under its own name, and nothing else.
	path, dir = fakeCLI(t, fakeHostReply, 0)
	b := &HostCLI{Path: path, Bare: true, APIKeyEnv: "MY_MODEL_KEY", WorkDir: t.TempDir(), Environ: loadedEnviron("MY_MODEL_KEY=sk-ant-api03-the-key")}
	if _, err := b.Complete(context.Background(), Request{System: "s", User: "u", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	env := read(t, dir+"/env")
	if !strings.Contains(env, "ANTHROPIC_API_KEY=sk-ant-api03-the-key") {
		t.Error("bare mode lost the key")
	}
	checkChildEnv(t, "bare", dir, "ANTHROPIC_API_KEY")

	// A login token in a variable with an innocent name, or in the usual one, is refused.
	for _, name := range []string{"MY_MODEL_KEY", "ANTHROPIC_API_KEY"} {
		path, dir = fakeCLI(t, fakeHostReply, 0)
		b = &HostCLI{Path: path, Bare: true, APIKeyEnv: name, WorkDir: t.TempDir(), Environ: environ(name + "=sk-ant-oat01-LOGINTOKENLOGINTOKEN")}
		if _, err := b.Complete(context.Background(), Request{System: "s", User: "u", Model: "m"}); failCode(err) != FailNeedsKey {
			t.Errorf("a login token in %s: %v", name, err)
		}
		if _, statErr := os.Stat(dir + "/env"); statErr == nil {
			t.Errorf("the child ran with a login token in %s", name)
		}
	}

	// Bare mode pointed at a login token refuses it.
	path, dir = fakeCLI(t, fakeHostReply, 0)
	b = &HostCLI{Path: path, Bare: true, APIKeyEnv: "CLAUDE_CODE_OAUTH_TOKEN", WorkDir: t.TempDir(), Environ: loadedEnviron()}
	_, err := b.Complete(context.Background(), Request{System: "s", User: "u", Model: "m"})
	if failCode(err) != FailNeedsKey {
		t.Errorf("a login token as the key: %v", err)
	}
	if _, statErr := os.Stat(dir + "/env"); statErr == nil {
		t.Error("the child ran")
	}

	// The detached worker starts with the same reduction.
	worker := WorkerEnviron(loadedEnviron("KENAZ_HOME=/k")(), Settings{Writer: WriterHostCLI}, func(n string) bool { return n == "KENAZ_HOME" })
	for _, kv := range worker {
		k, _, _ := strings.Cut(kv, "=")
		if strings.Contains(kv, vendorSecret) || (!allowedChildEnv[k] && k != "KENAZ_HOME") {
			t.Errorf("the worker started with %s", k)
		}
	}
}

func TestHostCLIRefusalsPauseWithACode(t *testing.T) {
	for _, c := range []struct {
		reply string
		exit  int
		code  string
	}{
		{`{"is_error":true,"api_error_status":429,"result":"x"}`, 0, FailRateLimited},
		{`{"is_error":true,"result":"You've hit your limit"}`, 1, FailRateLimited},
		{`{"is_error":true,"api_error_status":403,"result":"x"}`, 1, FailRefused},
		{`{"is_error":true,"result":"Not logged in. Please run /login"}`, 1, FailRefused},
	} {
		p, _ := fakeCLI(t, c.reply, c.exit)
		_, err := (&HostCLI{Path: p, Environ: environ()}).Complete(context.Background(), Request{User: "u", Model: "m"})
		if failCode(err) != c.code {
			t.Errorf("%s: %v", c.reply, err)
		}
	}
}

// Worker calls leave no trace: no hooks, no telemetry, no session on disk, no
// project context, and the prompt is not on the command line.
func TestHostCLIIsIsolated(t *testing.T) {
	for _, bare := range []bool{false, true} {
		path, dir := fakeCLI(t, fakeHostReply, 0)
		work := t.TempDir()
		h := &HostCLI{Path: path, Bare: bare, APIKeyEnv: "MY_KEY", WorkDir: work, Environ: environ("MY_KEY=sk-ant-api03-the-key", "ANTHROPIC_API_KEY=another")}
		r, err := h.Complete(context.Background(), Request{System: "SYS", User: "USERTEXT", Model: "small", MaxUSD: 0.05})
		if err != nil {
			t.Fatal(err)
		}
		if r.Text != `{"observations":[]}` || r.Usage.In != 150 || r.Usage.Out != 40 || r.Usage.USD != 0.0012 || !r.Usage.Reported {
			t.Errorf("response %+v", r)
		}
		args := read(t, dir+"/args")
		mode := "--safe-mode"
		if bare {
			mode = "--bare"
		}
		for _, want := range []string{mode, "-p", "--no-session-persistence", "--strict-mcp-config", "--setting-sources", "--tools", "disableAllHooks", "--max-budget-usd"} {
			if !strings.Contains(args, want) {
				t.Errorf("args lack %s:\n%s", want, args)
			}
		}
		if strings.Contains(args, "USERTEXT") {
			t.Error("the prompt is on the command line")
		}
		if !strings.Contains(read(t, dir+"/stdin"), "USERTEXT") {
			t.Error("the prompt is not on stdin")
		}
		env := read(t, dir+"/env")
		for _, bad := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_METRICS_EXPORTER", "UNRELATED_SECRET", "CLAUDE_CODE_ENABLE_TELEMETRY=1"} {
			if strings.Contains(env, bad) {
				t.Errorf("the call inherited %s", bad)
			}
		}
		for _, want := range []string{WorkerEnv + "=1", "CLAUDE_CODE_ENABLE_TELEMETRY=0"} {
			if !strings.Contains(env, want) {
				t.Errorf("env lacks %s", want)
			}
		}
		// The key mode uses the named key; the account mode never passes a key.
		if bare && !strings.Contains(env, "ANTHROPIC_API_KEY=sk-ant-api03-the-key") {
			t.Error("key mode did not pass the configured key")
		}
		if !bare && strings.Contains(env, "ANTHROPIC_API_KEY") {
			t.Error("account mode passed a key")
		}
		if got := strings.TrimSpace(read(t, dir+"/pwd")); !strings.HasSuffix(got, filepath.Base(work)) {
			t.Errorf("ran in %s", got)
		}
	}
}

func TestHostCLIFailuresAreCodesOnly(t *testing.T) {
	cases := []struct {
		name string
		h    func(t *testing.T) *HostCLI
		code string
	}{
		{"no binary", func(t *testing.T) *HostCLI { return &HostCLI{Path: "/nonexistent/host", Environ: environ()} }, FailCLIMissing},
		{"no key", func(t *testing.T) *HostCLI {
			p, _ := fakeCLI(t, fakeHostReply, 0)
			return &HostCLI{Path: p, Bare: true, APIKeyEnv: "MY_KEY", Environ: environ()}
		}, FailNeedsKey},
		{"exit", func(t *testing.T) *HostCLI {
			p, _ := fakeCLI(t, `{"result":"secret text"}`, 1)
			return &HostCLI{Path: p, Environ: environ()}
		}, FailExit},
		{"is_error", func(t *testing.T) *HostCLI {
			p, _ := fakeCLI(t, `{"is_error":true,"result":"quoted secret text"}`, 0)
			return &HostCLI{Path: p, Environ: environ()}
		}, FailExit},
		{"junk", func(t *testing.T) *HostCLI {
			p, _ := fakeCLI(t, `not json with secret text`, 0)
			return &HostCLI{Path: p, Environ: environ()}
		}, FailBadOutput},
	}
	for _, c := range cases {
		_, err := c.h(t).Complete(context.Background(), Request{User: "u", Model: "m"})
		if failCode(err) != c.code {
			t.Errorf("%s: %v", c.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the error quotes output: %v", c.name, err)
		}
	}
	// Cloud credentials stand in for a key in the bare mode.
	p, _ := fakeCLI(t, fakeHostReply, 0)
	h := &HostCLI{Path: p, Bare: true, APIKeyEnv: "MY_KEY", Environ: environ("CLAUDE_CODE_USE_BEDROCK=1", "AWS_REGION=us-east-2")}
	if _, err := h.Complete(context.Background(), Request{User: "u", Model: "m"}); err != nil {
		t.Errorf("cloud credentials: %v", err)
	}
}

func TestCommandBackendForALocalModel(t *testing.T) {
	path, dir := fakeCLI(t, `{"observations":[]}`, 0)
	c := &Command{Line: path + " --flag", Environ: environ()}
	r, err := c.Complete(context.Background(), Request{System: "SYS", User: "USERTEXT"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(r.Text) != `{"observations":[]}` || r.Usage.USD != 0 || r.Usage.In == 0 {
		t.Errorf("response %+v", r)
	}
	in := read(t, dir+"/stdin")
	if !strings.Contains(in, "SYS") || !strings.Contains(in, "USERTEXT") || !strings.Contains(read(t, dir+"/env"), WorkerEnv+"=1") {
		t.Errorf("stdin %q", in)
	}
}
