package dest_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The append-only invariant, enforced instead of asserted.
//
// Until this file existed, "no Delete/Remove/Overwrite method, anywhere" was a comment at the
// top of dest.go. The public site says it more strongly than that: "The storage layer has no
// delete, remove, or overwrite method anywhere in the codebase. Backups are append-only
// because the code physically cannot do otherwise." Nothing checked either sentence. Someone
// adding a cleanup helper to the S3 backend would have shipped green, and the claim on the
// site would have quietly become false.
//
// Two rules, and the second is the one that does the work:
//
//  1. No method on a destination may be *named* for destruction.
//  2. No code in these packages may *call* a destructive storage operation, whatever the
//     enclosing function is called. A method named Compact that calls DeleteObjects is the
//     same violation with better manners, and rule 1 alone would wave it through.
//
// This walks the source rather than using reflection, because the thing being defended is the
// absence of code. Reflection can only see what was written; only the parser can prove nothing
// was.

// Method and function names that are destruction by any other name.
var forbiddenNames = []string{
	"delete", "remove", "purge", "destroy", "overwrite", "truncate", "unlink",
	"erase", "wipe", "clobber", "expire", "prune", "clear", "drop",
}

// What the cloud SDKs call it. A method may be named anything; these are the calls that
// actually take a backup away, so this is the list that matters.
var forbiddenCalls = []string{
	// S3 and S3-compatible
	"DeleteObject", "DeleteObjects", "DeleteBucket", "DeleteBucketPolicy",
	"AbortMultipartUpload", "PutObjectRetention", "PutObjectLegalHold",
	// GCS: obj.Delete(), and the retention controls that could shorten a lock
	"ObjectHandle.Delete", "BucketHandle.Delete", "SetRetentionPolicy",
	// Azure
	"DeleteBlob", "DeleteContainer", "SetImmutabilityPolicy", "DeleteImmutabilityPolicy",
	"SetLegalHold", "Undelete",
	// Azure Resource Manager (armstorage) is not listed here. It is held to an allowlist instead,
	// in TestArmstorageReachesOnlyTheContainerRead, because no list of names keeps up with it.
}

// PutObjectRetention and SetImmutabilityPolicy are on the list deliberately. They do not
// delete anything, but they are how a lock gets shortened after the fact, which reaches the
// same outcome by a slower route. Retention belongs on the write and nowhere else.

func TestNoDestructiveMethodOnAnyDestination(t *testing.T) {
	forEachFile(t, func(t *testing.T, path string, file *ast.File, fset *token.FileSet) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
				continue // a plain function is not part of a backend's surface
			}
			if word := matchesForbidden(fn.Name.Name); word != "" {
				t.Errorf(
					"%s: method %s contains %q.\n"+
						"The Destination interface is append-only by construction and the public site "+
						"says so. If a backup can be taken away, that sentence has to come down first.",
					fset.Position(fn.Pos()), fn.Name.Name, word,
				)
			}
		}
	})
}

func TestNoDestructiveCallAnywhereInADestination(t *testing.T) {
	forEachFile(t, func(t *testing.T, path string, file *ast.File, fset *token.FileSet) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			for _, forbidden := range forbiddenCalls {
				// Compared on the last segment so "ObjectHandle.Delete" matches "obj.Delete()"
				// whatever the receiver was named at the call site.
				want := forbidden
				if i := strings.LastIndex(forbidden, "."); i >= 0 {
					want = forbidden[i+1:]
				}
				if sel.Sel.Name == want {
					t.Errorf(
						"%s: calls %s.\n"+
							"A destructive storage call inside a destination, whatever the enclosing "+
							"function is named. A method called Compact that deletes objects is the "+
							"same violation as one called Delete.",
						fset.Position(call.Pos()), sel.Sel.Name,
					)
				}
			}
			return true
		})
	})
}

// The interface itself, checked separately: it is the contract every backend is written
// against, so a destructive method appearing here would legitimise one in all of them at once.
func TestDestinationInterfaceDeclaresNothingDestructive(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "dest.go", nil, 0)
	if err != nil {
		t.Fatalf("parse dest.go: %v", err)
	}

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "Destination" {
			return true
		}
		iface, ok := spec.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		found = true
		for _, m := range iface.Methods.List {
			for _, name := range m.Names {
				if word := matchesForbidden(name.Name); word != "" {
					t.Errorf("Destination declares %s, which contains %q", name.Name, word)
				}
			}
		}
		return false
	})

	// Without this the test passes when the interface is renamed or deleted, which is the
	// exact failure mode this file exists to close.
	if !found {
		t.Fatal("no Destination interface found in dest.go: this test asserts nothing")
	}
}

func matchesForbidden(name string) string {
	lower := strings.ToLower(name)
	for _, word := range forbiddenNames {
		if strings.Contains(lower, word) {
			return word
		}
	}
	return ""
}

// Every non-test Go file under internal/dest, including the backends.
func forEachFile(t *testing.T, check func(*testing.T, string, *ast.File, *token.FileSet)) {
	t.Helper()
	fset := token.NewFileSet()
	seen := 0

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return perr
		}
		seen++
		check(t, path, file, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A walk that finds nothing is a test that asserts nothing, and it would keep passing
	// after the backends move to another directory.
	if seen < 4 {
		t.Fatalf("only %d source files walked; expected the interface plus each backend", seen)
	}
}

// armstorage is in this module for one question: is a container's immutability policy locked.
//
// The same package deletes containers, clears legal holds, replaces an unlocked policy with a
// shorter one, writes the lifecycle rules that delete blobs on a timer, and restores an account
// to an earlier point in time, which rewrites and removes blobs (BeginRestoreBlobRanges). It is
// generated, it grows with every API version, and forbiddenCalls above matches exact names, so
// a list of what not to call is always behind. What the code may reach is short and fixed, so
// that is what is listed. It is checked with the type checker rather than by name, because
// `x.Update` means nothing until you know what x is.
const armstoragePath = "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"

var armstorageAllowed = map[string]bool{
	"NewBlobContainersClient":     true, // the one client
	"(*BlobContainersClient).Get": true, // the one call: the container, with its policy's state
}

// Every function or method in armstorage that code in this module can reach is on the
// allowlist, whether it is named or reached through an interface a client is held by.
//
// Types, constants and struct fields are data and may be used freely; only what runs is checked.
// Both allowed entries must actually be reached, or this would go on passing after the code it
// guards had moved somewhere it does not look.
func TestArmstorageReachesOnlyTheContainerRead(t *testing.T) {
	pkgs := typecheckImporters(t, armstoragePath)
	if len(pkgs) == 0 {
		t.Fatal("no package in this module imports armstorage: this test asserts nothing")
	}

	reached := map[string]bool{}
	reach := func(pos token.Position, name, how string) {
		reached[name] = true
		if !armstorageAllowed[name] {
			t.Errorf("%s: %s armstorage %s, which is not on the allowlist. armstorage is here to "+
				"read whether a container's policy is locked. Another read can be added to "+
				"armstorageAllowed in review; anything that changes state cannot.", pos, how, name)
		}
	}
	for _, p := range pkgs {
		// By name: calls, method values, anything that refers to a function.
		for id, obj := range p.info.Uses {
			if fn, ok := obj.(*types.Func); ok && fn.Pkg() != nil && fn.Pkg().Path() == armstoragePath {
				reach(p.fset.Position(id.Pos()), funcName(fn), "reaches")
			}
		}
		// Through an interface. The azure backend holds its client as containerResource, which
		// declares Get. Adding Update to that interface would let code call armstorage through a
		// method this module declares, and the loop above would never see it.
		clients := armstorageClients(p.pkg)
		seen := map[*types.Interface]bool{}
		for expr, tv := range p.info.Types {
			iface, ok := tv.Type.Underlying().(*types.Interface)
			if !ok || iface.NumMethods() == 0 || seen[iface] {
				continue
			}
			seen[iface] = true
			for _, c := range clients {
				if !types.Implements(types.NewPointer(c), iface) && !types.Implements(c, iface) {
					continue
				}
				for i := range iface.NumMethods() {
					reach(p.fset.Position(expr.Pos()), "(*"+c.Obj().Name()+")."+iface.Method(i).Name(), "an interface exposes")
				}
			}
		}
	}
	for name := range armstorageAllowed {
		if !reached[name] {
			t.Errorf("armstorage %s is allowed and nothing reaches it: the code moved, or this test no longer looks where it is", name)
		}
	}
}

// funcName is how the allowlist spells a function: NewX, or (*T).M for a method.
func funcName(fn *types.Func) string {
	recv := fn.Type().(*types.Signature).Recv()
	if recv == nil {
		return fn.Name()
	}
	t := recv.Type()
	if ptr, ok := t.(*types.Pointer); ok {
		if named, ok := ptr.Elem().(*types.Named); ok {
			return "(*" + named.Obj().Name() + ")." + fn.Name()
		}
	}
	if named, ok := t.(*types.Named); ok {
		return "(" + named.Obj().Name() + ")." + fn.Name()
	}
	return fn.FullName()
}

// armstorageClients is every client type in the armstorage that pkg imports. Operations live on
// clients; everything else in the package is a model.
func armstorageClients(pkg *types.Package) []*types.Named {
	var out []*types.Named
	for _, imp := range pkg.Imports() {
		if imp.Path() != armstoragePath {
			continue
		}
		for _, name := range imp.Scope().Names() {
			isClient := strings.HasSuffix(name, "Client") || name == "ClientFactory"
			tn, ok := imp.Scope().Lookup(name).(*types.TypeName)
			if !ok || !isClient {
				continue
			}
			if named, ok := tn.Type().(*types.Named); ok {
				out = append(out, named)
			}
		}
	}
	return out
}

type typedPkg struct {
	fset *token.FileSet
	pkg  *types.Package
	info *types.Info
}

type listedPkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Imports    []string
	Export     string
}

// typecheckImporters type-checks, from source, every non-test package in this module that
// imports target, reading everything they import from the compiler's own export data.
//
// Standard library and the go command only, so guarding a dependency adds none. `go test` puts
// its own GOROOT/bin first on PATH, so "go" here is the toolchain that built this test and its
// export data is what go/importer expects.
func typecheckImporters(t *testing.T, target string) []typedPkg {
	t.Helper()
	list := func(args ...string) []listedPkg {
		t.Helper()
		out, err := exec.Command("go", append([]string{"list", "-json"}, args...)...).Output()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				t.Fatalf("go list %v: %v\n%s", args, err, exit.Stderr)
			}
			t.Fatalf("go list %v: %v", args, err)
		}
		var pkgs []listedPkg
		for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); {
			var p listedPkg
			if err := dec.Decode(&p); err != nil {
				t.Fatalf("read go list output: %v", err)
			}
			pkgs = append(pkgs, p)
		}
		return pkgs
	}

	var importers []listedPkg
	for _, p := range list("gitdr.io/gitdr/...") {
		if slices.Contains(p.Imports, target) {
			importers = append(importers, p)
		}
	}
	if len(importers) == 0 {
		return nil
	}
	args := []string{"-export", "-deps"}
	for _, p := range importers {
		args = append(args, p.ImportPath)
	}
	exports := map[string]string{}
	for _, p := range list(args...) {
		if p.Export != "" {
			exports[p.ImportPath] = p.Export
		}
	}

	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
	var out []typedPkg
	for _, p := range importers {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			files = append(files, f)
		}
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
		pkg, err := (&types.Config{Importer: imp}).Check(p.ImportPath, fset, files, info)
		if err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		out = append(out, typedPkg{fset: fset, pkg: pkg, info: info})
	}
	return out
}
