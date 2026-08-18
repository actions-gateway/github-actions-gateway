// Command backlogmetrics replays the backlog's git history into per-item events
// and summary flow metrics. It is the reporter behind
// scripts/docs/backlog-metrics.sh, which runs the git logs and hands them over
// (Q614).
//
// The backlog process makes this possible without any recording step: every
// mutation is an isolated commit and IDs are stable, so cumulative arrivals
// fall out of the highest ID ever seen.
//
// # Two eras, one series
//
// The backlog was a single table at docs/STATUS.md until Q889 moved it to one
// file per item under docs/queue/. Both eras produce the same event — an ID
// arriving, an ID leaving — so the series is continuous across the move, and
// the seam is reported rather than hidden: the summary prints an era boundary
// at the cutover date and the event stream carries an `era` column, so a chart
// over either can draw its delimiter where the storage changed.
//
// Two bulk commits at the seam are storage, not flow, and both are suppressed:
// the migration adds every live item under docs/queue/ at once (each keeps the
// table-era filing date it already has), and the cutover deletes docs/STATUS.md
// with every remaining row in it, which would otherwise book the whole open
// backlog as resolved on one day. -cutover names that date; without it there is
// no seam and only the table era is read.
//
// # Reading each era
//
// Table era, on stdin: `git log --reverse -p --format='@COMMIT %as %s'` over
// docs/STATUS.md. Within one commit, an ID present in both a `-` and a `+` line
// is an edit (reorder, status flip, table move) — not an add or a removal.
//
// A diff line is a table row with no table around it, which is why the replay
// reads cells through markdown.ParseRow rather than splitting on `|`: one
// escaped pipe in any cell shifts every positional field after it. Only
// Queue/Deferred rows repeat the bare ID right after the anchor
// (`<a id="Q123"></a>Q123`), so an ID cell reading anything else — a
// Progress-table row's plan link — is not an item (Q509).
//
// Store era, from -store-log: `git log --reverse --name-status
// --format='@COMMIT %as %s'` over docs/queue. One path per line, so an add and
// a delete are the statuses git already reports and no parsing is involved.
//
// Removal reasons come from the docs(status) commit-subject verbs
// (complete/prune/merge/defer); anything else is counted as "removed" — adopt
// the verb vocabulary to make throughput honest.
//
// Usage:
//
//	git log … | backlogmetrics [-events] [-status <STATUS.md>] \
//	    [-store <docs/queue>] [-store-log <file>] [-cutover <YYYY-MM-DD>]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

const dateLayout = "2006-01-02"

func main() {
	events := flag.Bool("events", false, "print the TSV event stream instead of the summary")
	status := flag.String("status", "", "path to the table-era backlog file, read for the Deferred table")
	store := flag.String("store", "", "path to the item store, read for parked items")
	storeLog := flag.String("store-log", "", "file holding the store era's --name-status git log")
	cutover := flag.String("cutover", "", "date docs/STATUS.md was deleted; the seam between the eras")
	today := flag.String("today", time.Now().Format(dateLayout), "date open items are aged against")
	flag.Parse()

	r := &replay{filed: map[string]*item{}, removed: map[string]*removal{}, cutover: *cutover}
	if err := r.read(os.Stdin, tableEra); err != nil {
		fail(err)
	}
	if *storeLog != "" {
		f, err := os.Open(*storeLog)
		if err != nil {
			fail(err)
		}
		defer func() { _ = f.Close() }()
		if err := r.read(f, storeEra); err != nil {
			fail(err)
		}
	}

	sm, err := storeMeta(*store)
	if err != nil {
		fail(err)
	}
	r.fill(sm)
	parked, err := parkedIDs(*status, sm)
	if err != nil {
		fail(err)
	}

	out := bufio.NewWriter(os.Stdout)
	if *events {
		r.writeEvents(out, *today)
	} else {
		r.writeSummary(out, parked)
	}
	if err := out.Flush(); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "backlog-metrics: %v\n", err)
	os.Exit(2)
}

// era names which storage an event was read out of. It is carried per item so
// the seam stays visible in the output rather than being spliced away.
type era string

const (
	tableEra era = "table"
	storeEra era = "store"
)

type item struct {
	id, filed, size, title string
	era                    era
}

type removal struct {
	date, reason string
	era          era
}

type replay struct {
	filed   map[string]*item
	removed map[string]*removal
	maxID   int
	// The date docs/STATUS.md was deleted. Removals booked by the table era on
	// that date are the deletion itself, not resolved work.
	cutover string

	// One commit's state, resolved when the next @COMMIT line arrives.
	era           era
	date, subject string
	added, gone   map[string]bool
	sizes, titles map[string]string
}

var (
	qIDRE    = regexp.MustCompile(`^Q[0-9]+$`)
	commitRE = regexp.MustCompile(`^@COMMIT (\S+) ?(.*)$`)

	completedRE = regexp.MustCompile(`[Cc]omplet|[Ff]inish|[Ss]hip|[Dd]one`)
	prunedRE    = regexp.MustCompile(`[Pp]rune|[Ss]tale|[Dd]rop|[Zz]ombie`)
	mergedRE    = regexp.MustCompile(`[Mm]erge Q|[Dd]edup`)
	deferredRE  = regexp.MustCompile(`[Dd]efer`)
)

func classify(subject string) string {
	switch {
	case completedRE.MatchString(subject):
		return "completed"
	case prunedRE.MatchString(subject):
		return "pruned"
	case mergedRE.MatchString(subject):
		return "merged"
	case deferredRE.MatchString(subject):
		return "deferred"
	}
	return "removed"
}

func (r *replay) read(in io.Reader, from era) error {
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	r.era = from
	r.startCommit("", "")
	for s.Scan() {
		line := s.Text()
		if m := commitRE.FindStringSubmatch(line); m != nil {
			r.flush()
			r.startCommit(m[1], m[2])
			continue
		}
		if from == storeEra {
			r.readStoreLine(line)
			continue
		}
		r.readTableLine(line)
	}
	r.flush()
	return s.Err()
}

// readTableLine reads one line of a `-p` diff over docs/STATUS.md: a row the
// diff touched, with its marker still on (`+|` or `-|`). A context line's
// leading space excludes it, as does a `---`/`+++` file header.
func (r *replay) readTableLine(line string) {
	if len(line) < 2 || line[1] != '|' || (line[0] != '+' && line[0] != '-') {
		return
	}
	row, ok := markdown.ParseRow(line[1:])
	if !ok {
		return
	}
	cells := row.Text
	i := idCell(cells)
	if i < 0 {
		return
	}
	id := cells[i]
	if line[0] == '-' {
		r.gone[id] = true
		return
	}
	r.added[id] = true
	rest := cells[i+1:]
	if len(rest) > 0 {
		r.titles[id] = rest[0]
	}
	for _, c := range rest {
		if c == "S" || c == "M" || c == "L" {
			r.sizes[id] = c
			break
		}
	}
}

// readStoreLine reads one `--name-status` line over docs/queue: a status letter,
// a tab, and the path. A rename (`R100\told\tnew`) is a re-rank or a retitle
// under a stable filename here, so only the head letter is consulted and an
// `R`/`M` is neither an arrival nor a removal.
func (r *replay) readStoreLine(line string) {
	status, path, ok := strings.Cut(line, "\t")
	if !ok || status == "" {
		return
	}
	// A rename reports two paths; the one that survives is the last field.
	if i := strings.LastIndexByte(path, '\t'); i >= 0 {
		path = path[i+1:]
	}
	id := strings.TrimSuffix(filepath.Base(path), ".md")
	if !qIDRE.MatchString(id) {
		return
	}
	switch status[0] {
	case 'A':
		r.added[id] = true
	case 'D':
		r.gone[id] = true
	}
}

// idCell returns the index of the row's ID cell — the first cell that renders
// as nothing but a bare Q-ID, which is what `<a id="Q123"></a>Q123` renders as.
// Reports -1 when the row has none.
//
// It is not pinned to cell 0: history holds rows a botched edit prefixed with a
// stray delimiter (`|---|---|---|---|| <a id="Q166">…`), and dropping those
// would book a removal for a row that never left. Searching every cell is safe
// because a Progress-table row carries its ID only inside prose or a link, so
// no cell of one renders as the bare ID (Q509).
func idCell(cells []string) int {
	for i, c := range cells {
		if qIDRE.MatchString(c) {
			return i
		}
	}
	return -1
}

func (r *replay) startCommit(date, subject string) {
	r.date, r.subject = date, subject
	r.added, r.gone = map[string]bool{}, map[string]bool{}
	r.sizes, r.titles = map[string]string{}, map[string]string{}
}

// flush resolves one commit. An ID on both sides is an edit to a row that
// stayed, so it is neither an arrival nor a removal.
//
// Two seam rules keep the storage move out of the flow numbers. An item already
// filed in the table era keeps that filing date, so the migration's bulk add
// under docs/queue/ books no arrivals; and a table-era removal dated on the
// cutover is docs/STATUS.md being deleted with every remaining row in it, which
// would otherwise resolve the whole open backlog on one day.
func (r *replay) flush() {
	for id := range r.added {
		if r.gone[id] {
			continue
		}
		if _, seen := r.filed[id]; !seen {
			r.filed[id] = &item{id: id, filed: r.date, size: r.sizes[id], title: r.titles[id], era: r.era}
		}
		if n := idNum(id); n > r.maxID {
			r.maxID = n
		}
	}
	if r.era == tableEra && r.cutover != "" && r.date == r.cutover {
		return
	}
	for id := range r.gone {
		if !r.added[id] {
			r.removed[id] = &removal{date: r.date, reason: classify(r.subject), era: r.era}
		}
	}
}

// fill supplies the title and size for items whose arrival was read out of the
// store era, where the log carries a path and nothing else. A table-era item
// already has both off the row the diff showed, and keeps them: its title at
// filing time is the historical fact, and the store may since have rewritten it
// to fit the 72-character cap (Q889 rewrote 62 of them).
func (r *replay) fill(sm map[string]*meta) {
	for id, it := range r.filed {
		m := sm[id]
		if m == nil {
			continue
		}
		if it.title == "" {
			it.title = m.title
		}
		if it.size == "" {
			it.size = m.size
		}
	}
}

// sorted returns the filed items in Q-ID order. The awk this replaces iterated
// a hash, so its event stream came out in an arbitrary order.
func (r *replay) sorted() []*item {
	out := make([]*item, 0, len(r.filed))
	for _, it := range r.filed {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return idNum(out[i].id) < idNum(out[j].id) })
	return out
}

func idNum(id string) int {
	n, _ := strconv.Atoi(id[1:])
	return n
}

// daysBetween counts whole days from a to b, both `YYYY-MM-DD`.
func daysBetween(a, b string) int {
	from, err1 := time.Parse(dateLayout, a)
	to, err2 := time.Parse(dateLayout, b)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(to.Sub(from).Hours() / 24)
}

// writeEvents prints the per-item stream. `era` names the storage the item was
// filed out of, so a chart over this can draw its delimiter at the cutover
// instead of presenting one continuous run of days that spans a storage change.
func (r *replay) writeEvents(w io.Writer, today string) {
	_, _ = fmt.Fprintln(w, "id\tfiled\tremoved\tdays_open\treason\tera\tsize\ttitle")
	for _, it := range r.sorted() {
		end, reason := today, "open"
		if rm := r.removed[it.id]; rm != nil {
			end, reason = rm.date, rm.reason
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			it.id, it.filed, r.removedDate(it.id), daysBetween(it.filed, end), reason, it.era, it.size, it.title)
	}
}

func (r *replay) removedDate(id string) string {
	if rm := r.removed[id]; rm != nil {
		return rm.date
	}
	return ""
}

func (r *replay) writeSummary(w io.Writer, deferred map[string]bool) {
	// Cumulative arrivals. This used to read the **Next ID:** counter line, but
	// that line is gone (IDs are allocated from refs/queue-ids/*), and parsing
	// it from history would freeze at its final value. The highest ID ever seen
	// in the replay tracks arrivals on its own.
	counter := r.maxID + 1

	var open []*item
	var cycle []int
	nDone, nPruned, nOther := 0, 0, 0
	for _, it := range r.sorted() {
		rm := r.removed[it.id]
		switch {
		case rm == nil:
			open = append(open, it)
		case rm.reason == "completed":
			nDone++
			cycle = append(cycle, daysBetween(it.filed, rm.date))
		case rm.reason == "pruned":
			nPruned++
		default:
			nOther++
		}
	}

	_, _ = fmt.Fprintf(w, "backlog metrics — high-water Q%d, %d items ever filed\n\n", counter, len(r.filed))
	r.writeSeam(w)
	_, _ = fmt.Fprintf(w, "  open now:        %d\n", len(open))
	_, _ = fmt.Fprintf(w, "  completed:       %d\n", nDone)
	ratio := 0.0
	if resolved := nDone + nPruned + nOther; resolved > 0 {
		ratio = 100 * float64(nPruned) / float64(resolved)
	}
	_, _ = fmt.Fprintf(w, "  pruned:          %d  (prune ratio %.0f%% of resolved)\n", nPruned, ratio)
	if nOther > 0 {
		_, _ = fmt.Fprintf(w, "  other removals:  %d  (no verb in commit subject — adopt complete/prune/merge/defer)\n", nOther)
	}
	if nDone > 0 {
		sort.Ints(cycle)
		med := float64(cycle[nDone/2])
		if nDone%2 == 0 {
			med = float64(cycle[nDone/2-1]+cycle[nDone/2]) / 2
		}
		sum := 0
		for _, d := range cycle {
			sum += d
		}
		_, _ = fmt.Fprintf(w, "  cycle time:      median %.0f days, mean %.1f days (filed -> completed)\n",
			med, float64(sum)/float64(nDone))
	}
	_, _ = fmt.Fprintln(w)

	var aging []*item
	nDeferred := 0
	for _, it := range open {
		if deferred[it.id] {
			nDeferred++
			continue
		}
		aging = append(aging, it)
	}
	if nDeferred > 0 {
		_, _ = fmt.Fprintf(w, "  parked in Deferred: %d (excluded from aging WIP)\n", nDeferred)
	}
	if len(aging) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "  aging WIP (open Queue rows by ID gap — the groom staleness signal):")
	for _, it := range aging {
		_, _ = fmt.Fprintf(w, "    %-6s gap %-4d filed %s  %s %s\n",
			it.id, counter-idNum(it.id), it.filed, it.size, truncate(it.title, 60))
	}
}

// writeSeam names the storage move and splits the arrivals either side of it.
// The totals above it are the continuous series; this says where the delimiter
// goes, so a reader is never left inferring that one run of days spans two
// storage layouts. Silent before the cutover, when there is only one era.
func (r *replay) writeSeam(w io.Writer) {
	if r.cutover == "" {
		return
	}
	nTable, nStore := 0, 0
	for _, it := range r.filed {
		if it.era == storeEra {
			nStore++
			continue
		}
		nTable++
	}
	_, _ = fmt.Fprintf(w, "  ─── docs/STATUS.md: %d filed ─── %s cutover ─── docs/queue/: %d filed ───\n\n",
		nTable, r.cutover, nStore)
}

// truncate cuts a title to n bytes, which is what the awk's substr() counted on
// this host.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// parkedIDs returns the IDs parked awaiting a trigger, from whichever storage
// is live. They are excluded from aging WIP: they were parked by an explicit
// decision, and aging measures items awaiting one.
//
// Both sources are optional and an absent one is not an error — the table is
// gone after the cutover, and before it there is no store. Neither present
// means nothing is parked, which is the honest answer for a fresh backlog and
// the one a throwaway test repo gives.
func parkedIDs(statusPath string, sm map[string]*meta) (map[string]bool, error) {
	ids := map[string]bool{}
	if statusPath != "" {
		src, err := os.ReadFile(statusPath)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			deferredRows(markdown.Parse(src), ids)
		}
	}
	for id, m := range sm {
		if m.parked {
			ids[id] = true
		}
	}
	return ids, nil
}

// meta is what an item's own file says about it, for the fields the store era's
// `--name-status` log cannot carry. The table era read them off the diff'd row.
type meta struct {
	title, size string
	parked      bool
}

// storeMeta reads every item in the store. An absent directory yields nothing,
// which is the state before the migration.
func storeMeta(dir string) (map[string]*meta, error) {
	out := map[string]*meta{}
	if dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".md")
		if !qIDRE.MatchString(id) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		m := &meta{parked: frontmatterField(src, "status") == "deferred"}
		m.title = frontmatterField(src, "title")
		m.size = strings.ToUpper(frontmatterField(src, "size"))
		out[id] = m
	}
	return out, nil
}

// frontmatterField reads one scalar out of the item's YAML frontmatter. Scalar
// only, and quotes are stripped: the fields read here are a status enum, a size
// letter and a title, none of which is a list or a nested map.
func frontmatterField(src []byte, name string) string {
	m := regexp.MustCompile(`(?m)^` + name + `:[ \t]*(.*)$`).FindSubmatch(src)
	if m == nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(string(m[1])), `"'`)
}

// deferredRows collects the IDs in the table era's Deferred section.
func deferredRows(doc *markdown.Document, into map[string]bool) {
	start, end, ok := doc.SectionRange(2, "Deferred")
	if !ok {
		return
	}
	for _, table := range doc.Tables() {
		for _, row := range table.Rows {
			if row.Line < start || row.Line > end || len(row.Text) == 0 {
				continue
			}
			if qIDRE.MatchString(row.Text[0]) {
				into[row.Text[0]] = true
			}
		}
	}
}
