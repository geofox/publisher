package drivers

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

// The core works from manifests and capabilities, never from destination
// names: a destination added later (a third-party plug-in) must not need a
// core change. This fails when a core package's non-test Go code names a
// built-in destination type in a string literal.
func TestCoreNamesNoDestination(t *testing.T) {
	Register()
	core := []string{"../dispatch", "../api", "../store", "../accounts", "../../cmd/publisher"}
	lit := regexp.MustCompile(`"(bluesky|mastodon|nostr|threads)"`)
	for _, dir := range core {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
					continue
				}
				if m := lit.FindString(line); m != "" {
					t.Errorf("%s:%d names destination %s; use manifests/capabilities instead", f, i+1, m)
				}
			}
		}
	}
}

// The server and the account registry reach destinations only through
// this package and the destination contract: neither imports a driver's
// package, so a driver added later needs no change there.
func TestCoreImportsNoDriver(t *testing.T) {
	Register()
	driverPkgs := map[string]string{}
	for _, typ := range destination.TypesInOrder() {
		d, _ := destination.Lookup(typ)
		rt := reflect.TypeOf(d)
		for rt.Kind() == reflect.Pointer {
			rt = rt.Elem()
		}
		driverPkgs[rt.PkgPath()] = typ
	}
	if len(driverPkgs) < 4 {
		t.Fatalf("driver packages = %v", driverPkgs)
	}
	for _, dir := range []string{"../../cmd/publisher", "../accounts"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range parsed.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if typ, ok := driverPkgs[path]; ok {
					t.Errorf("%s imports the %s driver (%s); go through internal/drivers or the destination contract", f, typ, path)
				}
			}
		}
	}
}
