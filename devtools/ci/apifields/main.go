// Command apifields fails a build when a served API field has no consumer: a
// spec field nothing reads, or a status field nothing writes.
//
// A field in a served CRD schema is a promise. The API server accepts and
// stores it whether or not any controller acts on it, so a field that ships
// unread is accepted silently and then ignored: `sharing.allowedNamespaces`
// shipped served-but-unenforced in v2beta1 and only a manual docs sweep caught
// it (Q166, Q573). The status side fails the same way in the other direction:
// v1alpha1's `status.activeSessions` was served, described, and never set
// (Q526).
//
// Each argument names one API group as a comma-separated list of the packages
// serving its versions. Versions convert through a JSON round-trip, so they are
// judged together: a field is consumed when the field at the same path in any
// version of its group is. That is what lets the controllers reconcile v2alpha1
// objects stored at another version.
//
// Within a version the unit is the Go struct field, not the Kind and JSON path:
// a type shared by two kinds is recorded once, under the first path the walk
// meets, so a reader of either kind's copy credits both.
//
// Within a package, every struct field reachable from a root kind's top-level
// fields is classified by the side it is on:
//
//	status  reached through the root's `status` field; needs a write
//	spec    reached through any other top-level field; needs a read
//
// A field reached from both sides needs a read. Then every non-test Go file in
// the workspace modules is type-checked, and each use of each field is
// classified by its syntactic position:
//
//	write   the selector (or any selector it is the base of) is an assignment
//	        or inc/dec target, or the field is a composite-literal key
//	read    anything else, which includes being passed to a function
//	both    its address is taken, or it is the receiver of a pointer-receiver
//	        method call
//
// Uses in generated and conversion code do not count: `zz_generated.*` copies
// every field and `conversion.go` moves every field between versions, so both
// would satisfy the gate for a field no controller ever looks at. Test files
// are not loaded, for the same reason.
//
// A field with no qualifying use is a finding unless the baseline file lists
// it, in the form the finding prints; a baseline entry whose field now has a
// use, or no longer exists, is a finding too, so the list cannot rot.
//
// The check is a lower bound on neglect, not a proof of enforcement. Each of
// these satisfies it without the field doing anything:
//
//   - a webhook that only validates a field's format
//   - a reader of another kind that shares the field's Go type (above)
//   - a function in the API package itself, whether or not anything calls it
//   - a status field whose address is passed to a function that only reads it,
//     including a read-only method with a pointer receiver, such as
//     metav1.Time.IsZero or resource.Quantity.Cmp
//
// Usage:
//
//	apifields [-baseline FILE] [-min N] <pkg>[,<pkg>...]...
//
// Run from the repository root; the workspace modules come from `go list -m`.
// Prints one finding per line, then the number of fields checked. Exits 1 on a
// finding, 2 when loading fails or fewer than -min fields were checked — a walk
// that silently stops matching looks identical to a clean tree.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/objectpath"
)

type side int

const (
	sideSpec side = iota
	sideStatus
)

// fields maps a struct field's key to its record. Each package sees its
// imports through export data, so the same field is a different *types.Var in
// every importer; the key is what they agree on.
type fields map[string]*field

// key names v by package path and object path, or returns "" for a variable
// with no stable path (a local, or a field of an unnamed struct).
func key(v *types.Var) string {
	if v.Pkg() == nil {
		return ""
	}
	p, err := objectpath.For(v)
	if err != nil {
		return ""
	}
	return v.Pkg().Path() + ":" + string(p)
}

// lookup returns v's record, or nil when v is not an API field.
func (fs fields) lookup(v *types.Var) *field {
	if v == nil {
		return nil
	}
	if k := key(v); k != "" {
		return fs[k]
	}
	return nil
}

// field is one struct field of an API type.
type field struct {
	group   int    // index of the command-line argument naming its package
	version string // package name, e.g. v2beta1
	path    string // Kind plus JSON path, e.g. ActionsGateway.spec.sharing.allowedNamespaces
	side    side
	read    bool
	write   bool
}

func main() {
	baseline := flag.String("baseline", "", "file listing 'version(s) Kind.path' entries known to have no consumer, one per line, # comments allowed")
	min := flag.Int("min", 1, "fail when fewer than this many fields were checked")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "usage: %s [-baseline FILE] [-min N] <api-package-path>[,<api-package-path>...]...\n", os.Args[0])
		os.Exit(2)
	}
	findings, checked, err := run(".", *baseline, flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "apifields: %v\n", err)
		os.Exit(2)
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	fmt.Printf("apifields: %d field(s) checked, %d finding(s)\n", checked, len(findings))
	switch {
	case checked < *min:
		fmt.Fprintf(os.Stderr, "apifields: checked %d field(s), fewer than -min %d\n", checked, *min)
		os.Exit(2)
	case len(findings) > 0:
		os.Exit(1)
	}
}

// run loads every workspace module under dir and returns the findings and the
// number of fields checked.
func run(dir, baseline string, groups []string) ([]string, int, error) {
	known := map[string]bool{}
	if baseline != "" {
		var err error
		if known, err = readBaseline(baseline); err != nil {
			return nil, 0, err
		}
	}
	mods, err := workspaceModules(dir)
	if err != nil {
		return nil, 0, err
	}
	patterns := make([]string, 0, len(mods))
	for _, m := range mods {
		patterns = append(patterns, m+"/...")
	}
	pkgs, err := load(dir, patterns)
	if err != nil {
		return nil, 0, err
	}
	fs, err := collect(pkgs, groups)
	if err != nil {
		return nil, 0, err
	}
	if err := unnamedAPIs(pkgs, groups); err != nil {
		return nil, 0, err
	}
	for _, p := range pkgs {
		classifyUses(p, fs)
	}
	return report(fs, known), len(fs), nil
}

func workspaceModules(dir string) ([]string, error) {
	cmd := exec.Command("go", "list", "-m")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -m: %w", err)
	}
	return strings.Fields(string(out)), nil
}

func load(dir string, patterns []string) ([]*packages.Package, error) {
	cfg := &packages.Config{
		Dir: dir,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}
	var errs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			errs = append(errs, e.Error())
		}
	})
	if len(errs) > 0 {
		return nil, fmt.Errorf("loading packages:\n  %s", strings.Join(errs, "\n  "))
	}
	return pkgs, nil
}

// collect walks every root kind in the named packages — a struct embedding
// ObjectMeta — and returns its fields. Each group is a comma-separated list of
// the package paths serving one API group. A named type reached by two paths
// is recorded once, under the path the walk meets first: kinds in scope order,
// fields in declaration order.
func collect(pkgs []*packages.Package, groups []string) (fields, error) {
	byPath := map[string]*packages.Package{}
	for _, p := range pkgs {
		byPath[p.PkgPath] = p
	}
	fs := fields{}
	for gi, group := range groups {
		for _, ap := range strings.Split(group, ",") {
			p, ok := byPath[ap]
			if !ok {
				return nil, fmt.Errorf("API package %s is not in any workspace module", ap)
			}
			scope := p.Types.Scope()
			roots := 0
			for _, name := range scope.Names() {
				tn, ok := scope.Lookup(name).(*types.TypeName)
				if !ok {
					continue
				}
				st, ok := tn.Type().Underlying().(*types.Struct)
				if !ok || !isRoot(st) {
					continue
				}
				roots++
				at := field{group: gi, version: p.Name}
				for i := 0; i < st.NumFields(); i++ {
					f := st.Field(i)
					tag := jsonName(st.Tag(i), f.Name())
					if f.Embedded() || tag == "" {
						continue
					}
					s := sideSpec
					if tag == "status" {
						s = sideStatus
					}
					add(fs, f, at, name+"."+tag, s, map[*types.Named]bool{})
				}
			}
			if roots == 0 {
				return nil, fmt.Errorf("API package %s declares no root kind", ap)
			}
		}
	}
	return fs, nil
}

// unnamedAPIs fails when a package declares a root kind that no group names:
// its fields would otherwise go unchecked, and a new API version is exactly when
// a field ships unconsumed.
func unnamedAPIs(pkgs []*packages.Package, groups []string) error {
	named := map[string]bool{}
	for _, g := range groups {
		for _, p := range strings.Split(g, ",") {
			named[p] = true
		}
	}
	var missing []string
	for _, p := range pkgs {
		if named[p.PkgPath] || !inRepo(p.PkgPath) {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			if st, ok := tn.Type().Underlying().(*types.Struct); ok && isRoot(st) {
				missing = append(missing, p.PkgPath)
				break
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("packages declaring a root kind that no argument names: %s", strings.Join(missing, ", "))
	}
	return nil
}

func isRoot(st *types.Struct) bool {
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Embedded() && f.Name() == "ObjectMeta" {
			return true
		}
	}
	return false
}

// add records f, then descends into its type when that is a struct declared in
// this repository. Types from other modules (corev1, metav1) are opaque: a
// field typed corev1.ResourceRequirements is consumed when the controller reads
// it, whatever it does with the parts.
func add(fs fields, f *types.Var, at field, path string, s side, seen map[*types.Named]bool) {
	k := key(f)
	if k == "" {
		return
	}
	if prev, ok := fs[k]; ok {
		if s == sideSpec {
			prev.side = sideSpec
		}
		return
	}
	at.path, at.side = path, s
	fs[k] = &at

	n := named(f.Type())
	if n == nil || seen[n] || n.Obj().Pkg() == nil || !inRepo(n.Obj().Pkg().Path()) {
		return
	}
	st, ok := n.Underlying().(*types.Struct)
	if !ok {
		return
	}
	seen[n] = true
	defer delete(seen, n)
	for i := 0; i < st.NumFields(); i++ {
		c := st.Field(i)
		tag := jsonName(st.Tag(i), c.Name())
		if tag == "" {
			continue
		}
		cp := path + "." + tag
		if c.Embedded() {
			// An inline embed contributes its fields at this level.
			cp = path
		}
		add(fs, c, at, cp, s, seen)
	}
}

// named unwraps pointers, slices, arrays and map values to the named type
// underneath, or returns nil.
func named(t types.Type) *types.Named {
	for {
		switch u := t.(type) {
		case *types.Named:
			return u
		case *types.Alias:
			t = types.Unalias(u)
		case *types.Pointer:
			t = u.Elem()
		case *types.Slice:
			t = u.Elem()
		case *types.Array:
			t = u.Elem()
		case *types.Map:
			t = u.Elem()
		default:
			return nil
		}
	}
}

const repoPrefix = "github.com/actions-gateway/github-actions-gateway/"

func inRepo(pkgPath string) bool { return strings.HasPrefix(pkgPath, repoPrefix) }

// jsonName returns the field's JSON name, or "" when it is not serialized.
func jsonName(tag, goName string) string {
	v, ok := reflect.StructTag(tag).Lookup("json")
	if !ok {
		return goName
	}
	switch name, _, _ := strings.Cut(v, ","); name {
	case "-":
		return ""
	case "":
		return goName
	default:
		return name
	}
}

// excluded reports whether uses in this file do not count.
func excluded(filename string) bool {
	base := filepath.Base(filename)
	return strings.HasPrefix(base, "zz_generated") || base == "conversion.go" ||
		strings.HasSuffix(base, "_test.go")
}

// classifyUses marks every field use in p's non-excluded files.
func classifyUses(p *packages.Package, fs fields) {
	for _, file := range p.Syntax {
		if excluded(p.Fset.File(file.Pos()).Name()) {
			continue
		}
		writes := map[*ast.SelectorExpr]bool{}
		both := map[*ast.SelectorExpr]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if n.Tok == token.DEFINE {
					return true
				}
				for _, lhs := range n.Lhs {
					markChain(lhs, writes)
				}
			case *ast.IncDecStmt:
				markChain(n.X, writes)
			case *ast.UnaryExpr:
				if n.Op == token.AND {
					markChain(n.X, both)
				}
			case *ast.CallExpr:
				// Only a pointer receiver can write the value it is called on;
				// a value receiver gets a copy.
				if sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr); ok && pointerRecv(p.TypesInfo.Selections[sel]) {
					markChain(sel.X, both)
				}
			case *ast.CompositeLit:
				for _, e := range n.Elts {
					kv, ok := e.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					id, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					if v, ok := p.TypesInfo.Uses[id].(*types.Var); ok {
						if f := fs.lookup(v); f != nil {
							f.write = true
						}
					}
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			s := p.TypesInfo.Selections[sel]
			if s == nil || s.Kind() != types.FieldVal {
				return true
			}
			// A promoted field reaches its target through embedded fields;
			// credit only the field the selector names.
			v, _ := s.Obj().(*types.Var)
			f := fs.lookup(v)
			if f == nil {
				return true
			}
			switch {
			case both[sel]:
				f.read, f.write = true, true
			case writes[sel]:
				f.write = true
			default:
				f.read = true
			}
			return true
		})
	}
}

// pointerRecv reports whether s selects a method with a pointer receiver.
func pointerRecv(s *types.Selection) bool {
	if s == nil || s.Kind() != types.MethodVal {
		return false
	}
	sig, ok := s.Obj().Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	_, ok = sig.Recv().Type().(*types.Pointer)
	return ok
}

// markChain marks e and every selector it is built from: in `a.B.C = x` the
// assignment writes C and only walks through B, so B is not read either. Index
// expressions and derefs are looked through for the same reason.
func markChain(e ast.Expr, into map[*ast.SelectorExpr]bool) {
	for {
		switch x := ast.Unparen(e).(type) {
		case *ast.SelectorExpr:
			into[x] = true
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		default:
			return
		}
	}
}

func readBaseline(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line, _, _ = strings.Cut(line, "#")
		if line = strings.TrimSpace(line); line != "" {
			known[line] = true
		}
	}
	return known, nil
}

// report aggregates fields by group and path: the versions in a group convert
// through a JSON round-trip, so a field is consumed when the field at the same
// path in any version of its group is.
func report(fs fields, known map[string]bool) []string {
	type agg struct {
		side     side
		used     bool
		versions map[string]bool
	}
	byPath := map[string]*agg{}
	for _, f := range fs {
		gp := fmt.Sprintf("%d:%s", f.group, f.path)
		a := byPath[gp]
		if a == nil {
			a = &agg{side: sideStatus, versions: map[string]bool{}}
			byPath[gp] = a
		}
		if f.side == sideSpec {
			a.side = sideSpec
		}
		a.versions[f.version] = true
		if (f.side == sideSpec && f.read) || (f.side == sideStatus && f.write) {
			a.used = true
		}
	}
	var out []string
	present := map[string]bool{}
	for gp, a := range byPath {
		_, path, _ := strings.Cut(gp, ":")
		vs := make([]string, 0, len(a.versions))
		for v := range a.versions {
			vs = append(vs, v)
		}
		sort.Strings(vs)
		name := strings.Join(vs, ",") + " " + path
		present[name] = true
		what := "spec field nothing reads"
		if a.side == sideStatus {
			what = "status field nothing writes"
		}
		switch {
		case !a.used && !known[name]:
			out = append(out, fmt.Sprintf("%s: %s", name, what))
		case a.used && known[name]:
			out = append(out, fmt.Sprintf("%s: baselined but now has a consumer; remove it from the baseline", name))
		}
	}
	for p := range known {
		if !present[p] {
			out = append(out, fmt.Sprintf("%s: baselined but no such field; remove it from the baseline", p))
		}
	}
	sort.Strings(out)
	return out
}
