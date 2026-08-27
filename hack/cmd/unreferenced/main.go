// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// unreferenced reports package-level declarations that nothing in the module
// refers to.
//
// # Why this exists alongside deadcode
//
// `make deadcode` runs Rapid Type Analysis from the main packages, which is
// sound with respect to reflection: it will not call a function dead when
// reflection could reach it. The price of that soundness is
// x/tools/go/callgraph/rta/rta.go, addRuntimeType: converting a value of type
// T to an interface materializes T's runtime type, and RTA then marks *every
// exported method of T* reachable, because reflection could call any of them.
//
// One `x = T{}; var i any = x` anywhere in the program therefore makes all of
// T's exported methods live for the rest of the analysis. Asking deadcode why
// gives:
//
//	$ go tool deadcode -whylive=...netlink.LinkManager.SetLinkNoARP ./...
//	deadcode: ...LinkManager.SetLinkNoARP is reachable only through reflection
//
// That is structurally the same blind spot as `unused` (which holds that a
// named type uses its exported methods), reached by a different route. It is
// the blind spot that hid seven dead LinkManager methods, and no setting on
// either tool changes it.
//
// This tool asks a narrower and much cruder question that the reflection rule
// cannot swallow: does any identifier anywhere in the module, in any package,
// in any test, under any build tag, refer to this declaration? If the answer
// is no, the declaration is dead however many interfaces its type is boxed
// into.
//
// # What it deliberately does not do
//
// It is a reference check, not a reachability analysis. A cluster of dead
// functions that only call each other all look referenced here. deadcode
// catches those. Neither tool subsumes the other; run both.
//
// Self-reference does not count, so a directly recursive dead function is
// still reported. Mutual recursion is not unpicked.
//
// Struct fields are out of scope. Field reads and writes go through struct
// tags, encoding/json and reflection often enough that a reference count says
// very little about them.
//
// # The one filter that matters
//
// A method can have no direct reference and still be load-bearing, because it
// is what makes its receiver satisfy an interface that is called dynamically.
// `wqProvider.NewDepthMetric` is never named anywhere in this tree; deleting
// it would break client-go's workqueue.MetricsProvider. Reporting those would
// make the output actively dangerous, so any method whose receiver implements
// an interface declaring a method of that name is dropped, and counted in the
// summary so the suppression is visible rather than silent.
//
// Interfaces are collected from every loaded package and every transitive
// dependency, so this covers slog.Handler, prometheus.Collector,
// yaml.Unmarshaler and the rest of the standard shapes.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

const loadMode = packages.NeedName |
	packages.NeedFiles |
	packages.NeedCompiledGoFiles |
	packages.NeedImports |
	packages.NeedDeps |
	packages.NeedTypes |
	packages.NeedSyntax |
	packages.NeedTypesInfo

// generatedHeader is the convention from https://go.dev/s/generatedcode.
var generatedHeader = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// declaration is one package-level declaration that could be reported.
type declaration struct {
	Kind     string `json:"kind"`    // func, method, type, const, var
	Name     string `json:"name"`    // Foo, or T.Foo for a method
	Package  string `json:"package"` // import path, with any test variant suffix stripped
	File     string `json:"file"`    // relative to the module root
	Line     int    `json:"line"`
	Exported bool   `json:"exported"`

	// Interface is the interface that kept this declaration out of the
	// report, when one did. Only set on suppressed entries.
	Interface string `json:"interface,omitempty"`

	// recv is the named receiver type of a method, for the interface filter.
	recv *types.Named `json:"-"`
}

// key identifies a declaration across build configurations and across the
// package variants `packages.Load` produces for tests.
//
// Loading with Tests:true type-checks a package's non-test files more than
// once - once for the package proper and once for the variant that includes
// its in-package tests - and each pass mints fresh types.Object values for the
// same source declaration. Object identity is therefore useless here. The
// declaring position is not: it is the same file, line and column in every
// variant and every tag configuration.
type key struct {
	file string
	line int
	col  int
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "unreferenced: %v\n", err)
		os.Exit(1)
	}
}

type tagSets []string

func (t *tagSets) String() string { return strings.Join(*t, " ") }

func (t *tagSets) Set(v string) error {
	*t = append(*t, v)

	return nil
}

func run(args []string, stdout io.Writer) error {
	var (
		configs  tagSets
		asJSON   bool
		showKept bool
		dir      string
		patterns = []string{"./..."}
	)

	flags := flag.NewFlagSet("unreferenced", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Var(&configs, "tags", "comma-separated build `tags` for one configuration; repeat the flag to scan several (default: one run with no tags)")
	flags.BoolVar(&asJSON, "json", false, "emit JSON instead of a text report")
	flags.BoolVar(&showKept, "show-kept", false, "list the methods an interface kept out of the report, not just their count")
	flags.StringVar(&dir, "C", "", "run as if in this `directory`")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() > 0 {
		patterns = flags.Args()
	}

	if len(configs) == 0 {
		configs = tagSets{""}
	}

	root, err := moduleRoot(dir)
	if err != nil {
		return err
	}

	scan := newScan(root)

	// Every configuration contributes both declarations and references. A
	// declaration that exists under one tag set and is referenced under
	// another is referenced, so the sets are unioned rather than intersected.
	for _, tags := range configs {
		if err := scan.load(dir, patterns, tags); err != nil {
			return err
		}
	}

	unreferenced, suppressed := scan.results()

	if asJSON {
		return writeJSON(stdout, unreferenced, suppressed)
	}

	return writeText(stdout, unreferenced, suppressed, configs, showKept)
}

// scan accumulates declarations and references across configurations.
type scan struct {
	root string

	declared map[key]*declaration
	used     map[key]bool

	// interfaces is every interface with at least one method seen anywhere in
	// the loaded programs, including dependencies.
	interfaces []*types.Interface
}

func newScan(root string) *scan {
	return &scan{
		root:     root,
		declared: map[key]*declaration{},
		used:     map[key]bool{},
	}
}

func (s *scan) load(dir string, patterns []string, tags string) error {
	cfg := &packages.Config{
		Mode:  loadMode,
		Dir:   dir,
		Tests: true,
	}

	if tags != "" {
		cfg.BuildFlags = []string{"-tags=" + tags}
	}

	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return fmt.Errorf("load packages (tags=%q): %w", tags, err)
	}

	if len(pkgs) == 0 {
		return fmt.Errorf("no packages matched %v (tags=%q)", patterns, tags)
	}

	// A type error means the type information is incomplete, and incomplete
	// type information silently under-counts references, which turns live code
	// into a deletion candidate. That has to be loud.
	var errs []error

	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, e := range pkg.Errors {
			errs = append(errs, fmt.Errorf("%s: %s", pkg.PkgPath, e))
		}
	})

	if len(errs) > 0 {
		return fmt.Errorf("packages failed to load cleanly (tags=%q): %w", tags, errors.Join(errs...))
	}

	s.collectInterfaces(pkgs)

	for _, pkg := range pkgs {
		s.collectDeclarations(pkg)
		s.collectReferences(pkg)
	}

	return nil
}

// collectInterfaces gathers every named interface in the loaded packages and
// their transitive dependencies.
//
// Dependencies are the point: the interfaces that keep methods alive in this
// tree are mostly other people's - slog.Handler, prometheus.Collector,
// workqueue.MetricsProvider.
func (s *scan) collectInterfaces(pkgs []*packages.Package) {
	seen := map[*types.Interface]bool{}

	for _, iface := range s.interfaces {
		seen[iface] = true
	}

	add := func(t types.Type) {
		iface, ok := t.Underlying().(*types.Interface)
		if !ok || iface.NumMethods() == 0 || seen[iface] {
			return
		}

		seen[iface] = true
		s.interfaces = append(s.interfaces, iface)
	}

	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		if pkg.Types == nil {
			return
		}

		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			if tn, ok := scope.Lookup(name).(*types.TypeName); ok {
				add(tn.Type())
			}
		}

		// Anonymous interfaces are common in this tree as narrow parameter
		// types, and a method can exist solely to satisfy one.
		if pkg.TypesInfo != nil {
			for _, tv := range pkg.TypesInfo.Types {
				if tv.Type != nil {
					add(tv.Type)
				}
			}
		}
	})
}

func (s *scan) collectDeclarations(pkg *packages.Package) {
	if pkg.Types == nil || pkg.TypesInfo == nil {
		return
	}

	path := trimTestVariant(pkg.PkgPath)

	// Files are indexed by the *ast.File so a declaration can be traced back
	// to the file it came from and skipped when that file is a test or is
	// generated.
	skip := map[string]bool{}

	for _, file := range pkg.Syntax {
		name := pkg.Fset.Position(file.Pos()).Filename
		skip[name] = strings.HasSuffix(name, "_test.go") || isGenerated(file)
	}

	scope := pkg.Types.Scope()

	for _, name := range scope.Names() {
		obj := scope.Lookup(name)

		pos := pkg.Fset.Position(obj.Pos())
		if skip[pos.Filename] {
			continue
		}

		switch obj := obj.(type) {
		case *types.Func:
			// main and init are called by the runtime, not by any identifier.
			if obj.Name() == "main" && pkg.Types.Name() == "main" {
				continue
			}

			s.record("func", obj.Name(), path, pos, obj.Exported(), nil)
		case *types.TypeName:
			if obj.IsAlias() {
				s.record("alias", obj.Name(), path, pos, obj.Exported(), nil)

				continue
			}

			s.record("type", obj.Name(), path, pos, obj.Exported(), nil)
			s.collectMethods(pkg, obj, path, skip)
		case *types.Const:
			s.record("const", obj.Name(), path, pos, obj.Exported(), nil)
		case *types.Var:
			s.record("var", obj.Name(), path, pos, obj.Exported(), nil)
		}
	}
}

func (s *scan) collectMethods(pkg *packages.Package, tn *types.TypeName, path string, skip map[string]bool) {
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return
	}

	// Generic types cannot be handed to types.Implements without being
	// instantiated first, and getting that wrong would suppress or report
	// methods for the wrong reason. There are none in this tree today; if that
	// changes, this is where to look.
	if named.TypeParams().Len() > 0 {
		return
	}

	for i := range named.NumMethods() {
		m := named.Method(i)

		pos := pkg.Fset.Position(m.Pos())
		if skip[pos.Filename] {
			continue
		}

		s.record("method", tn.Name()+"."+m.Name(), path, pos, m.Exported(), named)
	}
}

func (s *scan) record(kind, name, path string, pos token.Position, exported bool, recv *types.Named) {
	k := key{file: pos.Filename, line: pos.Line, col: pos.Column}
	if _, seen := s.declared[k]; seen {
		return
	}

	s.declared[k] = &declaration{
		Kind:     kind,
		Name:     name,
		Package:  path,
		File:     s.relative(pos.Filename),
		Line:     pos.Line,
		Exported: exported,
		recv:     recv,
	}
}

// collectReferences walks each file and marks every declaration an identifier
// denotes, except where the identifier is inside the declaration itself.
//
// Two exclusions matter:
//
//   - Self-reference. A directly recursive dead function calls itself, and
//     counting that would make it look alive.
//   - Method receivers. `func (w Widget) M()` is not an independent use of
//     Widget; without this, a type whose only remaining references are its own
//     methods' receivers keeps itself alive.
func (s *scan) collectReferences(pkg *packages.Package) {
	if pkg.TypesInfo == nil {
		return
	}

	for _, file := range pkg.Syntax {
		s.markLinknames(pkg, file)

		for _, decl := range file.Decls {
			var (
				self ast.Node
				skip ast.Node
			)

			if fn, ok := decl.(*ast.FuncDecl); ok {
				self = fn.Name

				if fn.Recv != nil {
					skip = fn.Recv
				}
			}

			s.markFile(pkg, decl, self, skip)
		}
	}
}

func (s *scan) markFile(pkg *packages.Package, decl, self, skip ast.Node) {
	selfKey, hasSelf := key{}, false

	if self != nil {
		if ident, ok := self.(*ast.Ident); ok {
			if obj := pkg.TypesInfo.Defs[ident]; obj != nil {
				selfKey, hasSelf = s.keyOf(pkg, obj), true
			}
		}
	}

	ast.Inspect(decl, func(n ast.Node) bool {
		if n == nil {
			return false
		}

		if skip != nil && n == skip {
			return false
		}

		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}

		obj := pkg.TypesInfo.Uses[ident]
		if obj == nil {
			return true
		}

		k := s.keyOf(pkg, obj)
		if hasSelf && k == selfKey {
			return true
		}

		s.used[k] = true

		return true
	})
}

func (s *scan) keyOf(pkg *packages.Package, obj types.Object) key {
	p := pkg.Fset.Position(obj.Pos())

	return key{file: p.Filename, line: p.Line, col: p.Column}
}

// markLinknames treats the target of a //go:linkname directive as referenced.
//
// The directive creates an aliasing this tool cannot see - the same gap
// x/tools/cmd/deadcode documents for itself - so anything named in one is
// left alone rather than reported on incomplete information.
func (s *scan) markLinknames(pkg *packages.Package, file *ast.File) {
	for _, group := range file.Comments {
		for _, c := range group.List {
			if !strings.HasPrefix(c.Text, "//go:linkname ") {
				continue
			}

			for _, name := range strings.Fields(c.Text)[1:] {
				short := name[strings.LastIndex(name, ".")+1:]

				for k, d := range s.declared {
					if d.Package == trimTestVariant(pkg.PkgPath) && lastSegment(d.Name) == short {
						s.used[k] = true
					}
				}
			}
		}
	}
}

// results splits the unreferenced declarations from the ones an interface
// keeps alive.
func (s *scan) results() (unreferenced, suppressed []declaration) {
	for k, d := range s.declared {
		if s.used[k] {
			continue
		}

		if d.Kind == "method" && d.recv != nil {
			if name, ok := s.satisfiesInterface(d); ok {
				d.Interface = name
				suppressed = append(suppressed, *d)

				continue
			}
		}

		unreferenced = append(unreferenced, *d)
	}

	sortDeclarations(unreferenced)
	sortDeclarations(suppressed)

	return unreferenced, suppressed
}

// satisfiesInterface reports whether deleting this method would break an
// interface its receiver implements.
func (s *scan) satisfiesInterface(d *declaration) (string, bool) {
	method := lastSegment(d.Name)
	pointer := types.NewPointer(d.recv)

	for _, iface := range s.interfaces {
		if !declaresMethod(iface, method) {
			continue
		}

		if types.Implements(d.recv, iface) || types.Implements(pointer, iface) {
			return iface.String(), true
		}
	}

	return "", false
}

func declaresMethod(iface *types.Interface, name string) bool {
	for i := range iface.NumMethods() {
		if iface.Method(i).Name() == name {
			return true
		}
	}

	return false
}

func sortDeclarations(in []declaration) {
	sort.Slice(in, func(i, j int) bool {
		switch {
		case in[i].Package != in[j].Package:
			return in[i].Package < in[j].Package
		case in[i].File != in[j].File:
			return in[i].File < in[j].File
		default:
			return in[i].Line < in[j].Line
		}
	})
}

func (s *scan) relative(path string) string {
	if s.root == "" {
		return path
	}

	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return path
	}

	return filepath.ToSlash(rel)
}

// trimTestVariant turns "example.com/p [example.com/p.test]" into
// "example.com/p" so a declaration reads the same however it was loaded.
func trimTestVariant(path string) string {
	if i := strings.Index(path, " ["); i >= 0 {
		path = path[:i]
	}

	return strings.TrimSuffix(path, "_test")
}

func lastSegment(name string) string {
	return name[strings.LastIndex(name, ".")+1:]
}

func isGenerated(file *ast.File) bool {
	for _, group := range file.Comments {
		// The convention places the marker before the package clause.
		if group.Pos() > file.Package {
			break
		}

		for _, c := range group.List {
			if generatedHeader.MatchString(c.Text) {
				return true
			}
		}
	}

	return false
}

func moduleRoot(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("working directory: %w", err)
		}

		dir = wd
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
			return abs, nil
		}

		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("no go.mod at or above %s", dir)
		}

		abs = parent
	}
}

type jsonReport struct {
	Unreferenced []declaration `json:"unreferenced"`
	Suppressed   []declaration `json:"suppressed"`
}

func writeJSON(w io.Writer, unreferenced, suppressed []declaration) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(jsonReport{Unreferenced: unreferenced, Suppressed: suppressed}); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	return nil
}

func writeText(w io.Writer, unreferenced, suppressed []declaration, configs tagSets, showKept bool) error {
	var b strings.Builder

	b.WriteString("unreferenced: package-level declarations no identifier in the module denotes,\n")
	b.WriteString("in any package, test or build configuration scanned")
	fmt.Fprintf(&b, " (%s).\n", describeConfigs(configs))
	b.WriteString("A cluster of dead code that only references itself still reads as referenced here;\n")
	b.WriteString("`make deadcode` is what catches that.\n\n")

	if len(unreferenced) == 0 {
		b.WriteString("Unreferenced: none\n")
	} else {
		fmt.Fprintf(&b, "Unreferenced (%d):\n\n", len(unreferenced))

		current := ""

		for _, d := range unreferenced {
			if d.Package != current {
				current = d.Package

				fmt.Fprintf(&b, "  %s\n", current)
			}

			fmt.Fprintf(&b, "    %s:%d: %s %s\n", d.File, d.Line, d.Kind, d.Name)
		}

		b.WriteString("\n")
	}

	// The kept set is large and each entry carries a full interface type, so
	// it is a count by default. It is still printed rather than dropped: a
	// suppression nobody can see is the same as a wrong answer.
	fmt.Fprintf(&b, "Kept as interface satisfaction: %d (-show-kept to list them)\n", len(suppressed))

	if showKept {
		for _, d := range suppressed {
			fmt.Fprintf(&b, "    %s:%d: %s %s satisfies %s\n", d.File, d.Line, d.Kind, d.Name, d.Interface)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	return nil
}

func describeConfigs(configs tagSets) string {
	described := make([]string, 0, len(configs))

	for _, tags := range configs {
		if tags == "" {
			tags = "no tags"
		}

		described = append(described, tags)
	}

	return "tag sets: " + strings.Join(described, "; ")
}
