package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"gitdr.io/gitdr/internal/dest"
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
		case st.Verdict.Immutable():
			add("worm", true, "immutable, "+st.Details)
		case cfg.WORM.Require:
			add("worm", false, "NOT immutable ("+st.Details+"); worm.require is set, backup would fail")
		// Unknown is not a quieter version of absent, so it does not borrow its words. What a
		// reader needs here is that gitdr could not see the answer and where to go instead.
		case st.Verdict == dest.VerdictUnknown:
			add("worm", true, "could not read immutability ("+st.Details+"); check with the provider, backup warns and proceeds")
		default:
			add("worm", true, "NOT immutable ("+st.Details+"), WORM recommended; backup warns and proceeds")
		}

		/*
		 * And what is actually on an object, which is a different question from what the bucket
		 * says about itself. A store can report Object Lock enabled, accept a write, and apply
		 * nothing.
		 *
		 * The newest object already under the prefix, never a canary: on a compliance-locked
		 * bucket a probe object is undeletable litter for the whole retention window, by
		 * construction. So this says nothing on an empty destination, which is honest - there is
		 * nothing there to look at yet.
		 *
		 * This is where the *confirming* direction belongs. A backup only ever lowers its verdict
		 * from this observation, because one retained object proves the store implements the
		 * headers and nothing about the rest; doctor is a deliberate diagnostic rather than a
		 * claim in a signed document, so here it may say what it saw.
		 */
		if observer, ok := dst.(dest.RetentionObserver); ok && st.Verdict.Immutable() {
			switch key, err := anyObject(ctx, dst); {
			case err != nil:
				add("retention", true, "could not list the destination to find an object to check: "+err.Error())
			case key == "":
				add("retention", true, "nothing written here yet, so there is no object to check")
			default:
				got, until, err := observer.ObserveRetention(ctx, key)
				switch got {
				case dest.RetentionPresent:
					add("retention", true, "an object here is held until "+until.Format(time.RFC3339))
				case dest.RetentionAbsent:
					// The bucket said it locks and the object holds nothing. This is the failure
					// the whole gate exists to prevent, and until now nothing could see it.
					add("retention", !cfg.WORM.Require,
						"this bucket reports object lock and the newest object here carries no retention")
				default:
					add("retention", true, "could not read the retention on an object here"+detail(err)+
						"; on S3 this needs s3:GetObjectRetention, which a create-only credential will not have")
				}
			}
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

// An object already under the configured prefix, or "" when the destination is empty.
//
// Any object answers the question, which is whether retention is landing on writes to this
// bucket at all. `dest.Object` carries no timestamp, and picking "the newest" would mean
// inferring one from the key format - an assumption this does not need to make.
//
// Deliberately not a write. Anything gitdr puts on a compliance-locked bucket to look at is
// undeletable for the whole retention window, and a diagnostic that leaves litter behind is one
// people stop running.
func anyObject(ctx context.Context, dst dest.Destination) (string, error) {
	objs, err := dst.List(ctx, "")
	if err != nil {
		return "", err
	}
	if len(objs) == 0 {
		return "", nil
	}
	return objs[len(objs)-1].Key, nil
}

// The store's own words, when there are any, in parentheses.
func detail(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}
