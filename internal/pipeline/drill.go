package pipeline

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/gitexec"
)

// The restore drill: proving the backups restore, on a schedule, without a person.
//
// Every framework a buyer answers to wants restoration tested and the results recorded — SOC 2
// A1.3, ISO 27001 A.8.13, CIS v8.1 11.5, and unconditionally in NIS2's implementing regulation.
// Today that evidence is made by hand, once a year, with screenshots. Nobody in this category
// automates it, and the reason is structural: a vendor holding your backups in a proprietary
// blob store has nothing for you to compare against and cannot let you reproduce the check
// without publishing their format and their reader.
//
// gitdr can, because git is content addressed. A commit hash covers its tree, its blobs and its
// entire ancestry, so comparing a restored repository's ref-to-commit map against the map the
// signed manifest recorded is not a sample of the data — it is a proof that the histories are
// equal.
//
// # What a drill actually proves
//
// Three ref maps, and the drill checks both joins:
//
//	R  what the source advertised when the copy was made   (signed manifest, v3 `refs`)
//	B  what the bundle's own header declares               (git bundle list-heads)
//	C  what a fresh clone of that bundle contains          (git for-each-ref)
//
// B == C says the artifact restores to the history it claims to carry. That is what `restore`
// has checked since it learned to. R == B says the artifact claims to carry the history the
// source actually had — which nothing checked before v3 recorded R, and which is the half an
// auditor is really asking about. A bundle that restores perfectly to a history missing half
// the branches passes the first check and fails the second.
//
// # What it does not prove
//
// Not that the repository's *content* is what somebody remembers. A commit hash covers the
// tree it points at, so equal hashes mean equal content, but if the source was already wrong
// when the copy was made then the copy is faithfully wrong. A backup can only prove it
// preserved what it was given.
//
// Not that unreferenced objects survive. A clone uses the default refspec, so a bundle's
// refs/merge-requests/* and refs/notes/* arrive as objects with nothing pointing at them. They
// are counted separately and named, never quietly folded into the matched total.

// DrillSchema is the versioned identifier of the drill-report contract. Like the manifest, this
// is a stable public contract: an auditor's tooling reads it, and changing it needs a version
// bump and a note in SPEC.md.
const DrillSchema = "gitdr.drill/v1"

// DrillDeps are the inputs to a drill.
type DrillDeps struct {
	Dest          dest.Destination
	Git           *gitexec.Git
	EncryptionKey []byte
	// PublicKey verifies the manifest being drilled. Without it the drill still runs and the
	// report says the manifest's signature was not checked — a drill against an unverified
	// manifest proves the artifacts restore, not that they are the ones gitdr wrote.
	PublicKey ed25519.PublicKey
	// SigningKey signs the report. A drill report is evidence, and unsigned evidence is a
	// text file anybody can write.
	SigningKey  ed25519.PrivateKey
	ToolVersion string
	Logger      *slog.Logger
	Now         func() time.Time
}

// DrillRequest selects what to drill.
type DrillRequest struct {
	// ManifestKey is the run to drill. Empty means the most recent manifest for Host/Owner.
	ManifestKey string
	Host        string
	Owner       string
	// Sample caps how many repositories are restored. Zero means all of them.
	//
	// A partial drill is honest about being one: the report records how many were eligible and
	// how many were tried, so nobody can read a ten-repository sample as a thousand-repository
	// guarantee. Repositories are chosen in slug order, so a sample is reproducible rather
	// than a different ten every time.
	Sample int
	// WorkDir is where repositories are restored. Each is removed as soon as it is compared;
	// a drill of a large organisation would otherwise need the whole estate on disk at once.
	WorkDir string
}

// DrillReport is the evidence pack. It is signed and written to the destination beside the
// manifest it drills, so the proof is as immutable as the thing it proves.
type DrillReport struct {
	Schema      string   `json:"schema"`
	DrillID     string   `json:"drillId"`
	Tool        ToolInfo `json:"tool"`
	ManifestKey string   `json:"manifestKey"`
	// ManifestSigned records whether the manifest's own signature was checked before its
	// contents were believed. False is not a failure and is not hidden: it narrows what the
	// drill proves, from "the artifacts gitdr wrote restore" to "these artifacts restore".
	ManifestSigned bool      `json:"manifestSigned"`
	StartedAt      time.Time `json:"startedAt"`
	FinishedAt     time.Time `json:"finishedAt"`
	Status         string    `json:"status"`
	// Eligible is how many repositories the manifest records a successful copy for; Drilled
	// is how many this report actually restored. They differ when Sample is set, and a reader
	// must be able to see that without reading the repo list.
	Eligible int         `json:"eligible"`
	Drilled  int         `json:"drilled"`
	Repos    []DrillRepo `json:"repos"`
}

// DrillRepo is one repository's drill outcome.
type DrillRepo struct {
	Slug   string `json:"slug"`
	Status string `json:"status"` // success | failed
	Error  string `json:"error,omitempty"`

	// SourceRefs is how many refs the signed manifest says the source advertised. Zero for a
	// copy made by a pre-v3 gitdr, where the drill can still prove the bundle restores and
	// cannot prove it matches the source.
	SourceRefs int `json:"sourceRefs"`
	// BundleRefs is how many the bundle's own header declares.
	BundleRefs int `json:"bundleRefs"`
	// RestoredRefs is how many of those the restored repository carries at the same object.
	RestoredRefs int `json:"restoredRefs"`
	// Unreferenced names refs the bundle declares that a clone's refspec does not create.
	// Counted apart from RestoredRefs rather than folded into it.
	Unreferenced []string `json:"unreferenced,omitempty"`
	// SourceMatch says whether the bundle declares exactly what the source advertised. Null
	// when the manifest recorded no source refs, which is not the same as false.
	SourceMatch *bool `json:"sourceMatch,omitempty"`
	// Mismatches name what did not line up, in ref order, so the first one named is stable.
	Mismatches []string `json:"mismatches,omitempty"`
}

// Drill restores repositories from a signed manifest and proves what came back.
func Drill(ctx context.Context, d DrillDeps, req DrillRequest) (*DrillReport, error) {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	started := now().UTC()

	key, m, signed, err := loadManifestForDrill(ctx, d, req)
	if err != nil {
		return nil, err
	}

	eligible := make([]RepoEntry, 0, len(m.Repos))
	for _, e := range m.Repos {
		// A skipped repository has no artifact in this run; the copy it relies on belongs to
		// an earlier one and is that run's to drill. Drilling it here would report a missing
		// bundle as a failure of a backup that is fine.
		if e.Status == StatusSuccess {
			eligible = append(eligible, e)
		}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].Slug < eligible[j].Slug })

	chosen := eligible
	if req.Sample > 0 && req.Sample < len(chosen) {
		chosen = chosen[:req.Sample]
	}

	report := &DrillReport{
		Schema:         DrillSchema,
		DrillID:        newRunID(started),
		Tool:           ToolInfo{Name: "gitdr", Version: d.ToolVersion},
		ManifestKey:    key,
		ManifestSigned: signed,
		StartedAt:      started,
		Eligible:       len(eligible),
		Drilled:        len(chosen),
	}

	allOK := true
	for _, entry := range chosen {
		res := drillOne(ctx, d, req, m, entry, log)
		if res.Status != StatusSuccess {
			allOK = false
		}
		report.Repos = append(report.Repos, res)
	}

	report.FinishedAt = now().UTC()
	report.Status = statusString(allOK)

	if err := uploadDrill(ctx, d, report, req, log); err != nil {
		// The drill ran and its result is in hand; failing to store it is a separate problem
		// and the caller is told both.
		return report, fmt.Errorf("store drill report: %w", err)
	}
	if !allOK {
		return report, fmt.Errorf("drill completed with failures")
	}
	return report, nil
}

func drillOne(ctx context.Context, d DrillDeps, req DrillRequest, m *Manifest, entry RepoEntry, log *slog.Logger) DrillRepo {
	out := DrillRepo{Slug: entry.Slug, Status: StatusSuccess, SourceRefs: len(entry.Refs)}

	host, owner, name, date, err := locate(m, entry)
	if err != nil {
		out.Status = StatusFailed
		out.Error = err.Error()
		return out
	}

	dir, err := os.MkdirTemp(req.WorkDir, "gitdr-drill-")
	if err != nil {
		out.Status = StatusFailed
		out.Error = fmt.Sprintf("workdir: %v", err)
		return out
	}
	// Removed as soon as it is compared. A drill of a thousand repositories would otherwise
	// need the whole organisation on disk at once, which is the difference between a drill
	// that runs nightly and one nobody turns on.
	defer func() { _ = os.RemoveAll(dir) }()

	restored, err := Restore(ctx, RestoreDeps{
		Dest: d.Dest, Git: d.Git, EncryptionKey: d.EncryptionKey,
		PublicKey: d.PublicKey, Logger: log,
	}, RestoreRequest{
		Host: host, Owner: owner, Name: name, Date: date,
		OutDir: path.Join(dir, name),
	})
	if err != nil {
		out.Status = StatusFailed
		out.Error = err.Error()
		return out
	}

	out.BundleRefs = restored.Refs.Declared
	out.RestoredRefs = restored.Refs.Matched
	out.Unreferenced = restored.Refs.Unreferenced
	for _, mm := range restored.Refs.Mismatches {
		out.Mismatches = append(out.Mismatches, mm.String())
	}
	// No verdict on those numbers here, deliberately. `Restore` fails on a ref mismatch before
	// it returns, so a branch checking it again could never fire — and unreachable code that
	// reads as a check is worse than no check, because a reviewer counts it as one. The
	// numbers are still recorded, because they are the evidence: a reader of this report needs
	// to see how much was compared, not just that nothing objected.
	//
	// Established by deleting `if !refs.OK()` from Restore's caller and watching nothing fail.

	// The second join, and the one nothing could check before the manifest recorded source
	// refs: does what came back carry the history the source actually had? A bundle written
	// from a half-fetched mirror passes every check above and fails this one.
	//
	// Left null rather than false for a copy made by a pre-v3 gitdr. "Not recorded" and "did
	// not match" are different answers and a reader must be able to tell them apart.
	if len(entry.Refs) > 0 {
		src, err := CompareSourceRefs(ctx, d.Git, entriesToRefs(entry.Refs), path.Join(dir, name))
		match := err == nil && src.OK()
		out.SourceMatch = &match
		if !match {
			out.Status = StatusFailed
			out.Error = "the restored repository does not carry the refs the source advertised when it was copied"
			if err != nil {
				out.Error = err.Error()
			}
			for _, mm := range src.Mismatches {
				out.Mismatches = append(out.Mismatches, "source: "+mm.Describe("the source advertised"))
			}
		}
	}
	return out
}

// locate works out which dated artifact belongs to this entry.
//
// Parsed from the end of the key, not the start. An artifact lives at
//
//	{host}/{owner}/{name}/{YYYY-MM-DD}/{name}.bundle
//
// and `owner` can be more than one segment: GitLab groups nest, so a real key from a real run
// reads `gitlab.com/pitici/gitdr/gitdr/2026-09-02/gitdr.bundle`. Counting from the front gave
// owner "pitici" and name "gitdr", and the restore then looked under
// `gitlab.com/pitici/manifests/` — a path that does not exist. Every drill of a GitLab project
// in a subgroup failed with a message about the wrong date.
//
// Found by running a backup and a drill against a real project. No unit test would have: the
// fixtures all use a single-segment owner, which is every GitHub repository and only some
// GitLab ones.
func locate(m *Manifest, entry RepoEntry) (host, owner, name, date string, err error) {
	for _, a := range entry.Artifacts {
		parts := strings.Split(a.Key, "/")
		// host + owner(1+) + name + date + file
		if len(parts) < 5 {
			continue
		}
		return parts[0],
			strings.Join(parts[1:len(parts)-3], "/"),
			parts[len(parts)-3],
			parts[len(parts)-2],
			nil
	}

	owner, name, found := strings.Cut(entry.Slug, "/")
	if !found {
		return "", "", "", "", fmt.Errorf("unreadable slug %q", entry.Slug)
	}
	return m.Source.Host, owner, name, "", fmt.Errorf("no artifact key to locate %q by", entry.Slug)
}

func loadManifestForDrill(ctx context.Context, d DrillDeps, req DrillRequest) (string, *Manifest, bool, error) {
	key := req.ManifestKey
	if key == "" {
		prefix := path.Join(req.Host, req.Owner, "manifests") + "/"
		objs, err := d.Dest.List(ctx, prefix)
		if err != nil {
			return "", nil, false, fmt.Errorf("list manifests: %w", err)
		}
		var keys []string
		for _, o := range objs {
			if strings.HasSuffix(o.Key, ".manifest.json") {
				keys = append(keys, o.Key)
			}
		}
		if len(keys) == 0 {
			return "", nil, false, fmt.Errorf("no manifest to drill under %s", prefix)
		}
		sort.Strings(keys)
		key = keys[len(keys)-1]
	}

	rc, err := d.Dest.Get(ctx, key)
	if err != nil {
		return "", nil, false, fmt.Errorf("read manifest %s: %w", key, err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes+1))
	if err != nil {
		return "", nil, false, fmt.Errorf("read manifest %s: %w", key, err)
	}

	signed := false
	if d.PublicKey != nil {
		sig, err := readSig(ctx, d.Dest, key+".sig")
		if err != nil {
			return "", nil, false, fmt.Errorf("read manifest signature: %w", err)
		}
		if !ed25519.Verify(d.PublicKey, raw, sig) {
			// Refused, not recorded. Drilling a manifest whose signature does not match would
			// produce evidence about an artifact set nobody can attribute to gitdr, which is
			// worse than no evidence.
			return "", nil, false, fmt.Errorf("manifest %s does not match its signature", key)
		}
		signed = true
	}

	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", nil, false, fmt.Errorf("parse manifest %s: %w", key, err)
	}
	return key, &m, signed, nil
}

func readSig(ctx context.Context, dst dest.Destination, key string) ([]byte, error) {
	rc, err := dst.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, 1<<16))
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
}

// uploadDrill stores the report and its signature beside the manifest it drills.
//
// Written through the same create-only path as everything else, so a drill report cannot be
// replaced by a later one that says something more comfortable.
func uploadDrill(ctx context.Context, d DrillDeps, report *DrillReport, req DrillRequest, log *slog.Logger) error {
	if d.Dest == nil || d.SigningKey == nil {
		return nil
	}
	canon, err := json.Marshal(report)
	if err != nil {
		return err
	}
	base := path.Dir(path.Dir(report.ManifestKey)) // {host}/{org}
	key := path.Join(base, "drills", report.FinishedAt.UTC().Format("20060102T150405Z")+".drill.json")

	if _, err := d.Dest.PutImmutable(ctx, key, strings.NewReader(string(canon)), int64(len(canon)), dest.Retention{}); err != nil {
		return err
	}
	sig := base64.StdEncoding.EncodeToString(crypto.Sign(d.SigningKey, canon))
	if _, err := d.Dest.PutImmutable(ctx, key+".sig", strings.NewReader(sig), int64(len(sig)), dest.Retention{}); err != nil {
		return err
	}

	// Say where it went.
	//
	// The report is the evidence and nothing else names its location: the key is derived here
	// and was never printed, so an operator had no supported way to fetch the document they
	// had just produced, and a reader of the report would have had to re-derive the path from
	// the manifest key — a second copy of a rule, which drifts.
	//
	// A log line rather than a field on the report: the signature covers the report's exact
	// bytes, so a report naming its own key would have to be signed after the key was chosen,
	// and `gitdr.drill/v1` consumers would need a version bump for something no consumer of
	// the JSON needs.
	log.Info("drill report written", "key", key, "signature", key+".sig")
	return nil
}

// LocateForTest exposes locate to the package's external tests. The parsing it does was wrong
// in a way only a real repository path revealed, so it is worth testing directly rather than
// only through a drill.
func LocateForTest(m *Manifest, e RepoEntry) (string, string, string, string, error) {
	return locate(m, e)
}
