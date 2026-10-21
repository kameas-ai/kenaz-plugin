package consent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// FirstRunResult reports what the member chose.
type FirstRunResult struct {
	Shared  []fleetapi.TelemetryClass // empty for "Not now"
	NotNow  bool
	NoneAvl bool // the plan includes no class; nothing was written
}

// RunFirstRun shows the Agent Telemetry Notice verbatim and lets the
// member choose classes. Every class starts unchecked. An
// admin pre-selection is only a tag ("suggested by your administrator"),
// never a default. Choosing classes writes all available classes:
// chosen ones on, the rest off. "Not now" (empty input) writes nothing.
//
// The caller must have refreshed m after login.
func RunFirstRun(ctx context.Context, m *Manager, vars NoticeVars, in io.Reader, out io.Writer) (FirstRunResult, error) {
	st := m.State()
	if !st.SignedIn {
		return FirstRunResult{}, fleetapi.ErrNotSignedIn
	}
	if vars.OrgName == "" {
		vars.OrgName = st.OrgDisplay
	}
	if vars.Edition.ID == "" {
		vars.Edition = m.NoticeEdition()
	}
	text, err := RenderNotice(vars)
	if err != nil {
		return FirstRunResult{}, err
	}
	fmt.Fprintln(out, text)
	fmt.Fprintln(out)

	type row struct {
		n  int
		ci ClassInfo
	}
	var rows []row
	n := 0
	for _, s := range m.OptInStates() {
		ci := classInfo(s.Class)
		switch {
		case !s.Available:
			fmt.Fprintf(out, "      %s (not available on your plan)\n", ci.Label)
		default:
			n++
			tag := ""
			if s.AdminSuggested {
				tag = " (suggested by your administrator)"
			}
			fmt.Fprintf(out, "  [ ] %d. %s%s\n", n, ci.Label, tag)
			rows = append(rows, row{n, ci})
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "\nNone of these are included in your plan. Nothing will be shared.")
		return FirstRunResult{NoneAvl: true}, nil
	}

	r := bufio.NewReader(in)
	fmt.Fprint(out, "\nShare selected: type the numbers to share (for example 1,2), or press Enter for Not now: ")
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" || strings.EqualFold(line, "not now") {
		fmt.Fprintln(out, "Not now. Nothing will be shared. You can choose later with `kenaz optin`.")
		return FirstRunResult{NotNow: true}, nil
	}
	chosen := map[fleetapi.TelemetryClass]bool{}
	for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
		k, err := strconv.Atoi(f)
		if err != nil || k < 1 || k > len(rows) {
			return FirstRunResult{}, fmt.Errorf("consent: %q is not one of the listed numbers", f)
		}
		chosen[rows[k-1].ci.Class] = true
	}
	var labels []string
	sel := map[fleetapi.TelemetryClass]bool{}
	var shared []fleetapi.TelemetryClass
	for _, rw := range rows {
		on := chosen[rw.ci.Class]
		sel[rw.ci.Class] = on
		if on {
			labels = append(labels, rw.ci.Label)
			shared = append(shared, rw.ci.Class)
		}
	}
	fmt.Fprintf(out, "Share %s with %s? [y/N]: ", strings.Join(labels, ", "), vars.OrgName)
	confirm, _ := r.ReadString('\n')
	if c := strings.ToLower(strings.TrimSpace(confirm)); c != "y" && c != "yes" {
		fmt.Fprintln(out, "Not now. Nothing will be shared.")
		return FirstRunResult{NotNow: true}, nil
	}
	if err := m.SetOptIns(ctx, sel); err != nil {
		return FirstRunResult{}, err
	}
	fmt.Fprintln(out, "Saved. Change this any time with `kenaz optin`.")
	return FirstRunResult{Shared: shared}, nil
}

func classInfo(c fleetapi.TelemetryClass) ClassInfo {
	for _, ci := range Classes {
		if ci.Class == c {
			return ci
		}
	}
	return ClassInfo{c, string(c)}
}
