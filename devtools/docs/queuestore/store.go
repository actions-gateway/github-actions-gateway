package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// frontmatter is the YAML head of an item file. Title and Notes live in the
// body instead, so the file reads as the page it is rather than as a record
// with prose hidden in a quoted scalar.
type frontmatter struct {
	ID     string   `yaml:"id"`
	Rank   string   `yaml:"rank"`
	Labels []string `yaml:"labels,omitempty"`
	Status string   `yaml:"status"`
	Size   string   `yaml:"size"`
	Target string   `yaml:"target,omitempty"`
}

const fence = "---"

// Marshal renders an item as its file: YAML frontmatter, an H1 carrying the
// title, then the notes.
func (it Item) Marshal() ([]byte, error) {
	// Written in item-relative form: the file is a published page, so its
	// links have to resolve from where it sits rather than from the table.
	on := it.toItemForm()
	head, err := yaml.Marshal(frontmatter{
		ID:     on.ID,
		Rank:   on.Rank,
		Labels: on.Labels,
		Status: on.Status,
		Size:   on.Size,
		Target: on.Target,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", it.ID, err)
	}
	var b strings.Builder
	b.WriteString(fence + "\n")
	b.Write(head)
	b.WriteString(fence + "\n\n")
	b.WriteString("# " + on.Title + "\n")
	if on.Notes != "" {
		b.WriteString("\n" + on.Notes + "\n")
	}
	return []byte(b.String()), nil
}

// UnmarshalItem reads an item file back.
func UnmarshalItem(src []byte) (Item, error) {
	text := string(src)
	if !strings.HasPrefix(text, fence+"\n") {
		return Item{}, fmt.Errorf("file does not open with a %q frontmatter fence", fence)
	}
	rest := text[len(fence)+1:]
	end := strings.Index(rest, "\n"+fence+"\n")
	if end < 0 {
		return Item{}, fmt.Errorf("frontmatter is not closed by a %q fence", fence)
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(rest[:end+1]), &fm); err != nil {
		return Item{}, fmt.Errorf("frontmatter: %w", err)
	}

	body := strings.TrimLeft(rest[end+len(fence)+2:], "\n")
	title, notes := body, ""
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		title, notes = body[:i], strings.TrimSpace(body[i+1:])
	}
	if !strings.HasPrefix(title, "# ") {
		return Item{}, fmt.Errorf("%s: body does not open with an H1 title", fm.ID)
	}

	// Back to the table-relative form every other part of this program holds.
	it := Item{
		ID:     fm.ID,
		Rank:   fm.Rank,
		Labels: fm.Labels,
		Status: fm.Status,
		Size:   fm.Size,
		Target: fm.Target,
		Title:  strings.TrimPrefix(title, "# "),
		Notes:  notes,
	}.toTableForm()
	return it, it.Validate()
}

// WriteStore writes one file per item into dir, named for the item's ID.
//
// A file per item is the whole point: two items added, edited or deleted on
// different branches touch different paths, which no merge algorithm can turn
// into a conflict.
func WriteStore(dir string, items []Item) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, it := range items {
		if err := it.Validate(); err != nil {
			return err
		}
		body, err := it.Marshal()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, it.ID+".md"), body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// ReadStore reads every item file in dir. The returned order is by filename,
// which is not the priority order — call SortItems for that.
func ReadStore(dir string) ([]Item, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var items []Item
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "README.md" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		it, err := UnmarshalItem(src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if want := strings.TrimSuffix(e.Name(), ".md"); it.ID != want {
			return nil, fmt.Errorf("%s: file holds id %q", e.Name(), it.ID)
		}
		if seen[it.ID] {
			return nil, fmt.Errorf("%s: id is filed twice", it.ID)
		}
		seen[it.ID] = true
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}
