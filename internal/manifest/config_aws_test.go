package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func loadConfigYAML(t *testing.T, content string) (*Config, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "lambdary.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	return LoadConfig(path)
}

func TestConfigAWS(t *testing.T) {
	t.Run("off and default region when omitted", func(t *testing.T) {
		c, err := loadConfigYAML(t, "port: 9000\n")
		if err != nil {
			t.Fatalf("LoadConfig() unexpected error: %v", err)
		}

		if c.AWS.Environment {
			t.Error("AWS.Environment = true, want false by default")
		}

		if got := c.AWS.RegionOrDefault(); got != "us-east-1" {
			t.Errorf("RegionOrDefault() = %q, want us-east-1", got)
		}
	})

	t.Run("parses environment and region", func(t *testing.T) {
		c, err := loadConfigYAML(t, "aws:\n  environment: true\n  region: eu-south-2\n")
		if err != nil {
			t.Fatalf("LoadConfig() unexpected error: %v", err)
		}

		if !c.AWS.Environment || c.AWS.RegionOrDefault() != "eu-south-2" {
			t.Errorf("AWS = %+v, want environment on, region eu-south-2", c.AWS)
		}
	})

	t.Run("environment alone keeps the default region", func(t *testing.T) {
		c, err := loadConfigYAML(t, "aws:\n  environment: true\n")
		if err != nil {
			t.Fatalf("LoadConfig() unexpected error: %v", err)
		}

		if got := c.AWS.RegionOrDefault(); got != DefaultAWSRegion {
			t.Errorf("RegionOrDefault() = %q, want %q", got, DefaultAWSRegion)
		}
	})

	t.Run("rejects an invalid region", func(t *testing.T) {
		for _, region := range []string{"US East", "eu_south_2", "-eu", "eu--1", "'  '", "us-east-1!"} {
			_, err := loadConfigYAML(t, "aws:\n  region: "+region+"\n")
			if !errors.Is(err, ErrInvalidRegion) {
				t.Errorf("region %q: error = %v, want wrapping %v", region, err, ErrInvalidRegion)
			}
		}
	})

	t.Run("rejects unknown aws keys", func(t *testing.T) {
		if _, err := loadConfigYAML(t, "aws:\n  endpoint: x\n"); err == nil {
			t.Error("LoadConfig() expected an error for an unknown aws key")
		}
	})
}
