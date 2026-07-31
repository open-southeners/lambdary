package cli

import (
	"testing"
)

// TestNewDevCmd verifies the dev command is properly configured.
func TestNewDevCmd(t *testing.T) {
	cmd := newDevCmd()
	if cmd.Use != "dev" {
		t.Errorf("expected Use 'dev', got %q", cmd.Use)
	}
	if cmd.RunE == nil {
		t.Errorf("expected RunE to be set")
	}
}

// TestValidateBackend checks --backend flag validation.
func TestValidateBackend(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		wantErr bool
	}{
		{"auto", "auto", false},
		{"container", "container", false},
		{"empty", "", false},
		{"process", "process", false},
		{"unknown", "unknown", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBackend(tt.backend)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateBackend(%q) error = %v, wantErr %v", tt.backend, err, tt.wantErr)
			}
		})
	}
}
