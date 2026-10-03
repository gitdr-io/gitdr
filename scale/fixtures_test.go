//go:build scale

package scale

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fixtures, written with git fast-import from a fixed seed and fixed dates, so a profile gives
// the same repositories, object for object, on every run and every machine.

const fixtureEpoch = 1767225600 // 2026-01-01T00:00:00Z

// An org is one GitHub organisation on the forge, seen by one App installation.
type org struct {
	owner   string
	install int64
	names   []string // listing order
	// changing are the repositories a simulated day may change.
	changing []string
	empty    string // the repository with no commits, if the org has one
	lfs      string // the small LFS repository, if the org has one
}

func (o *org) slug(name string) string { return o.owner + "/" + name }

type seeded struct {
	once sync.Once
	org  *org
	err  error
}

var (
	smallOrg, killOrg, refsOrg, bigOrg, rateOrg seeded
)

func (h *harness) needOrg(t *testing.T, s *seeded, seed func(*forge) (*org, error)) *org {
	t.Helper()
	f := h.needForge(t)
	s.once.Do(func() {
		start := time.Now()
		s.org, s.err = seed(f)
		if s.err == nil {
			fmt.Fprintf(os.Stderr, "scale: seeded %s, %d repositories, in %s\n", s.org.owner, len(s.org.names), time.Since(start).Round(time.Millisecond))
		}
	})
	if s.err != nil {
		t.Fatalf("harness: seeding fixtures: %v", s.err)
	}
	return s.org
}

// ---- the organisations ----

// seedSmall is the day-cycle organisation: Repos small repositories, every hundredth with 200
// branches and 200 tags, one repository with issues and no commits, and one small LFS repository
// with an object only a pull request reaches.
func (h *harness) seedSmall(f *forge) (*org, error) {
	o := &org{owner: "scale-small", install: 1, empty: "empty-with-issues", lfs: "lfs-small"}
	for i := 1; i <= h.prof.Repos; i++ {
		o.names = append(o.names, fmt.Sprintf("repo-%04d", i))
	}
	o.changing = append([]string(nil), o.names...)
	err := parallel(len(o.names), func(i int) error {
		name := o.names[i]
		dir := f.repoDir(o.owner, name)
		if err := initBare(dir); err != nil {
			return err
		}
		return fastImport(dir, bytes.NewReader(smallStream(name, i+1)))
	})
	if err != nil {
		return nil, err
	}
	for _, name := range o.names {
		f.addRepo(o.owner, name, 12, plainMeta())
	}

	if err := initBare(f.repoDir(o.owner, o.empty)); err != nil {
		return nil, err
	}
	f.addRepo(o.owner, o.empty, 0, issuesMeta())
	o.names = append(o.names, o.empty)

	if err := h.seedLFS(f, o.owner, o.lfs, 3, 64<<10); err != nil {
		return nil, err
	}
	o.names = append(o.names, o.lfs)

	f.addInstallation(o.install, slugs(o))
	return o, nil
}

// seedKill is the kill-matrix organisation: a dozen repositories of 256 KiB, so each clone and
// upload takes long enough to stop in the middle of.
func (h *harness) seedKill(f *forge) (*org, error) {
	o := &org{owner: "scale-kill", install: 2}
	for i := 1; i <= h.prof.KillRepos; i++ {
		o.names = append(o.names, fmt.Sprintf("kill-%02d", i))
	}
	err := parallel(len(o.names), func(i int) error {
		dir := f.repoDir(o.owner, o.names[i])
		if err := initBare(dir); err != nil {
			return err
		}
		var fi fastImportStream
		blob := fi.blob(detBytes("kill/"+o.names[i], 256<<10))
		readme := fi.blob([]byte("# " + o.names[i] + "\n"))
		fi.commit("refs/heads/main", fixtureEpoch+int64(i), "initial", "", map[string]int{"data.bin": blob, "README.md": readme})
		return fastImport(dir, bytes.NewReader(fi.Bytes()))
	})
	if err != nil {
		return nil, err
	}
	for _, name := range o.names {
		f.addRepo(o.owner, name, 260, plainMeta())
	}
	f.addInstallation(o.install, slugs(o))
	return o, nil
}

// seedRefs is RefsRepos repositories of RefsPerRepo refs each, half of them refs/pull/*, over a
// thousand commits.
//
// The refs are written as a packed-refs file. fast-import with a hundred thousand resets takes
// about forty seconds and pack-refs another eighty, per repository.
func (h *harness) seedRefs(f *forge) (*org, error) {
	o := &org{owner: "scale-refs", install: 3}
	for i := 1; i <= h.prof.RefsRepos; i++ {
		o.names = append(o.names, fmt.Sprintf("refs-%d", i))
	}
	const commits = 1000
	err := parallel(len(o.names), func(i int) error {
		name := o.names[i]
		dir := f.repoDir(o.owner, name)
		if err := initBareFiles(dir); err != nil {
			return err
		}
		var fi fastImportStream
		marks := make([]int, commits)
		for c := range commits {
			content := fi.blob(fmt.Appendf(nil, "%s commit %d\n", name, c))
			from := ""
			if c > 0 {
				from = fmt.Sprintf(":%d", marks[c-1])
			}
			marks[c] = fi.commit("refs/heads/main", fixtureEpoch+int64(c), fmt.Sprintf("commit %d", c), from, map[string]int{"f.txt": content})
		}
		marksFile := filepath.Join(dir, "fixture-marks")
		if err := fastImport(dir, bytes.NewReader(fi.Bytes()), "--export-marks="+marksFile); err != nil {
			return err
		}
		oids, err := readMarks(marksFile)
		if err != nil {
			return err
		}
		_ = os.Remove(marksFile)
		refs := make([]string, 0, h.prof.RefsPerRepo)
		pulls := h.prof.RefsPerRepo / 2
		branches := (h.prof.RefsPerRepo - pulls) / 2
		for k := range h.prof.RefsPerRepo {
			oid := oids[marks[k%commits]]
			var ref string
			switch {
			case k < pulls:
				ref = fmt.Sprintf("refs/pull/%d/head", k+1)
			case k < pulls+branches:
				ref = fmt.Sprintf("refs/heads/branch-%06d", k)
			default:
				ref = fmt.Sprintf("refs/tags/tag-%06d", k)
			}
			refs = append(refs, oid+" "+ref)
		}
		return writePackedRefs(dir, refs)
	})
	if err != nil {
		return nil, err
	}
	for _, name := range o.names {
		f.addRepo(o.owner, name, 200, plainMeta())
	}
	f.addInstallation(o.install, slugs(o))
	return o, nil
}

// seedBig is a repository whose pack is BigPackBytes of random data, one whose large files the
// source sends as chains of deltas, one with BigLFSBytes of LFS objects, an eighth of them
// reachable only from a pull request, and two small ones, which a run has in flight beside them.
func (h *harness) seedBig(f *forge) (*org, error) {
	o := &org{owner: "scale-big", install: 4, names: []string{"big-pack", "big-deltas", "lfs-heavy", "small-1", "small-2"}}
	for i, name := range o.names[3:] {
		dir := f.repoDir(o.owner, name)
		if err := initBare(dir); err != nil {
			return nil, err
		}
		if err := fastImport(dir, bytes.NewReader(smallStream(name, i+1))); err != nil {
			return nil, err
		}
		f.addRepo(o.owner, name, 12, plainMeta())
	}
	dir := f.repoDir(o.owner, "big-pack")
	if err := initBare(dir); err != nil {
		return nil, err
	}
	// Above the threshold the forge's git tries no deltas, which random data never gains from,
	// so serving the pack costs the harness no delta search of its own.
	if err := gitIn(dir, "config", "core.bigFileThreshold", "1m"); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(bigPackStream(pw, h.prof.BigPackBytes)) }()
	if err := fastImport(dir, pr, "--big-file-threshold=1m"); err != nil {
		return nil, err
	}
	f.addRepo(o.owner, "big-pack", h.prof.BigPackBytes>>10, plainMeta())

	const deltaFiles, deltaVersions, deltaFileSize = 8, 8, 48 << 20
	if err := importDeltas(f.repoDir(o.owner, "big-deltas"), deltaFiles, deltaVersions, deltaFileSize); err != nil {
		return nil, err
	}
	f.addRepo(o.owner, "big-deltas", deltaFiles*deltaFileSize>>10, plainMeta())

	const lfsObject = 128 << 20
	count := int((h.prof.BigLFSBytes + lfsObject - 1) / lfsObject)
	if err := h.seedLFS(f, o.owner, "lfs-heavy", count, lfsObject); err != nil {
		return nil, err
	}
	f.addInstallation(o.install, slugs(o))
	return o, nil
}

// bigPackStream writes a fast-import stream of total bytes of random blobs and one commit holding
// them. It is generated as it is read: it never sits in memory.
//
// The blobs are 96 MiB and 48 MiB in turn, the files a repository on GitHub can have, where 100
// MiB is the limit on one. They are also the ones that cost a copy memory: git's delta search
// takes every blob under core.bigFileThreshold, 512 MiB by default, and holds a window of them,
// and their indexes, at once.
func bigPackStream(w io.Writer, total int64) error {
	sizes := [...]int64{96 << 20, 48 << 20}
	// A bufio.Writer keeps its first error, so the one Flush returns covers every write before it.
	bw := bufio.NewWriterSize(w, 1<<20)
	files := map[string]int{}
	mark := 0
	for left := total; left > 0; {
		n := min(left, sizes[mark%len(sizes)])
		left -= n
		mark++
		_, _ = fmt.Fprintf(bw, "blob\nmark :%d\ndata %d\n", mark, n)
		if _, err := io.CopyN(bw, detStream(fmt.Sprintf("big-pack/%d", mark)), n); err != nil {
			return err
		}
		_, _ = bw.WriteString("\n")
		files[fmt.Sprintf("blobs/big-%02d.bin", mark)] = mark
	}
	fi := fastImportStream{mark: mark}
	fi.commit("refs/heads/main", fixtureEpoch, "a pack past five gibibytes", "", files)
	_, _ = bw.Write(fi.Bytes())
	return bw.Flush()
}

// importDeltas makes dir a bare repository holding deltaStream's files.
func importDeltas(dir string, files, versions, size int) error {
	if err := initBare(dir); err != nil {
		return err
	}
	// Up to fastimport.unpackLimit objects, 100 by default, fast-import unpacks what it wrote into
	// loose objects, which carry no deltas.
	if err := gitIn(dir, "config", "fastimport.unpackLimit", "0"); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(deltaStream(pw, files, versions, size)) }()
	return fastImport(dir, pr)
}

// deltaStream writes a fast-import stream of files files of size bytes, each in versions versions
// a few KiB apart, and a commit for each version. fast-import stores a blob as a delta of the one
// written just before it, so the versions of a file, written one after another, become a chain of
// deltas. A clone resolves the chains side by side, a thread to a chain, and each thread holds
// whole versions while it does: the memory git's thread count multiplies.
func deltaStream(w io.Writer, files, versions, size int) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	marks := make([][]int, versions)
	for v := range marks {
		marks[v] = make([]int, files)
	}
	mark := 0
	for f := range files {
		data := detBytes(fmt.Sprintf("big-deltas/%d", f), size)
		for v := range versions {
			// Each version after the first rewrites 512 bytes at eight places.
			for k := range 8 {
				if v > 0 {
					seed := fmt.Sprintf("big-deltas/%d/%d/%d", f, v, k)
					copy(data[detUint(seed)%uint64(size-512):], detBytes(seed, 512))
				}
			}
			mark++
			marks[v][f] = mark
			_, _ = fmt.Fprintf(bw, "blob\nmark :%d\ndata %d\n", mark, size)
			_, _ = bw.Write(data)
			_, _ = bw.WriteString("\n")
		}
	}
	fi := fastImportStream{mark: mark}
	from := ""
	for v := range versions {
		tree := map[string]int{}
		for f := range files {
			tree[fmt.Sprintf("assets/asset-%02d.bin", f)] = marks[v][f]
		}
		c := fi.commit("refs/heads/main", fixtureEpoch+int64(v)*86400, fmt.Sprintf("assets, version %d", v+1), from, tree)
		from = fmt.Sprintf(":%d", c)
	}
	_, _ = bw.Write(fi.Bytes())
	return bw.Flush()
}

// The big-deltas repository is worth its time only if the source holds chains of deltas for a
// clone to resolve. Needs no Docker: `go test -tags scale -run DeltaFixture ./scale`.
func TestDeltaFixtureIsChainsOfDeltas(t *testing.T) {
	const files, versions = 2, 4
	dir := filepath.Join(t.TempDir(), "deltas.git")
	if err := importDeltas(dir, files, versions, 1<<20); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "cat-file", "--batch-all-objects", "--batch-check=%(objecttype) %(deltabase)")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	blobs, deltas := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if typ, base, _ := strings.Cut(line, " "); typ == "blob" {
			blobs++
			if strings.Trim(base, "0") != "" {
				deltas++
			}
		}
	}
	if blobs != files*versions || deltas != files*(versions-1) {
		t.Errorf("%d blobs, %d of them deltas; want %d and %d", blobs, deltas, files*versions, files*(versions-1))
	}
}

// seedRate is RateRepos tiny repositories under an installation whose budget the scenario sets.
func (h *harness) seedRate(f *forge) (*org, error) {
	o := &org{owner: "scale-rate", install: 6}
	for i := 1; i <= h.prof.RateRepos; i++ {
		o.names = append(o.names, fmt.Sprintf("rate-%02d", i))
	}
	err := parallel(len(o.names), func(i int) error {
		dir := f.repoDir(o.owner, o.names[i])
		if err := initBare(dir); err != nil {
			return err
		}
		return fastImport(dir, bytes.NewReader(smallStream(o.names[i], i+1)))
	})
	if err != nil {
		return nil, err
	}
	for _, name := range o.names {
		f.addRepo(o.owner, name, 12, plainMeta())
	}
	f.addInstallation(o.install, slugs(o))
	return o, nil
}

// seedLFS writes count LFS objects of size bytes into the forge's LFS store and a repository
// whose main branch points at all but the last eighth of them; a pull request ref adds the rest.
func (h *harness) seedLFS(f *forge, owner, name string, count int, size int64) error {
	dir := f.repoDir(owner, name)
	if err := initBare(dir); err != nil {
		return err
	}
	store := f.lfsDir(owner, name)
	if err := os.MkdirAll(store, 0o755); err != nil {
		return err
	}
	pointers := make([][]byte, count)
	var total int64
	err := parallel(count, func(i int) error {
		oid, err := writeLFSObject(store, fmt.Sprintf("%s/%s/lfs-%d", owner, name, i), size)
		if err != nil {
			return err
		}
		pointers[i] = fmt.Appendf(nil, "version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size)
		return nil
	})
	if err != nil {
		return err
	}
	total = int64(count) * size
	onlyPR := max(count/8, 1)
	var fi fastImportStream
	files := map[string]int{
		".gitattributes": fi.blob([]byte("*.bin filter=lfs diff=lfs merge=lfs -text\n")),
		"README.md":      fi.blob([]byte("# " + name + "\n")),
	}
	for i := range count - onlyPR {
		files[fmt.Sprintf("assets/obj-%03d.bin", i)] = fi.blob(pointers[i])
	}
	mainTip := fi.commit("refs/heads/main", fixtureEpoch, "assets", "", files)
	prFiles := map[string]int{}
	for i := count - onlyPR; i < count; i++ {
		prFiles[fmt.Sprintf("assets/obj-%03d.bin", i)] = fi.blob(pointers[i])
	}
	fi.commit("refs/pull/1/head", fixtureEpoch+60, "more assets, in a pull request", fmt.Sprintf(":%d", mainTip), prFiles)
	if err := fastImport(dir, bytes.NewReader(fi.Bytes())); err != nil {
		return err
	}
	f.addRepo(owner, name, total>>10, plainMeta())
	return nil
}

// writeLFSObject writes size deterministic bytes into store under their SHA-256 and returns it.
func writeLFSObject(store, seed string, size int64) (string, error) {
	tmp, err := os.CreateTemp(store, ".incoming-")
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	w := bufio.NewWriterSize(io.MultiWriter(tmp, sum), 1<<20)
	if _, err := io.CopyN(w, detStream(seed), size); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	oid := hex.EncodeToString(sum.Sum(nil))
	return oid, os.Rename(tmp.Name(), filepath.Join(store, oid))
}

// changeDay moves the main branch of the org's repositories chosen for day d, ChangePercent in
// a hundred of them, and returns their names.
func (h *harness) changeDay(t *testing.T, o *org, day int) []string {
	t.Helper()
	f := h.needForge(t)
	var picked []string
	for _, name := range o.changing {
		if int(detUint(fmt.Sprintf("%s/day-%d", name, day))%100) < h.prof.ChangePercent {
			picked = append(picked, name)
		}
	}
	err := parallel(len(picked), func(i int) error {
		var fi fastImportStream
		changes := fi.blob(fmt.Appendf(nil, "changed on simulated day %d\n%x\n", day, detBytes(fmt.Sprintf("%s/changes-%d", picked[i], day), 64)))
		fi.commit("refs/heads/main", fixtureEpoch+int64(day)*86400, fmt.Sprintf("day %d", day), "refs/heads/main^0", map[string]int{"CHANGES.md": changes})
		return fastImport(f.repoDir(o.owner, picked[i]), bytes.NewReader(fi.Bytes()))
	})
	if err != nil {
		t.Fatalf("harness: changing day %d: %v", day, err)
	}
	return picked
}

// ---- content ----

func smallStream(name string, i int) []byte {
	var fi fastImportStream
	readme := fi.blob(fmt.Appendf(nil, "# %s\n\nA fixture of the gitdr scale harness.\n", name))
	data := fi.blob(detBytes("small/"+name+"/data", 2048+int(detUint(name)%6144)))
	first := fi.commit("refs/heads/main", fixtureEpoch+int64(i), "initial", "", map[string]int{"README.md": readme, "data.bin": data})
	notes := fi.blob(detBytes("small/"+name+"/notes", 512))
	second := fi.commit("refs/heads/main", fixtureEpoch+int64(i)+60, "notes", fmt.Sprintf(":%d", first), map[string]int{"notes.txt": notes})
	fi.reset("refs/tags/v1.0", second)
	// Every hundredth, like a busy repository in a real organisation.
	if i%100 == 0 {
		for k := range 200 {
			fi.reset(fmt.Sprintf("refs/heads/topic-%03d", k), []int{first, second}[k%2])
			fi.reset(fmt.Sprintf("refs/tags/r%03d", k), []int{first, second}[k%2])
		}
	}
	return fi.Bytes()
}

func plainMeta() repoMeta {
	return repoMeta{Labels: []map[string]any{
		{"id": 1, "name": "bug", "color": "d73a4a", "default": true},
		{"id": 2, "name": "enhancement", "color": "a2eeef", "default": true},
	}}
}

// issuesMeta is for the repository with no commits: what it has is its issues.
func issuesMeta() repoMeta {
	m := plainMeta()
	m.Milestones = []map[string]any{{"id": 1, "number": 1, "title": "v1", "state": "open"}}
	for n := 1; n <= 3; n++ {
		m.Issues = append(m.Issues, map[string]any{
			"id": n, "number": n, "title": fmt.Sprintf("issue %d", n), "state": "open",
			"user": map[string]any{"login": "octo"}, "comments": 1, "created_at": "2026-01-02T00:00:00Z",
		})
		m.Comments = append(m.Comments, map[string]any{"id": 100 + n, "body": "a comment", "user": map[string]any{"login": "octo"}})
	}
	return m
}

func slugs(o *org) []string {
	out := make([]string, len(o.names))
	for i, n := range o.names {
		out[i] = o.slug(n)
	}
	return out
}

// detStream is an endless deterministic byte stream for seed.
func detStream(seed string) io.Reader {
	s := sha3.NewSHAKE128()
	_, _ = s.Write([]byte(seed))
	return s
}

func detBytes(seed string, n int) []byte {
	b := make([]byte, n)
	_, _ = io.ReadFull(detStream(seed), b)
	return b
}

func detUint(seed string) uint64 {
	sum := sha256.Sum256([]byte(seed))
	return binary.BigEndian.Uint64(sum[:8])
}

// ---- git ----

// fastImportStream builds a git fast-import stream.
type fastImportStream struct {
	buf  bytes.Buffer
	mark int
}

func (fi *fastImportStream) Bytes() []byte { return fi.buf.Bytes() }

func (fi *fastImportStream) blob(data []byte) int {
	fi.mark++
	fmt.Fprintf(&fi.buf, "blob\nmark :%d\ndata %d\n", fi.mark, len(data))
	fi.buf.Write(data)
	fi.buf.WriteString("\n")
	return fi.mark
}

// commit writes a commit on ref with files changed from from (a mark as ":n", a ref, or "" for
// none) and returns its mark.
func (fi *fastImportStream) commit(ref string, when int64, msg, from string, files map[string]int) int {
	fi.mark++
	fmt.Fprintf(&fi.buf, "commit %s\nmark :%d\n", ref, fi.mark)
	fmt.Fprintf(&fi.buf, "author Scale Harness <scale@gitdr.test> %d +0000\n", when)
	fmt.Fprintf(&fi.buf, "committer Scale Harness <scale@gitdr.test> %d +0000\n", when)
	fmt.Fprintf(&fi.buf, "data %d\n%s\n", len(msg), msg)
	if from != "" {
		fmt.Fprintf(&fi.buf, "from %s\n", from)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(&fi.buf, "M 100644 :%d %s\n", files[p], p)
	}
	fi.buf.WriteString("\n")
	return fi.mark
}

func (fi *fastImportStream) reset(ref string, mark int) {
	fmt.Fprintf(&fi.buf, "reset %s\nfrom :%d\n\n", ref, mark)
}

func initBare(dir string) error {
	return runGit("", "init", "--bare", "--quiet", "--initial-branch=main", dir)
}

// initBareFiles makes a repository with the files ref backend, whose packed-refs file the refs
// fixture writes directly.
func initBareFiles(dir string) error {
	err := runGit("", "init", "--bare", "--quiet", "--initial-branch=main", "--ref-format=files", dir)
	if err != nil && strings.Contains(err.Error(), "ref-format") {
		return initBare(dir) // a git older than the flag has only the files backend
	}
	return err
}

func fastImport(dir string, stream io.Reader, args ...string) error {
	full := append([]string{"-c", "pack.compression=0", "-c", "core.compression=0", "fast-import", "--quiet"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Stdin = stream
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fast-import into %s: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func gitIn(dir string, args ...string) error { return runGit(dir, args...) }

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func readMarks(path string) (map[int]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		var mark int
		var oid string
		if _, err := fmt.Sscanf(line, ":%d %s", &mark, &oid); err != nil {
			return nil, fmt.Errorf("marks line %q: %w", line, err)
		}
		out[mark] = oid
	}
	return out, nil
}

// writePackedRefs writes "oid name" lines as the repository's packed-refs, sorted, the way git
// would.
func writePackedRefs(dir string, lines []string) error {
	sort.Slice(lines, func(i, j int) bool {
		return strings.SplitN(lines[i], " ", 2)[1] < strings.SplitN(lines[j], " ", 2)[1]
	})
	var b bytes.Buffer
	b.WriteString("# pack-refs with: peeled fully-peeled sorted \n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return os.WriteFile(filepath.Join(dir, "packed-refs"), b.Bytes(), 0o644)
}

// readDirNames lists the names in dir, leaving out dot files.
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// parallel runs fn for 0..n-1 on every CPU and returns the first error.
func parallel(n int, fn func(int) error) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		next  = make(chan int)
	)
	for range min(runtime.NumCPU(), max(n, 1)) {
		wg.Go(func() {
			for i := range next {
				if err := fn(i); err != nil {
					mu.Lock()
					first = errors.Join(first, err)
					mu.Unlock()
				}
			}
		})
	}
	for i := range n {
		next <- i
	}
	close(next)
	wg.Wait()
	return first
}
