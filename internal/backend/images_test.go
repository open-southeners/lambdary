package backend

import "testing"

func TestImageFor(t *testing.T) {
	cases := []struct {
		runtime string
		want    string
	}{
		// Explicit table.
		{"nodejs22.x", "public.ecr.aws/lambda/nodejs:22"},
		{"python3.13", "public.ecr.aws/lambda/python:3.13"},
		{"ruby3.3", "public.ecr.aws/lambda/ruby:3.3"},
		{"dotnet8", "public.ecr.aws/lambda/dotnet:8"},
		{"provided.al2023", "public.ecr.aws/lambda/provided:al2023"},
		{"provided.al2", "public.ecr.aws/lambda/provided:al2"},
		// Generic family+version(.x) pattern, not in the explicit table.
		{"nodejs20.x", "public.ecr.aws/lambda/nodejs:20"},
		{"nodejs18.x", "public.ecr.aws/lambda/nodejs:18"},
		{"python3.12", "public.ecr.aws/lambda/python:3.12"},
		{"java21", "public.ecr.aws/lambda/java:21"},
		{"java17", "public.ecr.aws/lambda/java:17"},
	}

	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			got, err := ImageFor(tc.runtime)
			if err != nil {
				t.Fatalf("ImageFor(%q) unexpected error: %v", tc.runtime, err)
			}
			if got != tc.want {
				t.Errorf("ImageFor(%q) = %q, want %q", tc.runtime, got, tc.want)
			}
		})
	}

	unknown := []string{"", "notaruntime", "provided.unknownos", "nodejs", "22.x"}
	for _, runtime := range unknown {
		t.Run("unknown/"+runtime, func(t *testing.T) {
			_, err := ImageFor(runtime)
			if err == nil {
				t.Fatalf("ImageFor(%q) expected error, got nil", runtime)
			}
		})
	}
}
