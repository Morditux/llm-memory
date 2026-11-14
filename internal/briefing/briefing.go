// Package briefing composes the Markdown text returned by get_context: what
// a new LLM session needs to pick up the work where the previous one left it.
package briefing

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Morditux/llm-memory/internal/store"
)

// Budget bounds (in characters).
const (
	DefaultMaxChars = 6000
	MinMaxChars     = 400 // smaller budgets are raised to this value
	maxLineRunes    = 240 // a single list item is cut beyond this length
	maxRecent       = 10  // decisions and facts shown at most
	dateLayout      = "2006-01-02 15:04 UTC"
)

// Options configures Build.
type Options struct {
	Namespace string // default "global"
	MaxChars  int    // default 6000, minimum 400
}

// Build loads the namespace and renders its briefing. It returns a
// *store.NamespaceNotFoundError if the namespace does not exist.
func Build(ctx context.Context, st *store.Store, opt Options) (string, error) {
	nsName := opt.Namespace
	if strings.TrimSpace(nsName) == "" {
		nsName = store.DefaultNamespace
	}
	ns, err := st.RequireNamespace(ctx, nsName)
	if err != nil {
		return "", err
	}
	d := data{namespace: ns}
	if d.active, err = st.List(ctx, store.ListFilter{Namespace: ns}); err != nil {
		return "", err
	}
	last, err := st.List(ctx, store.ListFilter{Namespace: ns, Archived: store.IncludeArchived, Limit: 1})
	if err != nil {
		return "", err
	}
	if len(last) > 0 {
		d.last = &last[0]
	}
	if ns != store.DefaultNamespace {
		if d.global, err = st.List(ctx, store.ListFilter{Namespace: store.DefaultNamespace}); err != nil {
			return "", err
		}
	}
	all, err := st.ListNamespaces(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range all {
		if n.Name != ns {
			d.others = append(d.others, n)
		}
	}
	return render(d, opt.MaxChars), nil
}

// data is everything render needs; render itself does no I/O.
type data struct {
	namespace string
	active    []store.Memory // active memories of the namespace, most recent first
	global    []store.Memory // active memories of "global" (empty when namespace is global)
	last      *store.Memory  // most recently written memory of the namespace, archived included
	others    []store.Namespace
}

func render(d data, maxChars int) string {
	switch {
	case maxChars <= 0:
		maxChars = DefaultMaxChars
	case maxChars < MinMaxChars:
		maxChars = MinMaxChars
	}
	w := &writer{max: maxChars}
	isGlobal := d.namespace == store.DefaultNamespace

	// 1. Header (always written).
	w.force(fmt.Sprintf("# Shared memory — namespace \"%s\"\n\n", d.namespace))
	status := plural(len(d.active), "active memory", "active memories")
	if d.last != nil {
		status += fmt.Sprintf(" · last activity %s (%s)", date(d.last.UpdatedAt), d.last.UpdatedBy)
	}
	w.force(status + "\n")
	w.force("Use the ids (#42) with get_memory, update_memory and forget.\n")

	if len(d.active) == 0 && len(d.global) == 0 {
		w.force(welcome)
		w.footer(d.others)
		return w.String()
	}
	if len(d.active) == 0 {
		w.add("\nNo active memory in this namespace yet: use remember to create one.\n")
	}

	shown := map[int64]bool{} // already placed in an earlier section

	// 2. Latest active handoff, full text.
	for _, m := range d.active {
		if m.Kind == store.KindHandoff {
			shown[m.ID] = true
			w.handoff(m)
			break
		}
	}

	// 3. Importance 3, all kinds (namespace, then global).
	var imp []string
	for _, src := range []struct {
		ms     []store.Memory
		marker string
	}{{d.active, ""}, {d.global, " (global)"}} {
		for _, m := range src.ms {
			if m.Importance == 3 && m.Kind != store.KindHandoff && !shown[m.ID] {
				shown[m.ID] = true
				imp = append(imp, item(m, fmt.Sprintf("[%s]%s ", m.Kind, src.marker), false))
			}
		}
	}
	w.section("Essential (importance 3)", imp, 0, "")

	// 4. Active task states.
	tasks, _ := pick(d.active, store.KindTaskState, shown, -1, false)
	w.section("Ongoing tasks", tasks, 0, "task_state")

	// 5. Preferences: namespace, then global.
	prefs, _ := pick(d.active, store.KindPreference, shown, -1, false)
	if isGlobal {
		w.section("Preferences", prefs, 0, "preference")
	} else {
		w.section("Project preferences", prefs, 0, "preference")
		gprefs, _ := pick(d.global, store.KindPreference, shown, -1, false)
		w.section("Global preferences", gprefs, 0, "preference")
	}

	// 6. Ten most recent decisions.
	decisions, more := pick(d.active, store.KindDecision, shown, maxRecent, false)
	w.section("Recent decisions", decisions, more, "decision")

	// 7. Ten most recent facts (title only when there is one).
	facts, more := pick(d.active, store.KindFact, shown, maxRecent, true)
	w.section("Recent facts", facts, more, "fact")

	// Remaining kinds (notes, older handoffs) are only counted.
	notes := 0
	for _, m := range d.active {
		if m.Kind == store.KindNote && !shown[m.ID] {
			notes++
		}
	}
	if notes > 0 {
		w.add(fmt.Sprintf("\n%s not shown: use recall (kinds: note).\n", plural(notes, "note", "notes")))
	}

	if w.omitted > 0 {
		w.add(fmt.Sprintf("\n(Budget reached: %s not shown, use recall.)\n", plural(w.omitted, "memory", "memories")))
	}

	// 8. Footer.
	w.footer(d.others)
	return w.String()
}

const welcome = `
No memory stored yet. This memory is shared with other assistants:
- remember: store a decision and its reason, a lasting preference or a stable fact about the project;
- recall: look information up before asking for it again;
- handoff: before ending a session, summarise the state, next steps and open questions;
- never store secrets in it (passwords, keys, tokens).
`

// pick formats the memories of kind k not yet shown, up to limit (-1 = all).
// It returns the lines and the number of further memories left out by limit.
func pick(ms []store.Memory, k store.Kind, shown map[int64]bool, limit int, titleOnly bool) ([]string, int) {
	var lines []string
	more := 0
	for _, m := range ms {
		if m.Kind != k || shown[m.ID] {
			continue
		}
		shown[m.ID] = true
		if limit >= 0 && len(lines) >= limit {
			more++
			continue
		}
		lines = append(lines, item(m, "", titleOnly))
	}
	return lines, more
}

// item renders one list line: "- #42 prefix **Title**: content".
func item(m store.Memory, prefix string, titleOnly bool) string {
	content := oneLine(m.Content)
	var body string
	switch {
	case m.Title != "" && titleOnly:
		body = "**" + m.Title + "**"
	case m.Title != "":
		body = "**" + m.Title + "**: " + content
	default:
		body = content
	}
	return cut(fmt.Sprintf("- #%d %s%s", m.ID, prefix, body), maxLineRunes) + "\n"
}

// ---- Budgeted writer --------------------------------------------------------

type writer struct {
	b       strings.Builder
	used    int
	max     int
	omitted int // memories left out because of the budget
}

func (w *writer) String() string { return w.b.String() }

func (w *writer) fits(s string) bool { return w.used+utf8.RuneCountInString(s) <= w.max }

// force writes s regardless of the budget (header only).
func (w *writer) force(s string) {
	w.b.WriteString(s)
	w.used += utf8.RuneCountInString(s)
}

func (w *writer) add(s string) bool {
	if !w.fits(s) {
		return false
	}
	w.force(s)
	return true
}

func omittedNote(n int, kind string) string {
	hint := ""
	if kind != "" {
		hint = " (kinds: " + kind + ")"
	}
	return fmt.Sprintf("- … %d more, use recall%s\n", n, hint)
}

// section writes a heading and as many lines as the budget allows, then a
// "… N more" note counting what was left out (by the budget or by the extra cap).
func (w *writer) section(title string, lines []string, extra int, kind string) {
	total := len(lines) + extra
	if total == 0 {
		return
	}
	heading := "\n## " + title + "\n"
	// The heading is written only if at least the "… N more" note fits after
	// it (or the single line, when there is nothing else to announce).
	single := total == 1 && len(lines) == 1 && w.fits(heading+lines[0])
	if !single && !w.fits(heading+omittedNote(total, kind)) {
		w.omitted += total
		return
	}
	w.force(heading)
	for i, line := range lines {
		rest := total - i - 1
		if w.fits(line) && (rest == 0 || w.fits(line+omittedNote(rest, kind))) {
			w.force(line)
			continue
		}
		// Fits: reserved when the previous line (or the heading) was accepted.
		w.force(omittedNote(total-i, kind))
		w.omitted += len(lines) - i
		return
	}
	if extra > 0 {
		w.add(omittedNote(extra, kind))
	}
}

// handoff writes the full handoff, truncated only if the budget requires it.
func (w *writer) handoff(m store.Memory) {
	heading := fmt.Sprintf("\n## Latest handoff (#%d, %s, %s)\n", m.ID, m.UpdatedBy, date(m.UpdatedAt))
	body := demoteHeadings(strings.TrimSpace(m.Content)) + "\n"
	if w.add(heading + body) {
		return
	}
	tail := fmt.Sprintf("… (truncated: get_memory #%d for the full text)\n", m.ID)
	room := w.max - w.used - utf8.RuneCountInString(heading+tail) - 1 // -1: newline before tail
	if room < 40 {
		if !w.add(fmt.Sprintf("\n## Latest handoff: #%d (no room left, get_memory #%d)\n", m.ID, m.ID)) {
			w.omitted++
		}
		return
	}
	r := []rune(body)
	w.force(heading + strings.TrimRight(string(r[:room]), " \n") + "\n" + tail)
}

func (w *writer) footer(others []store.Namespace) {
	if len(others) == 0 {
		return
	}
	parts := make([]string, len(others))
	for i, n := range others {
		parts[i] = fmt.Sprintf("%s (%d)", n.Name, n.ActiveCount)
	}
	w.add("\n---\nOther namespaces: " + strings.Join(parts, ", ") + ".\n")
}

// ---- Helpers ----------------------------------------------------------------

// demoteHeadings pushes Markdown headings of embedded text two levels down,
// so that "## Summary" inside a handoff does not compete with the briefing's
// own sections.
func demoteHeadings(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "#") {
			lines[i] = "##" + l
		}
	}
	return strings.Join(lines, "\n")
}

func date(t time.Time) string { return t.UTC().Format(dateLayout) }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// plural renders "1 memory" / "0 memories" / "3 memories".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
