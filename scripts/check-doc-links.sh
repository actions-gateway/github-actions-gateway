#!/usr/bin/env bash
#
# check-doc-links.sh — GitHub-slug-aware markdown link & anchor checker (Q52).
#
# Walks every tracked, non-vendored Markdown file and fails on:
#   1. Dead relative file links — a `[text](path)` (or reference definition,
#      or `<path>`-wrapped target) whose resolved path is neither a tracked
#      file nor a tracked directory.
#   2. Dead anchors — a `#fragment` (same-page `[x](#frag)` or cross-doc
#      `[x](other.md#frag)`) that matches no heading slug or explicit
#      `<a id="...">`/`<a name="...">` anchor in the target file.
#
# Anchors are validated with GitHub's heading-slug algorithm (github-slugger):
# strip inline markdown, lowercase, drop every character that is not
# [a-z0-9 _-], turn spaces into hyphens, and de-duplicate repeats with a
# `-1`, `-2`, … suffix. Leading/trailing/run hyphens are NOT collapsed —
# GitHub does not collapse them, so neither do we.
#
# Out of scope (deliberately): external URLs (http/https/mailto/tel), links
# inside fenced or inline code, anchors in non-Markdown or vendored targets.
# A trailing `:NN` / `:NN-MM` line reference on a file link (e.g.
# `provisioner.go:42`) is tolerated — only the file part is resolved.
#
# Usage:
#   scripts/check-doc-links.sh
#
# Exits non-zero on the first run that finds any broken link/anchor, printing
# `file:line: message` for each (GitHub `::error::` annotations under CI).

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

# Files to scan: tracked Markdown, excluding the vendored third-party trees.
mapfile -t md_files < <(git ls-files -- '*.md' ':!:**/vendor/**' ':!:vendor/**' | LC_ALL=C sort)

# Skip symlinks (e.g. AGENTS.md -> CLAUDE.md) so the target is scanned once.
scan_files=()
for f in "${md_files[@]}"; do
    [[ -L "$f" ]] && continue
    scan_files+=("$f")
done

if (( ${#scan_files[@]} == 0 )); then
    echo "check-doc-links: no markdown files to check" >&2
    exit 0
fi

# Existence oracle for relative-link resolution: every tracked path, plus
# present-but-untracked (non-ignored) files so a link to a brand-new file added
# in the same change resolves before it is staged. The awk program derives
# ancestor directories from these so directory links resolve too.
exist_file="$(mktemp "${TMPDIR:-/tmp}/check-doc-links.XXXXXX")"
trap 'rm -f "$exist_file"' EXIT
{ git ls-files; git ls-files --others --exclude-standard; } > "$exist_file"

awk -v GHA="${GITHUB_ACTIONS:-}" -v EXIST_FILE="$exist_file" '
    # ---- helpers ----------------------------------------------------------
    function add_dirs(path,   parts, n, i, acc) {
        n = split(path, parts, "/")
        acc = ""
        for (i = 1; i < n; i++) {
            acc = (acc == "" ? parts[i] : acc "/" parts[i])
            exists[acc] = 1
        }
    }
    # Strip [text](url) / ![alt](url) to their visible text for slugging.
    function strip_links(s,   out, rest, start, len, m, txt, pre) {
        out = ""
        rest = s
        while (match(rest, /\[[^]]*\]\([^)]*\)/)) {
            start = RSTART; len = RLENGTH
            pre = substr(rest, 1, start - 1)
            sub(/!$/, "", pre)                 # drop the image "!" marker
            m = substr(rest, start, len)
            txt = m
            sub(/^\[/, "", txt)
            sub(/\]\([^)]*\)$/, "", txt)
            out = out pre txt
            rest = substr(rest, start + len)
        }
        return out rest
    }
    # GitHub heading-slug algorithm. Inline code spans render literally, so
    # markdown inside backticks (e.g. a `[QN](#QN)` link) is NOT processed —
    # split on backticks and only strip link markup in the non-code segments.
    function slugify(s,   r, out, i, n, parts) {
        n = split(s, parts, "`")
        out = ""
        for (i = 1; i <= n; i++)
            out = out ((i % 2 == 0) ? parts[i] : strip_links(parts[i]))
        r = tolower(out)
        gsub(/[^a-z0-9 _-]/, "", r)            # drop punctuation, emoji, etc.
        gsub(/ /, "-", r)                      # spaces -> hyphens (no collapse)
        return r
    }
    # Normalise a path: resolve "." / ".." / empty segments.
    function normalize(p,   parts, n, i, k, st, out) {
        n = split(p, parts, "/")
        k = 0
        for (i = 1; i <= n; i++) {
            if (parts[i] == "" || parts[i] == ".") continue
            if (parts[i] == "..") { if (k > 0) k--; continue }
            st[++k] = parts[i]
        }
        out = ""
        for (i = 1; i <= k; i++) out = (out == "" ? st[i] : out "/" st[i])
        return out
    }
    # Resolve a link path relative to the source file (or repo root for /...).
    function resolve(srcfile, path,   dir) {
        if (path ~ "^/") return normalize(substr(path, 2))
        dir = srcfile
        sub("/[^/]*$", "", dir)
        if (dir == srcfile) dir = ""           # srcfile had no directory part
        return normalize(dir == "" ? path : dir "/" path)
    }
    function report(file, lineno, msg) {
        fail = 1; errcount++
        if (GHA != "") printf "::error file=%s,line=%d::%s\n", file, lineno, msg
        else           printf "%s:%d: %s\n", file, lineno, msg
    }
    # Record one link target for END-phase validation (anchors of every file
    # must be known before any cross-file anchor can be checked).
    function collect(srcfile, lineno, target) {
        nlinks++
        L_src[nlinks] = srcfile; L_line[nlinks] = lineno; L_raw[nlinks] = target
    }
    function clean_target(t) {
        if (t ~ /^</) { sub(/^</, "", t); sub(/>.*$/, "", t); return t }
        sub(/[ \t].*$/, "", t)                 # drop a "(url \"title\")" title
        return t
    }
    function validate(srcfile, lineno, t,   anchor, path, resolved, bare) {
        if (t == "") return
        if (t ~ "^[a-zA-Z][a-zA-Z0-9+.-]*://") return   # scheme://...
        if (t ~ /^(mailto|tel):/) return
        if (t ~ /^#/) { check_anchor(srcfile, srcfile, substr(t, 2), lineno, t); return }
        anchor = ""
        path = t
        if (index(t, "#") > 0) {
            anchor = t; sub(/^[^#]*#/, "", anchor)
            sub(/#.*$/, "", path)
        }
        sub(/\?.*$/, "", path)
        sub(/:[0-9]+(-[0-9]+)?$/, "", path)    # tolerate a "file.go:42" line ref
        if (path == "") { check_anchor(srcfile, srcfile, anchor, lineno, t); return }
        resolved = resolve(srcfile, path)
        bare = resolved; sub("/$", "", bare)
        if (!(bare in exists) && !(resolved in exists)) {
            report(srcfile, lineno, "dead link: " t " -> " (resolved == "" ? "(outside repo)" : resolved))
            return
        }
        if (anchor != "" && bare ~ /\.md$/ && (bare in scanned))
            check_anchor(srcfile, bare, anchor, lineno, t)
    }
    function check_anchor(srcfile, targetfile, anchor, lineno, raw) {
        if (anchor == "") return
        if ((targetfile, anchor) in anchors) return
        report(srcfile, lineno, "dead anchor: " raw " -> #" anchor " has no matching heading or <a id> in " targetfile)
    }

    # ---- setup ------------------------------------------------------------
    BEGIN {
        while ((getline line < EXIST_FILE) > 0) { exists[line] = 1; add_dirs(line) }
        close(EXIST_FILE)
    }

    # ---- per-file reset ---------------------------------------------------
    FNR == 1 { scanned[FILENAME] = 1; nfiles++; infence = 0; fencechar = "" }

    # ---- fenced code blocks ----------------------------------------------
    {
        if ($0 ~ /^[ \t]*```+/ || $0 ~ /^[ \t]*~~~+/) {
            c = ($0 ~ /~~~/) ? "~" : "`"
            if (!infence)            { infence = 1; fencechar = c }
            else if (c == fencechar) { infence = 0; fencechar = "" }
            next
        }
        if (infence) next
    }

    # ---- explicit HTML anchors (anywhere on the line) --------------------
    {
        s = $0
        while (match(s, /<a[ \t]+(id|name)[ \t]*=[ \t]*"[^"]*"/)) {
            m = substr(s, RSTART, RLENGTH)
            id = m; sub(/^.*=[ \t]*"/, "", id); sub(/".*$/, "", id)
            anchors[FILENAME, id] = 1
            s = substr(s, RSTART + RLENGTH)
        }
    }

    # ---- ATX headings -> slug anchors (github-slugger dedup) --------------
    /^#{1,6}[ \t]/ {
        h = $0
        sub(/^#{1,6}[ \t]+/, "", h)
        sub(/[ \t]+#+[ \t]*$/, "", h)          # optional closing hashes
        sub(/^[ \t]+/, "", h); sub(/[ \t]+$/, "", h)
        base = slugify(h); res = base
        while ((FILENAME, res) in occ) { occ[FILENAME, base]++; res = base "-" occ[FILENAME, base] }
        occ[FILENAME, res] = 0
        anchors[FILENAME, res] = 1
    }

    # ---- collect links for END-phase validation --------------------------
    {
        # Reference definition: `[label]: target` (skip footnote `[^id]:`).
        if ($0 ~ /^[ \t]*\[[^]^][^]]*\]:[ \t]*[^ \t]/) {
            rd = $0
            sub(/^[ \t]*\[[^]]*\]:[ \t]*/, "", rd)
            collect(FILENAME, FNR, clean_target(rd))
        }
        # Inline links, with inline code spans blanked first.
        tmp = $0
        gsub(/`[^`]*`/, "  ", tmp)
        while (match(tmp, /\[[^]]*\]\([^)]*\)/)) {
            m = substr(tmp, RSTART, RLENGTH)
            tg = m; sub(/^\[[^]]*\]\(/, "", tg); sub(/\)$/, "", tg)
            collect(FILENAME, FNR, clean_target(tg))
            tmp = substr(tmp, RSTART + RLENGTH)
        }
    }

    # ---- validate everything ---------------------------------------------
    END {
        for (i = 1; i <= nlinks; i++) validate(L_src[i], L_line[i], L_raw[i])
        if (fail) {
            printf "check-doc-links: FAILED — %d broken link/anchor%s\n", errcount, (errcount == 1 ? "" : "s")
            exit 1
        }
        printf "check-doc-links: ok (%d markdown files, %d links/anchors checked)\n", nfiles, nlinks
    }
' "${scan_files[@]}"
