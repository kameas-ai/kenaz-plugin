package capture

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/pkg/secretscan"
)

const (
	redactedCredential = "[redacted credential]"
	redactedPrivate    = "[private]"
	redactedEmail      = "[email]"
)

// secretName is any name that says its value is a secret. The short words count
// only as a whole word at the end of the name, so that bypass and author do not.
const secretName = `(?:[A-Za-z0-9_.-]*(?:password|passwd|pwd|passphrase|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|credential|client[_-]?secret|signing[_-]?key|encryption[_-]?key)[A-Za-z0-9_.-]*|\b(?:[A-Za-z0-9_.-]*[_.-])?(?:pass|auth)|\b(?-i:[A-Z0-9_]*_KEY)|\b(?:AccountKey|SharedAccessKey|SharedAccessSignature))`

// secretJSONKey is a JSON key whose whole value is a secret. A key here is a plain
// label, so these also take the words that are too common to judge in free text.
var secretJSONKey = regexp.MustCompile(`(?i)^(?:` + secretName + `|session|key|cookie)$`)

var (
	privateTag = regexp.MustCompile(`(?i)<(/?)private\b[^>]*>`)

	// Whole blocks go first, so that nothing later can leave part of one behind.
	// An unterminated block runs to the end of the text: when in doubt, keep less.
	pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z0-9 ]*-----.*?(?:-----END [A-Z0-9 ]*-----|\z)`)

	// valueCredentials take the value along with the name. The shared detector's
	// shapes match some of these names alone, which would leave the value behind.
	valueCredentials = []*regexp.Regexp{
		// A secret name takes the rest of its line: a value can hold spaces, and an
		// unquoted one has no other end.
		regexp.MustCompile(`(?i)` + secretName + `\\?["']?\s*[:=][^\n]*`),
		// A command line's own password flags.
		regexp.MustCompile(`(?i)(?:^|\s)--(?:password|passwd|token|secret|api[_-]?key)[= ]\s*\S+`),
		regexp.MustCompile(`(?:^|\s)(?:-u|--user)[ =]\S*:\S+`),
		regexp.MustCompile(`(?i)\b(?:mysql|mysqldump|mariadb)\b[^\n]*?\s-p\S+`),
		regexp.MustCompile(`(?i)\bsshpass\b[^\n]*?\s-p\s*\S+`),
		regexp.MustCompile(`(?i)\bredis-cli\b[^\n]*?\s-a\s*\S+`),
		regexp.MustCompile(`(?i)\bdocker\s+login\b[^\n]*?\s(?:-p|--password)[ =]?\S+`),
		regexp.MustCompile(`(?i)\b(?:authorization|cookie|set-cookie|x-api-key)\s*:\s*[^\r\n]+`),
		// A token as the user of a URL, and a signed URL's signature.
		regexp.MustCompile(`(?i)\bhttps?://[^/\s:@]+@`),
		regexp.MustCompile(`(?i)[?&]sig=[^&\s]+`),
	}

	// Shapes on top of the shared detector.
	tokenShapes = []*regexp.Regexp{
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{10,}`),
		regexp.MustCompile(`\bnpm_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`\bhf_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
		regexp.MustCompile(`\bya29\.[A-Za-z0-9_-]{10,}`),
		regexp.MustCompile(`\b(?:sk|rk|pk)_test_[A-Za-z0-9]{10,}`),
		regexp.MustCompile(`\bxapp-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`\bwhsec_[A-Za-z0-9+/=_-]{8,}`),
		regexp.MustCompile(`\bdop_v1_[A-Za-z0-9]{8,}`),
		regexp.MustCompile(`\bpypi-[A-Za-z0-9_-]{8,}`),
		regexp.MustCompile(`\bAGE-SECRET-KEY-1[A-Z0-9]{8,}`),
		regexp.MustCompile(`https://hooks\.slack\.com/services/\S+`),
		regexp.MustCompile(`\brt_[A-Za-z0-9_-]{16,}`),
	}

	// assignedCredential is a long unquoted value under a secret name: what model
	// output is dropped for. Prose that only mentions a secret name is not.
	assignedCredential = regexp.MustCompile(`(?i)` + secretName + `\\?["']?\s*[:=]\s*\\?["']?[^\s"',;]{6,}`)

	emailAddress = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)

	// Home directories, whoever's they are: a note must not name an account. The
	// spaced form needs a path after it, so that prose after a path stays.
	homeSpaced = regexp.MustCompile(`(?:/mnt/[a-z]/Users/|/Users/|/var/home/|/export/home/|/home/|[A-Za-z]:(?:\\{1,2}|/)Users(?:\\{1,2}|/))[\w.-]+(?: [\w.-]+){1,3}(?:[/\\])`)
	homeDir    = regexp.MustCompile(`(?:/mnt/[a-z]/Users/|/Users/|/var/home/|/export/home/|/home/|[A-Za-z]:(?:\\{1,2}|/)Users(?:\\{1,2}|/))[^/\\\s"'` + "`" + `:;,)\]}]+`)
	rootDir    = regexp.MustCompile(`(^|[\s"'=(:])/root(?:/|\b)`)
)

// maxFlatten bounds what is parsed as JSON.
const maxFlatten = 256 << 10

// Scrub removes what must not reach the summariser or a note: <private> blocks,
// credential shapes, email addresses and home directories. JSON text is reduced to
// its string values first, so that escaping cannot hide anything. It returns the
// clean text and how many credential shapes it redacted. It is idempotent. Like the
// shared detector it catches common shapes: a safety net, not a guarantee.
func Scrub(s, home string) (string, int) {
	before := strings.Count(s, redactedCredential)
	s = Flatten(s)
	s = strings.NewReplacer(`<`, "<", `>`, ">", `<`, "<", `>`, ">").Replace(s)
	s = stripPrivate(s)
	n := max(strings.Count(s, redactedCredential)-before, 0)
	redact := func(re *regexp.Regexp) {
		s = re.ReplaceAllStringFunc(s, func(string) string { n++; return redactedCredential })
	}
	redact(pemBlock)
	for _, re := range valueCredentials {
		redact(re)
	}
	for _, re := range secretscan.Patterns() {
		redact(re)
	}
	for _, re := range tokenShapes {
		redact(re)
	}
	for _, re := range secretscan.SyncPatterns() {
		redact(re)
	}
	s = emailAddress.ReplaceAllString(s, redactedEmail)
	s = homeSpaced.ReplaceAllString(s, "~/")
	s = homeDir.ReplaceAllString(s, "~")
	s = rootDir.ReplaceAllString(s, "$1~/")
	if home != "" && home != "/" {
		// As a whole path only: /h must not touch /host.
		if re, err := regexp.Compile(regexp.QuoteMeta(home) + `($|[/\\\s"'])`); err == nil {
			s = re.ReplaceAllString(s, "~$1")
		}
	}
	return s, n
}

// HasEmail reports an email address in s.
func HasEmail(s string) bool { return emailAddress.MatchString(s) }

// stripPrivate removes <private> blocks, counting nesting, whatever attributes the
// tag has. An unterminated block runs to the end.
func stripPrivate(s string) string {
	tags := privateTag.FindAllStringSubmatchIndex(s, -1)
	if tags == nil {
		return s
	}
	var b strings.Builder
	pos, depth := 0, 0
	for _, t := range tags {
		closing := t[3] > t[2]
		switch {
		case !closing && depth == 0:
			b.WriteString(s[pos:t[0]])
			b.WriteString(redactedPrivate)
			depth = 1
		case !closing:
			depth++
		case depth > 0:
			depth--
			if depth == 0 {
				pos = t[1]
			}
		}
	}
	if depth == 0 {
		b.WriteString(s[pos:])
	}
	return b.String()
}

// Flatten turns JSON text into plain lines of its string and number values, with
// their keys, decoding JSON held inside strings too. Anything else comes back as
// it is. Scrubbing then sees text as a person would read it.
func Flatten(s string) string {
	t := strings.TrimSpace(s)
	if len(t) < 2 || len(t) > maxFlatten || (t[0] != '{' && t[0] != '[' && t[0] != '"') {
		return s
	}
	var v any
	if json.Unmarshal([]byte(t), &v) != nil {
		return s
	}
	var lines []string
	flattenValue("", v, 0, &lines)
	return strings.Join(lines, "\n")
}

func flattenValue(key string, v any, depth int, out *[]string) {
	label := func(text string) string {
		if key == "" {
			return text
		}
		return key + ": " + text
	}
	// A secret name takes its whole value, whatever is in it: line breaks and
	// quotes would otherwise let part of it get past the patterns.
	if key != "" && secretJSONKey.MatchString(key) {
		if _, isBool := v.(bool); !isBool && v != nil {
			*out = append(*out, redactedCredential)
		}
		return
	}
	switch x := v.(type) {
	case string:
		if depth < 4 {
			t := strings.TrimSpace(x)
			if len(t) >= 2 && len(t) <= maxFlatten && (t[0] == '{' || t[0] == '[') {
				var inner any
				if json.Unmarshal([]byte(t), &inner) == nil {
					flattenValue(key, inner, depth+1, out)
					return
				}
			}
		}
		*out = append(*out, label(x))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			flattenValue(k, x[k], depth+1, out)
		}
	case []any:
		for _, item := range x {
			flattenValue(key, item, depth+1, out)
		}
	case nil:
	default:
		b, err := json.Marshal(x)
		if err == nil {
			*out = append(*out, label(string(b)))
		}
	}
}

// LooksSensitive reports whether model output still carries a credential shape.
// Output like that is dropped, not cleaned: a model that repeats a credential
// despite the redaction has seen something the scan missed.
func LooksSensitive(parts ...string) bool {
	joined := strings.Join(parts, "\n")
	if secretscan.LooksLikeCredentialForSync(joined) || pemBlock.MatchString(joined) || assignedCredential.MatchString(joined) {
		return true
	}
	for _, re := range tokenShapes {
		if re.MatchString(joined) {
			return true
		}
	}
	return false
}

// sensitivePath matches files whose whole content is credentials or keys, and
// commands that print them. A tool call that touches one is not captured at all.
var sensitivePath = regexp.MustCompile(`(?i)(?:^|[/\\\s"'=])(?:\.env(?:\.[\w.-]+)?|\.claude\.json|environ|\.netrc|\.npmrc|\.pypirc|\.pgpass|\.git-credentials|id_rsa|id_ed25519|id_ecdsa|id_dsa|\.?credentials(?:\.\w+)?|auth\.json|hosts\.yml|application_default_credentials\.json|secrets?(?:\.\w+)?|[\w.-]+\.(?:pem|p12|pfx|key|keystore|jks|keychain|keychain-db)|\.aws[/\\]+[^\s]*|\.ssh[/\\]+[^\s]*|\.gnupg[/\\]+[^\s]*|\.claude[/\\]+\.[^\s]*|\.claude[/\\]+settings(?:\.local)?\.json|\.codex[/\\]+[^\s]*|\.kube[/\\]+[^\s]*|\.docker[/\\]+config\.json|\.config[/\\]+(?:gh|gcloud)[/\\]+[^\s]*|Keychains[/\\]+[^\s]*)(?:$|[\s"';|&)>,}])`)

// credentialCommand matches commands whose output is the environment or a stored
// secret.
var credentialCommand = regexp.MustCompile(`(?:^|[;&|(":])\s*(?:sudo\s+)?(?:printenv|env|security\s+find-[\w-]*password|gh\s+auth\s+token|aws\s+configure\s+get|gcloud\s+auth\s+print-[\w-]*token)(?:$|[\s;&|)"])`)

func touchesSensitivePath(s string) bool {
	return sensitivePath.MatchString(s) || sensitivePath.MatchString(Flatten(s)) || credentialCommand.MatchString(Flatten(s))
}
