package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/devtools/git/keyedrecords"
	"github.com/actions-gateway/github-actions-gateway/devtools/git/mdregistry"
)

// dupeKeyFunc normalizes a row's link target for the whole-page duplicate
// check, and reports false for a target that is not in the keyed namespace at
// all. Each registry decides what "the same subject twice" means.
type dupeKeyFunc func(target string) (string, bool)

// registryDriver merges a Markdown page whose per-group tables are keyed on the
// link in column 1 — the same cell each page's own checker reads, so the driver
// and the gate agree by construction.
//
// The two pages that use it differ only in what counts as a duplicate, so the
// splitting, the per-table merge and the prose handling live here once. They
// were 44 identical lines of awk in each driver before the port.
type registryDriver struct {
	subject string // "script", "plan": names the key in every message
	dupeKey dupeKeyFunc
	noun    string // "registry", "index": names the page in every message
}

func (d registryDriver) run(in *invocation) {
	sides := map[string]string{"base": in.base, "ours": in.ours, "theirs": in.theirs}
	docs := make(map[string]*mdregistry.Doc, 3)
	for _, name := range []string{"base", "ours", "theirs"} {
		lines, err := readLines(sides[name])
		if err != nil {
			in.fallback("%s: could not be read", name)
		}
		doc, err := mdregistry.Split(lines)
		if err != nil {
			in.fallback("%s: the %s tables could not be located", name, d.noun)
		}
		docs[name] = doc
	}

	base, ours, theirs := docs["base"], docs["ours"], docs["theirs"]

	// A side that added or dropped a whole table has restructured the page, and
	// the per-table pairing this driver depends on no longer holds.
	if len(ours.Tables) != len(base.Tables) || len(theirs.Tables) != len(base.Tables) {
		in.fallback("the sides disagree on how many tables the %s has (base %d, ours %d, theirs %d)",
			d.noun, len(base.Tables), len(ours.Tables), len(theirs.Tables))
	}

	work, err := os.MkdirTemp("", in.name+"-merge")
	if err != nil {
		in.fallback("no temporary directory")
	}
	defer func() { _ = os.RemoveAll(work) }()

	var result []string
	for i := range base.Tables {
		prose, clean := in.mergeProse(work, i, base.Tables[i].Pre, ours.Tables[i].Pre, theirs.Tables[i].Pre)
		if !clean {
			in.fallback("the prose before table %d conflicts", i+1)
		}
		rows, err := keyedrecords.Merge(
			base.Tables[i].Rows, ours.Tables[i].Rows, theirs.Tables[i].Rows, mdregistry.LinkKey)
		if err != nil {
			in.fallback("table %d: %s", i+1, err)
		}
		result = append(result, prose...)
		result = append(result, rows...)
	}

	post, clean := in.mergeProse(work, len(base.Tables), base.Post, ours.Post, theirs.Post)
	if !clean {
		in.fallback("the prose after the last table conflicts")
	}
	result = append(result, post...)

	// Each table merged on its own, so nothing above can see a subject that
	// ended up in two of them — the shape one branch regrouping an entry
	// produces while another edits its old row. One subject, one row, whole
	// page.
	if dupes := d.duplicates(result); len(dupes) > 0 {
		in.fallback("the merged %s lists a %s more than once: %s",
			d.noun, d.subject, strings.Join(dupes, " "))
	}

	if err := writeLines(in.ours, result); err != nil {
		in.fallback("the merged page could not be written")
	}
	in.note("resolved %s by %s path; review the row set before committing", in.targetPath, d.subject)
}

func (d registryDriver) duplicates(lines []string) []string {
	seen := make(map[string]bool)
	var dupes []string
	for _, line := range lines {
		target := mdregistry.LinkKey(line)
		if target == "" {
			continue
		}
		key, keyed := d.dupeKey(target)
		if !keyed {
			continue
		}
		if seen[key] {
			dupes = append(dupes, key)
		}
		seen[key] = true
	}
	return dupes
}

// mergeProse three-way merges one prose segment with git's own merge, so the
// text between the tables resolves exactly as it would have without a driver.
func (in *invocation) mergeProse(work string, n int, base, ours, theirs []string) ([]string, bool) {
	paths := make([]string, 3)
	for i, side := range [][]string{base, ours, theirs} {
		p := filepath.Join(work, []string{"base", "ours", "theirs"}[i]+".pre."+strconv.Itoa(n))
		if err := writeLines(p, side); err != nil {
			in.fallback("a prose segment could not be staged")
		}
		paths[i] = p
	}
	out, clean, err := in.mergeFile(paths[0], paths[1], paths[2], false)
	if err != nil {
		in.fallback("git merge-file could not be run")
	}
	return splitLines(string(out)), clean
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{""}
	}
	return strings.Split(s, "\n")
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out, sc.Err()
}

func writeLines(path string, lines []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, line := range lines {
		if _, err := w.WriteString(line + "\n"); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
