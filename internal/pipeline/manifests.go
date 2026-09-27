package pipeline

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/source"
)

// Where a run files its manifest, and the one way anything reads a manifest back.
//
// A backup writes one manifest per run. Restore, drill and the next backup all read manifests
// back, and each used to find and parse them its own way: restore looked only under the
// repository's own namespace and the copy's date, a drill took the lexically last key under a
// prefix, and the next backup did the same without asking what it had taken. Each of those rules
// broke on something real. These are shared, so they hold or break together.

// manifestStamp is how a manifest is named: the UTC second its run finished.
const manifestStamp = "20060102T150405Z"

// manifestSuffix ends the key of every run-manifest.
const manifestSuffix = ".manifest.json"

// manifestSchemaPrefix starts the schema of every run-manifest gitdr has written.
const manifestSchemaPrefix = "gitdr.manifest/"

// maxSignatureBytes caps a detached signature read. One is 88 bytes of base64.
const maxSignatureBytes = 1 << 16

// manifestName is the whole name of a manifest object. The signature beside it, a project named
// like a date and anything else that shares the prefix do not match.
var manifestName = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z\.manifest\.json$`)

// manifestAnchor is the deepest namespace that holds every repository in the run, compared path
// segment by path segment, byte for byte. On GitHub it is the organisation or user. On GitLab it is
// the group above every subgroup the run covers, and "" when the run spans namespaces that share
// nothing, which files the manifest at the host.
//
// It replaced the first repository's owner, which was whichever namespace the source happened to
// list first. That changed whenever a project was created, and each change filed the manifest
// where the next run did not look for it, so the next run copied everything again.
func manifestAnchor(repos []source.Repo) string {
	if len(repos) == 0 {
		return ""
	}
	shared := strings.Split(repos[0].Owner, "/")
	for _, r := range repos[1:] {
		segs := strings.Split(r.Owner, "/")
		n := 0
		for n < len(shared) && n < len(segs) && shared[n] == segs[n] {
			n++
		}
		shared = shared[:n]
	}
	return strings.Join(shared, "/")
}

// manifestDir is where a run over these repositories files its manifest, and so where the next
// run over them finds the previous one.
func manifestDir(repos []source.Repo) string {
	if len(repos) == 0 {
		return ""
	}
	return path.Join(repos[0].Host, manifestAnchor(repos), "manifests")
}

// manifestSearchDirs is where a restore without -manifest looks for the manifest of a repository
// in namespace owner: the namespace itself, each namespace above it, then the host. A run files its
// manifest under the deepest namespace holding everything it copied, which for any repository it
// copied is one of these, and never one below or beside it.
func manifestSearchDirs(host, owner string) []string {
	var dirs []string
	ns := owner
	for ns != "" {
		dirs = append(dirs, path.Join(host, ns, "manifests"))
		i := strings.LastIndex(ns, "/")
		if i < 0 {
			break
		}
		ns = ns[:i]
	}
	return append(dirs, path.Join(host, "manifests"))
}

// filedManifests returns the manifests filed directly in dir, newest first, out of a listing
// that may reach further.
//
// Object stores list by prefix, recursively. A listing of {host}/acme/manifests/ also returns the
// manifests of a GitLab subgroup called acme/manifests, filed one level down, and those sort after
// every manifest of acme itself. Taking the last key took theirs.
func filedManifests(objs []dest.Object, dir string) []string {
	var keys []string
	for _, o := range objs {
		if path.Dir(o.Key) == dir && manifestName.MatchString(path.Base(o.Key)) {
			keys = append(keys, o.Key)
		}
	}
	// A fixed-width UTC timestamp, so lexical order is chronological.
	slices.Sort(keys)
	slices.Reverse(keys)
	return keys
}

// newestManifest is the key of the newest manifest filed directly in dir whose name is not later
// than now, or "" when there is none.
//
// A name is only a claim. A byte copy of an old manifest and its signature, stored under a name
// in the future, stayed the newest for good, and every drill that picked the newest after it
// tested the old run. The loader refuses such a copy by its name. This keeps it from being picked
// before that, so it cannot stop every drill either.
func newestManifest(ctx context.Context, d dest.Destination, dir string, now time.Time) (string, error) {
	objs, err := d.List(ctx, dir+"/")
	if err != nil {
		return "", fmt.Errorf("list manifests under %s/: %w", dir, err)
	}
	for _, key := range filedManifests(objs, dir) {
		at, err := time.Parse(manifestStamp, strings.TrimSuffix(path.Base(key), manifestSuffix))
		if err != nil || at.After(now) {
			continue
		}
		return key, nil
	}
	return "", nil
}

// manifestRefused is a manifest that was read and is not what its key says it is: its signature
// does not hold, it is some other document, or it is not named for the run it records. A manifest
// that could not be read is a different failure and is not one of these.
type manifestRefused struct{ reason string }

func (e *manifestRefused) Error() string { return e.reason }

func refuseManifest(format string, args ...any) error {
	return &manifestRefused{reason: fmt.Sprintf(format, args...)}
}

// loadManifest reads the run-manifest at key. Drill, restore and the next backup all read
// manifests through it, and nothing else in gitdr does.
//
// In this order: at most maxManifestBytes are read; with a public key, the detached signature has
// to hold over those exact bytes before any of them is parsed; the schema has to start with
// gitdr.manifest/; and the name has to be the document's own finishedAt.
//
// The schema, because a drill report signed with the same key parses as a manifest with no
// repositories, and a drill that took one recorded manifestSigned over a document that is not a
// manifest. `verify` already refused it.
//
// The name, because a signature covers a document and not the place it is stored. A byte copy of
// an old manifest and its .sig under a later name verifies, and whatever picked the newest name
// picked the old run. Every manifest gitdr has written is named for its finishedAt, so one named
// for anything else is a copy.
func loadManifest(ctx context.Context, d dest.Destination, pub ed25519.PublicKey, key string) (*Manifest, error) {
	raw, err := readCapped(ctx, d, key, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", key, err)
	}
	if pub != nil {
		b64, err := readCapped(ctx, d, key+".sig", maxSignatureBytes)
		if err != nil {
			return nil, fmt.Errorf("read the signature of manifest %s: %w", key, err)
		}
		sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b64)))
		if err != nil {
			return nil, refuseManifest("manifest %s does not match its signature, which is not base64: %v", key, err)
		}
		if err := crypto.Verify(pub, raw, sig); err != nil {
			return nil, refuseManifest("manifest %s does not match its signature: %v", key, err)
		}
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", key, err)
	}
	if !strings.HasPrefix(m.Schema, manifestSchemaPrefix) {
		return nil, refuseManifest("%s is a %q document, not a run-manifest: refusing it", key, m.Schema)
	}
	if path.Base(key) != m.FinishedAt.UTC().Format(manifestStamp)+manifestSuffix {
		return nil, refuseManifest("%s is named for a run that finished at %s, but it records finishedAt %s: it is not the manifest its name says; refusing it",
			key, namedFinish(key), m.FinishedAt.UTC().Format(time.RFC3339))
	}
	return &m, nil
}

// namedFinish is the finish time a manifest's name claims, for a refusal to quote.
func namedFinish(key string) string {
	stamp := strings.TrimSuffix(path.Base(key), manifestSuffix)
	if at, err := time.Parse(manifestStamp, stamp); err == nil {
		return at.Format(time.RFC3339)
	}
	return strconv.Quote(stamp)
}

// readCapped reads the object at key, and refuses one larger than limit rather than truncating
// it.
func readCapped(ctx context.Context, d dest.Destination, key string, limit int64) ([]byte, error) {
	rc, err := d.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return b, nil
}
