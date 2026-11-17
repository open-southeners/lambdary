package cli

import "testing"

// TestRootFlagDefault checks that LAMBDARY_ROOT sets --root's default, that
// the default is "." without it, and that an explicit --root wins.
func TestRootFlagDefault(t *testing.T) {
	tests := []struct {
		name string
		env  string
		args []string
		want string
	}{
		{"unset", "", nil, "."},
		{"env sets the default", "functions", nil, "functions"},
		{"flag overrides env", "functions", []string{"--root", "other"}, "other"},
		{"flag without env", "", []string{"--root", "other"}, "other"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvRoot, tc.env)

			cmd := newRootCmd()
			if err := cmd.PersistentFlags().Parse(tc.args); err != nil {
				t.Fatal(err)
			}

			if root != tc.want {
				t.Errorf("root = %q, want %q", root, tc.want)
			}
		})
	}
}
