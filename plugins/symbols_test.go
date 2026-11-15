package plugins

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestSymbolTableCoversThePluginAPI guards against the generated symbol table
// falling behind plugins/api.
//
// The table is what the interpreter resolves a plugin's imports against, so
// anything added to the API but missing from it does not exist as far as a
// plugin is concerned. The failure is confusing: the package compiles, the
// tests of the Go side pass, and only an interpreted plugin using the new type
// fails, with "undefined selector" at a line in the plugin's own source. This
// turns that into a test failure naming the missing identifier.
//
// Regenerate with:
//
//	make plugin-symbols
func TestSymbolTableCoversThePluginAPI(t *testing.T) {
	exported := exportedAPIIdentifiers(t)

	table, ok := Symbols["github.com/pelican/wings/plugins/api/api"]
	if !ok {
		t.Fatal("the plugin API is not in the symbol table at all; run make plugin-symbols")
	}

	var missing []string
	for _, name := range exported {
		if _, present := table[name]; !present {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("the symbol table is out of date, so plugins cannot use these: %s\nrun: make plugin-symbols",
			strings.Join(missing, ", "))
	}
}

// exportedAPIIdentifiers lists every exported type, function, constant and
// variable declared in the plugin API package.
func exportedAPIIdentifiers(t *testing.T) []string {
	t.Helper()

	dir := filepath.Join("api")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("could not read the api package: %v", err)
	}

	var names []string
	fset := token.NewFileSet()

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("could not parse %s: %v", entry.Name(), err)
		}

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				// Methods belong to their type, which is itself exported, so
				// only plain functions are named in the table.
				if d.Recv == nil && d.Name.IsExported() {
					names = append(names, d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							names = append(names, s.Name.Name)
						}
					case *ast.ValueSpec:
						for _, ident := range s.Names {
							if ident.IsExported() {
								names = append(names, ident.Name)
							}
						}
					}
				}
			}
		}
	}

	if len(names) == 0 {
		t.Fatal("found no exported identifiers in the api package, which cannot be right")
	}
	return names
}
