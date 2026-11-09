package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// WorkerEnv is set on the worker and everything it starts. Every kenaz hook
// exits at once when it sees it, so a model call can never produce hook
// activity, telemetry or more capture for itself.
const WorkerEnv = "KENAZ_MEMORY_WORKER"

// Usage is what one model call cost. USD is what the host reported, or an
// estimate when Reported is false.
type Usage struct {
	In, Out  int64
	USD      float64
	Reported bool
}

// Request is one model call.
type Request struct {
	System string
	User   string
	Model  string
	// MaxUSD is a hard ceiling for this call where the host supports one.
	MaxUSD float64
}

// Response is a model call's result.
type Response struct {
	Text  string
	Usage Usage
}

// Failure is a summariser failure, with a code from the closed set. It holds
// no text from the failed process.
type Failure struct{ Code string }

func (f *Failure) Error() string { return "summariser failed: " + f.Code }

func failCode(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Code
	}
	return FailExit
}

// Summarizer turns a request into text. Implementations must not retain the
// request, and must not include its text in an error.
type Summarizer interface {
	Complete(ctx context.Context, r Request) (Response, error)
}

// callTimeout bounds one model call.
const callTimeout = 2 * time.Minute

// maxCallOutput bounds what is read back from a model process.
const maxCallOutput = 2 << 20

// Fallback prices for estimating spend when the host reports none, in USD per
// million tokens: the default model's list prices, for showing usage.
const (
	estimateUSDPerMTokIn  = 2.0
	estimateUSDPerMTokOut = 10.0
)

func estimateUSD(in, out int64) float64 {
	return float64(in)*estimateUSDPerMTokIn/1e6 + float64(out)*estimateUSDPerMTokOut/1e6
}

// HostCLI runs the host's own CLI, one process per request.
type HostCLI struct {
	Path string
	// Bare runs the host in its bare mode. It reads only an API key or cloud
	// credentials, never the user's signed-in account, and skips
	// hooks and plugins. Otherwise the host runs under the user's own signed-in
	// account in safe mode, which also skips hooks, plugins and project files.
	Bare bool
	// APIKeyEnv names the variable holding the user's own API key (Bare).
	APIKeyEnv string
	WorkDir   string
	Environ   func() []string
}

// HostCLIDefault is the default host's CLI: its own name, not a Kenaz one.
const HostCLIDefault = "claude"

// isolation keeps a worker call from being a session in any other sense:
// nothing is persisted, no tool or MCP server is available, no settings file
// is read (a user's settings may set OpenTelemetry export or hooks), and hooks
// and telemetry are off however the host was configured.
const isolation = `{"disableAllHooks":true,"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"0"}}`

func (h *HostCLI) args(r Request) []string {
	mode := "--safe-mode"
	if h.Bare {
		mode = "--bare"
	}
	args := []string{
		mode, "-p",
		"--output-format", "json",
		"--model", r.Model,
		"--system-prompt", r.System,
		"--tools", "",
		"--strict-mcp-config",
		"--setting-sources", "",
		"--settings", isolation,
		"--no-session-persistence",
		"--disable-slash-commands",
	}
	if r.MaxUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.4f", r.MaxUSD))
	}
	return args
}

// passEnv is what a worker call inherits. Everything else, including every
// OpenTelemetry setting, stays behind.
func passEnv(name string) bool {
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TEMP", "TMP", "LANG",
		"USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "SYSTEMROOT", "SystemRoot", "COMSPEC", "PATHEXT",
		"CLAUDE_CONFIG_DIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS":
		return true
	}
	return strings.HasPrefix(name, "LC_")
}

// passCloudEnv is what the bare mode needs for a cloud provider instead of a
// key.
func passCloudEnv(name string) bool {
	for _, pre := range []string{"CLAUDE_CODE_USE_", "AWS_", "GOOGLE_", "GCLOUD_", "CLOUD_ML_", "ANTHROPIC_VERTEX_", "ANTHROPIC_BEDROCK_", "ANTHROPIC_FOUNDRY_"} {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}

// ValidKeyEnv refuses a variable that cannot be an API key: one named for a login or
// session, one the program needs for something else. A name proves little, so
// isAPIKeyValue checks what the variable holds as well.
func ValidKeyEnv(name string) bool {
	n := strings.ToUpper(name)
	if n == "" || passEnv(name) {
		return false
	}
	for _, bad := range []string{"OAUTH", "O_AUTH", "AUTH", "TOKEN", "SESSION", "REFRESH", "BEARER"} {
		if strings.Contains(n, bad) {
			return false
		}
	}
	return true
}

// isAPIKeyValue reports a value shaped like an Anthropic API key, the only thing the
// API-key writer passes on. No base URL is passed, so nothing else could work anyway.
func isAPIKeyValue(v string) bool {
	return strings.HasPrefix(v, "sk-ant-api")
}

// allowedEnv reduces an environment to what keep admits. Values of anything else
// are never held.
func allowedEnv(environ []string, keep func(name string) bool) map[string]string {
	vars := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && keep(k) {
			vars[k] = v
		}
	}
	return vars
}

// WorkerEnviron is the environment the detached worker starts with: the same allowlist
// its model calls get, the writer's own credential if it has one, and extra names
// the program needs for itself. Nothing else reaches the worker.
func WorkerEnviron(environ []string, s Settings, extra func(name string) bool) []string {
	vars := allowedEnv(environ, func(k string) bool {
		return passEnv(k) || extra(k) || (s.Writer == WriterAPIKey && (k == s.APIKeyEnv && ValidKeyEnv(k) || passCloudEnv(k)))
	})
	out := []string{WorkerEnv + "=1"}
	for k, v := range vars {
		if k == s.APIKeyEnv && !passEnv(k) && !isAPIKeyValue(v) {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}

func (h *HostCLI) env() ([]string, error) {
	environ := os.Environ
	if h.Environ != nil {
		environ = h.Environ
	}
	vars := allowedEnv(environ(), func(k string) bool {
		return passEnv(k) || (h.Bare && (passCloudEnv(k) || (k == h.APIKeyEnv && ValidKeyEnv(k))))
	})
	out := []string{WorkerEnv + "=1", "CLAUDE_CODE_ENABLE_TELEMETRY=0", "OTEL_SDK_DISABLED=true"}
	cloud := false
	for k, v := range vars {
		if strings.HasPrefix(k, "CLAUDE_CODE_USE_") && v != "" && v != "0" {
			cloud = true
		}
	}
	for k, v := range vars {
		switch {
		case passEnv(k):
			out = append(out, k+"="+v)
		case h.Bare && cloud && passCloudEnv(k):
			out = append(out, k+"="+v)
		}
	}
	if h.Bare {
		key := vars[h.APIKeyEnv]
		if key != "" && !isAPIKeyValue(key) {
			return nil, &Failure{Code: FailNeedsKey}
		}
		if key == "" && !cloud {
			return nil, &Failure{Code: FailNeedsKey}
		}
		if key != "" {
			out = append(out, DefaultAPIKeyEnv+"="+key)
		}
	}
	return out, nil
}

// Complete runs one request. The prompt travels on stdin, never in the
// argument list, where other users of the machine could read it. stderr is
// discarded: it can quote the input.
func (h *HostCLI) Complete(ctx context.Context, r Request) (Response, error) {
	path := h.Path
	if path == "" {
		path = HostCLIDefault
	}
	bin, err := exec.LookPath(path)
	if err != nil {
		return Response{}, &Failure{Code: FailCLIMissing}
	}
	env, err := h.env()
	if err != nil {
		return Response{}, err
	}
	out, err := runProcess(ctx, bin, h.args(r), env, h.WorkDir, r.User)
	if err != nil && failCode(err) != FailExit {
		return Response{}, err
	}
	var res struct {
		IsError bool    `json:"is_error"`
		Status  int     `json:"api_error_status"`
		Result  string  `json:"result"`
		Cost    float64 `json:"total_cost_usd"`
		Usage   struct {
			In    int64 `json:"input_tokens"`
			Out   int64 `json:"output_tokens"`
			Write int64 `json:"cache_creation_input_tokens"`
			Read  int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(out, &res) != nil {
		if err != nil {
			return Response{}, err
		}
		return Response{}, &Failure{Code: FailBadOutput}
	}
	if err != nil || res.IsError {
		return Response{}, &Failure{Code: classifyHostError(res.Status, res.Result)}
	}
	u := Usage{In: res.Usage.In + res.Usage.Write + res.Usage.Read, Out: res.Usage.Out}
	if u.In == 0 {
		// A host that reports nothing still counts against the cap.
		u.In = approxTokens(r.System + r.User)
	}
	if u.Out == 0 {
		u.Out = approxTokens(res.Result)
	}
	if res.Cost > 0 {
		u.USD, u.Reported = res.Cost, true
	} else {
		u.USD = estimateUSD(u.In, u.Out)
	}
	return Response{Text: res.Result, Usage: u}, nil
}

// classifyHostError turns a host's error into a code. The text is read only for
// this and goes no further.
func classifyHostError(status int, text string) string {
	t := strings.ToLower(text)
	switch {
	case status == 429 || strings.Contains(t, "rate limit") || strings.Contains(t, "usage limit") || strings.Contains(t, "hit your limit") || strings.Contains(t, "limit reached"):
		return FailRateLimited
	case status == 401 || status == 403 || strings.Contains(t, "not logged in") || strings.Contains(t, "please run /login") || strings.Contains(t, "invalid api key"):
		return FailRefused
	}
	return FailExit
}

// approxTokens is the usual four-bytes-a-token rule of thumb.
func approxTokens(s string) int64 { return int64(len(s)/4) + 1 }

// Command runs a command the user named, for any other model, such as a
// local one. It gets the whole request on stdin and prints the answer. Usage
// is estimated: there is no way to know its price, so USD is zero.
type Command struct {
	Line    string
	WorkDir string
	Environ func() []string
}

// Complete runs one request.
func (c *Command) Complete(ctx context.Context, r Request) (Response, error) {
	f := strings.Fields(c.Line)
	if len(f) == 0 {
		return Response{}, &Failure{Code: FailCLIMissing}
	}
	bin, err := exec.LookPath(f[0])
	if err != nil {
		return Response{}, &Failure{Code: FailCLIMissing}
	}
	environ := os.Environ
	if c.Environ != nil {
		environ = c.Environ
	}
	env := []string{WorkerEnv + "=1", "OTEL_SDK_DISABLED=true"}
	for k, v := range allowedEnv(environ(), passEnv) {
		env = append(env, k+"="+v)
	}
	input := r.System + "\n\n" + r.User
	out, err := runProcess(ctx, bin, f[1:], env, c.WorkDir, input)
	if err != nil {
		return Response{}, err
	}
	return Response{Text: string(out), Usage: Usage{In: approxTokens(input), Out: approxTokens(string(out))}}, nil
}

// runProcess runs one bounded child. Its failures become codes only.
func runProcess(ctx context.Context, bin string, args, env []string, dir, stdin string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env, cmd.Dir, cmd.Stdin = env, dir, strings.NewReader(stdin)
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &limitWriter{w: &out, n: maxCallOutput}
	cmd.Stderr = io.Discard
	detach(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, &Failure{Code: FailTimeout}
	}
	if err != nil {
		// A host error can come with a JSON result on stdout that names it.
		return out.Bytes(), &Failure{Code: FailExit}
	}
	return out.Bytes(), nil
}

// limitWriter discards what is past its limit, so a runaway child cannot
// grow the worker without bound.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	full := len(p)
	if l.n <= 0 {
		return full, nil
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.w.Write(p)
	l.n -= n
	if err != nil {
		return n, err
	}
	return full, nil
}

// NewSummarizer builds the model backend of a background writer, or nil for a
// writer that has none.
func NewSummarizer(s Settings, workDir string) Summarizer {
	switch s.Writer {
	case WriterAPIKey:
		return &HostCLI{Path: s.HostCLI, Bare: true, APIKeyEnv: s.APIKeyEnv, WorkDir: workDir}
	case WriterHostCLI:
		return &HostCLI{Path: s.HostCLI, WorkDir: workDir}
	case WriterLocal:
		return &Command{Line: s.Command, WorkDir: workDir}
	}
	return nil
}
