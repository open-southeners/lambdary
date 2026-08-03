package backend

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ParseEnvFile reads a dotenv-lite file at path and returns its KEY=VALUE
// pairs, for local.env_file (see internal/manifest.Local.EnvFile). The
// format is intentionally minimal — no shell interpolation, no escape
// sequences — so it stays predictable rather than trying to match any one
// shell's own dotenv conventions:
//
//   - One "KEY=VALUE" pair per line; leading/trailing whitespace around the
//     whole line, and around VALUE, is trimmed.
//   - Blank lines are skipped.
//   - A line whose first non-whitespace character is "#" is a full-line
//     comment. There is no trailing-comment support: "KEY=VAL # note" keeps
//     "# note" as part of VAL, since detecting a "real" comment marker
//     inside an unquoted value would need quoting rules this parser
//     deliberately doesn't have.
//   - An optional "export " prefix before KEY is stripped, so a file also
//     meant to be `source`-d in a shell still parses.
//   - VALUE may be wrapped in a single matching pair of single or double
//     quotes, which are stripped verbatim; no escape sequences inside the
//     quotes are interpreted (e.g. `\n` stays the two characters `\` and
//     `n`) and no `${VAR}`-style interpolation is performed anywhere in the
//     file.
//
// A line with no "=" or an empty KEY is malformed: ParseEnvFile returns an
// error naming path and the offending line number.
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("envfile: %s: %w", path, err)
	}
	defer f.Close()

	env := make(map[string]string)

	scanner := bufio.NewScanner(f)
	lineNo := 0

	for scanner.Scan() {
		lineNo++

		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("envfile: %s: line %d: missing \"=\"", path, lineNo)
		}

		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("envfile: %s: line %d: empty key", path, lineNo)
		}

		env[key] = unquoteEnvValue(strings.TrimSpace(value))
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("envfile: %s: %w", path, err)
	}

	return env, nil
}

// unquoteEnvValue strips one matching pair of surrounding single or double
// quotes from value, verbatim — no escape-sequence interpretation. A value
// with no surrounding quotes, or mismatched/partial quoting, is returned
// unchanged.
func unquoteEnvValue(value string) string {
	if len(value) < 2 {
		return value
	}

	first, last := value[0], value[len(value)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return value[1 : len(value)-1]
	}

	return value
}
