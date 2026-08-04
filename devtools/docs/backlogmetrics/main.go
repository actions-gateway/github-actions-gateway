// Command backlogmetrics replays the backlog file's git history into per-item
// events and summary flow metrics. It is the reporter behind
// scripts/docs/backlog-metrics.sh, which runs the git log and hands it over
// (Q614).
//
// The backlog process makes this possible without any recording step: every
// mutation is an isolated commit to one file and IDs are stable, so cumulative
// arrivals fall out of the highest ID ever seen.
//
// Input on stdin is `git log --reverse -p --format='@COMMIT %as %s'` over the
// backlog file. Within one commit, an ID present in both a `-` and a `+` line
// is an edit (reorder, status flip, table move) — not an add or a removal.
//
// A diff line is a table row with no table around it, which is why the replay
// reads cells through markdown.ParseRow rather than splitting on `|`: one
// escaped pipe in any cell shifts every positional field after it. Only
// Queue/Deferred rows repeat the bare ID right after the anchor
// (`<a id="Q123"></a>Q123`), so an ID cell reading anything else — a
// Progress-table row's plan link — is not an item (Q509).
//
// Removal reasons come from the docs(status) commit-subject verbs
// (complete/prune/merge/defer); anything else is counted as "removed" — adopt
// the verb vocabulary to make throughput honest.
//
// Usage:
//
//	git log … | backlogmetrics [-events] -status <path/to/STATUS.md>
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

const dateLayout = "2006-01-02"

func main() {
	events := flag.Bool("events", false, "print the TSV event stream instead of the summary")
	status := flag.String("status", "", "path to the backlog file, read for the Deferred table")
	today := flag.String("today", time.Now().Format(dateLayout), "date open items are aged against")
	flag.Parse()

	src, err := os.ReadFile(*status)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backlog-metrics: %v\n", err)
		os.Exit(2)
	}

	r := &replay{filed: map[string]*item{}, removed: map[string]*removal{}}
	if err := r.read(os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "backlog-metrics: %v\n", err)
		os.Exit(2)
	}

	out := bufio.NewWriter(os.Stdout)
	if *events {
		r.writeEvents(out, *today)
	} else {
		r.writeSummary(out, deferredIDs(markdown.Parse(src)))
	}
	if err := out.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "backlog-metrics: %v\n", err)
		os.Exit(2)
	}
}

type item struct {
	id, filed, size, title string
}

type removal struct {
	date, reason string
}

type replay struct {
	filed   map[string]*item
	removed map[string]*removal
	maxID   int

	// One commit's state, resolved when the next @COMMIT line arrives.
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

func (r *replay) read(in io.Reader) error {
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	r.startCommit("", "")
	for s.Scan() {
		line := s.Text()
		if m := commitRE.FindStringSubmatch(line); m != nil {
			r.flush()
			r.startCommit(m[1], m[2])
			continue
		}
		// A row a diff touched, with its marker still on: `+|` or `-|`. A
		// context line's leading space excludes it, as does a `---`/`+++` file
		// header.
		if len(line) < 2 || line[1] != '|' || (line[0] != '+' && line[0] != '-') {
			continue
		}
		row, ok := markdown.ParseRow(line[1:])
		if !ok {
			continue
		}
		cells := row.Text
		i := idCell(cells)
		if i < 0 {
			continue
		}
		id := cells[i]
		if line[0] == '-' {
			r.gone[id] = true
			continue
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
	r.flush()
	return s.Err()
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
func (r *replay) flush() {
	for id := range r.added {
		if r.gone[id] {
			continue
		}
		if _, seen := r.filed[id]; !seen {
			r.filed[id] = &item{id: id, filed: r.date, size: r.sizes[id], title: r.titles[id]}
		}
		if n := idNum(id); n > r.maxID {
			r.maxID = n
		}
	}
	for id := range r.gone {
		if !r.added[id] {
			r.removed[id] = &removal{date: r.date, reason: classify(r.subject)}
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

func (r *replay) writeEvents(w io.Writer, today string) {
	_, _ = fmt.Fprintln(w, "id\tfiled\tremoved\tdays_open\treason\tsize\ttitle")
	for _, it := range r.sorted() {
		end, reason := today, "open"
		if rm := r.removed[it.id]; rm != nil {
			end, reason = rm.date, rm.reason
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			it.id, it.filed, r.removedDate(it.id), daysBetween(it.filed, end), reason, it.size, it.title)
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

// truncate cuts a title to n bytes, which is what the awk's substr() counted on
// this host.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// deferredIDs returns the IDs parked in the Deferred table. They are excluded
// from aging WIP: they were parked by an explicit decision, and aging measures
// rows awaiting one.
func deferredIDs(doc *markdown.Document) map[string]bool {
	ids := map[string]bool{}
	start, end, ok := doc.SectionRange(2, "Deferred")
	if !ok {
		return ids
	}
	for _, table := range doc.Tables() {
		for _, row := range table.Rows {
			if row.Line < start || row.Line > end || len(row.Text) == 0 {
				continue
			}
			if qIDRE.MatchString(row.Text[0]) {
				ids[row.Text[0]] = true
			}
		}
	}
	return ids
}
