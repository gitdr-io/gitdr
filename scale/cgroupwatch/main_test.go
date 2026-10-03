//go:build scale

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The watcher reads a cgroup v2 directory and /proc status files. A fake cgroup proves it reads
// what the kernel writes there; a real one is only inside a container (make scale-image).
func TestTheWatcherReadsACgroup(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"memory.current": "3221225472\n",
		"memory.peak":    "4026531840\n",
		"memory.max":     "4294967296\n",
		"memory.stat":    "anon 1073741824\nfile 2147483648\nfile_mapped 536870912\nshmem 0\nactive_file 1073741824\ninactive_file 1073741824\n",
		"memory.events":  "low 0\nhigh 0\nmax 12\noom 1\noom_kill 1\noom_group_kill 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := &watch{cgroup: dir, procs: map[string]*procPeak{}}
	w.sample()
	w.final()
	s := w.summary()
	for name, got := range map[string][2]int64{
		"limit":        {s.Limit, 4 << 30},
		"peak":         {s.Peak, 3840 << 20},
		"current":      {s.MaxCurrent, 3 << 30},
		"anon":         {s.MaxAnon, 1 << 30},
		"file":         {s.MaxFile, 2 << 30},
		"file mapped":  {s.MaxFileMapped, 512 << 20},
		"resident":     {s.MaxResident, 1536 << 20}, // anon + file_mapped
		"working set":  {s.MaxWorkingSet, 2 << 30},  // current - inactive_file
		"oom kills":    {s.OOMKill, 1},
		"oom (events)": {s.OOM, 1},
	} {
		if got[0] != got[1] {
			t.Errorf("%s = %d, want %d", name, got[0], got[1])
		}
	}
}

func TestStatusLinesAreReadInKilobytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status")
	body := "Name:\tgit\nVmHWM:\t  2097152 kB\nVmRSS:\t   1048576 kB\nRssAnon:\t  524288 kB\nRssFile:\t  524288 kB\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	kv := readKV(path)
	if kv["VmHWM"] != 2097152 || kv["RssAnon"] != 524288 || kv["RssFile"] != 524288 {
		t.Errorf("read %v", kv)
	}
}
