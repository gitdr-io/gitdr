package dest_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
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
