package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"gitdr.io/gitdr/internal/source"
)

type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// runDoctor runs read-only preflight checks: tooling, config, source auth, and the
// WORM lock. It writes nothing to the destination.
func runDoctor(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	common := registerCommon(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, log, err := common.load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}

	var checks []checkResult
	add := func(name string, ok bool, detail string) {
		checks = append(checks, checkResult{Name: name, OK: ok, Detail: detail})
	}

	if _, err := exec.LookPath("git"); err != nil {
		add("git", false, "not found on PATH")
	} else {
		add("git", true, "found")
	}
	if _, err := exec.LookPath("git-lfs"); err != nil {
		add("git-lfs", true, "not found (LFS objects will be skipped)") // optional, not a failure
	} else {
		add("git-lfs", true, "found")
	}

	if err := cfg.Validate(); err != nil {
		add("config", false, err.Error())
		return emitDoctor(common.output, checks)
	}
	add("config", true, "valid")

	if cfg.Encryption.Enabled {
		if _, err := resolveEncryptionKey(cfg); err != nil {
			add("encryption key", false, err.Error())
		} else {
			add("encryption key", true, "valid 32-byte key")
		}
	}

	if src, err := buildSource(cfg, log); err != nil {
		add("source", false, err.Error())
	} else if ga, ok := src.(source.GitAuther); ok {
		if _, err := ga.GitAuthHeader(ctx); err != nil {
			add("source auth", false, err.Error())
		} else {
			add("source auth", true, "installation token minted")
		}
	} else {
		add("source", true, "built")
	}

	if dst, err := buildDest(ctx, cfg, log); err != nil {
		add("destination", false, err.Error())
	} else {
		st, err := dst.VerifyWorm(ctx)
		switch {
		case err != nil:
			add("worm", !cfg.WORM.Require, "could not verify immutability: "+err.Error())
		case st.Locked:
			add("worm", true, "immutable, "+st.Details)
		case cfg.WORM.Require:
			add("worm", false, "NOT immutable ("+st.Details+"); worm.require is set, backup would fail")
		default:
			add("worm", true, "NOT immutable ("+st.Details+"), WORM recommended; backup warns and proceeds")
		}
	}

	return emitDoctor(common.output, checks)
}

func emitDoctor(output string, checks []checkResult) int {
	ok := true
	for _, c := range checks {
		if !c.OK {
			ok = false
		}
	}
	if output == "json" {
		b, _ := json.MarshalIndent(struct {
			OK     bool          `json:"ok"`
			Checks []checkResult `json:"checks"`
		}{ok, checks}, "", "  ")
		fmt.Println(string(b))
	} else {
		for _, c := range checks {
			status := "ok"
			if !c.OK {
				status = "FAIL"
			}
			fmt.Printf("[%s] %s, %s\n", status, c.Name, c.Detail)
		}
	}
	if !ok {
		return 1
	}
	return 0
}
