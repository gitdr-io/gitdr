package cli

import (
	"strings"
	"testing"
)

// A run says once, as it starts, which variables in its environment git no longer gets, by name
// and never by value: GIT_CONFIG_PARAMETERS can carry a credential.
func TestLoadNamesTheVariablesGitNoLongerGets(t *testing.T) {
	const secret = "Y2FuYXJ5LWluLWEtdmFsdWU"
	t.Setenv("GITDR_LOG_LEVEL", "info")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'http.extraheader'='Authorization: Basic "+secret+"'")

	var err error
	logs := captureStderr(t, func() { _, _, err = (&commonOpts{}).load() })
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs, withheldWarning); n != 1 {
		t.Fatalf("the warning is logged %d times, want once:\n%s", n, logs)
	}
	if !strings.Contains(logs, "GIT_CONFIG_PARAMETERS") {
		t.Errorf("the warning does not name GIT_CONFIG_PARAMETERS:\n%s", logs)
	}
	if strings.Contains(logs, secret) {
		t.Errorf("the warning printed a value:\n%s", logs)
	}
}
