package config

import (
	"fmt"
	"strings"
	"testing"

	"gitdr.io/gitdr/internal/redact"
)

func TestEnvOverrides(t *testing.T) {
	t.Setenv("GITDR_DESTINATION_S3_BUCKET", "env-bucket")
	t.Setenv("GITDR_DESTINATION_RETENTION_DAYS", "7")
	t.Setenv("GITDR_SOURCE_GITHUB_APPID", "42")
	t.Setenv("GITDR_DESTINATION_S3_USEPATHSTYLE", "true")
	t.Setenv("GITDR_GITHUB_APP_PRIVATE_KEY", "secret-pem")

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Destination.S3.Bucket != "env-bucket" {
		t.Errorf("bucket = %q", c.Destination.S3.Bucket)
	}
	if c.Destination.Retention.Days != 7 {
		t.Errorf("days = %d", c.Destination.Retention.Days)
	}
	if c.Source.GitHub.AppID != 42 {
		t.Errorf("appID = %d", c.Source.GitHub.AppID)
	}
	if !c.Destination.S3.UsePathStyle {
		t.Error("usePathStyle should be true")
	}
	if c.Source.GitHub.PrivateKey.Reveal() != "secret-pem" {
		t.Error("private key not injected from env")
	}
}

func TestDefaultsRetained(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Destination.Retention.Mode != "COMPLIANCE" || c.Destination.Retention.Days != 30 {
		t.Errorf("defaults lost: %+v", c.Destination.Retention)
	}
	if c.WORM.Require {
		t.Error("worm.require must default false")
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	c.Destination.S3.Bucket = "b"
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	c.Destination.Retention.Mode = "BOGUS"
	if err := c.Validate(); err == nil {
		t.Error("bad retention mode accepted")
	}
	c2 := Default() // missing bucket
	if err := c2.Validate(); err == nil {
		t.Error("missing bucket accepted")
	}
}

func TestSecretNeverFormatted(t *testing.T) {
	c := Default()
	c.Source.GitHub.PrivateKey = redact.Secret("super-secret-key")
	if s := fmt.Sprintf("%v %+v", c.Source.GitHub, c.Source); strings.Contains(s, "super-secret-key") {
		t.Fatalf("secret leaked in formatted output: %s", s)
	}
}
