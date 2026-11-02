// Package versions implements Lambda's published versions and aliases on
// local disk: immutable numbered snapshots of a function, and named pointers
// to them with optional weighted routing. All state lives under
// <.lambdary>/versions/<function>/ so it survives a dev server restart:
//
//	state.json    version metadata and aliases
//	<N>/code/     the frozen function code
//	<N>/layers/   the frozen, merged layer content (when it has layers)
//
// Validation mirrors AWS's own rules, so a local mistake fails the same way
// it would against the real service.
package versions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// Latest is the qualifier for a function's unpublished, hot-reloaded code.
const Latest = "$LATEST"

// Errors the Store reports, each mapping to one AWS error type. Match them
// with errors.Is; the wrapping message is written for the API caller.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrInvalid      = errors.New("invalid parameter")
	ErrPrecondition = errors.New("precondition failed")
)

// aliasNamePattern is AWS's alias name rule, minus its "not all digits"
// clause, which versionPattern covers.
var (
	aliasNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
	versionPattern   = regexp.MustCompile(`^[0-9]+$`)
)

// Version is one published, immutable snapshot of a function.
type Version struct {
	Version      string             `json:"version"`
	Description  string             `json:"description"`
	CodeSha256   string             `json:"code_sha256"`
	CodeSize     int64              `json:"code_size"`
	ConfigSha256 string             `json:"config_sha256"`
	RevisionID   string             `json:"revision_id"`
	LastModified time.Time          `json:"last_modified"`
	Runtime      string             `json:"runtime"`
	Handler      string             `json:"handler"`
	Backend      string             `json:"backend"`
	Image        string             `json:"image"`
	Manifest     *manifest.Manifest `json:"manifest"`
}

// Alias is a named pointer to a version, optionally sending a share of its
// invocations to one additional version.
type Alias struct {
	Name          string  `json:"name"`
	Version       string  `json:"version"`
	Description   string  `json:"description"`
	CanaryVersion string  `json:"canary_version,omitempty"`
	CanaryWeight  float64 `json:"canary_weight,omitempty"`
	RevisionID    string  `json:"revision_id"`
}

// AliasUpdate is an UpdateAlias request. A nil field keeps its current
// value; a non-nil, empty Weights removes the canary.
type AliasUpdate struct {
	Version     *string
	Description *string
	Weights     *map[string]float64
	RevisionID  string
}

// PublishInput is a PublishVersion request.
type PublishInput struct {
	Description string
	// CodeSha256 and RevisionID, when set, must match or the publish fails
	// with ErrPrecondition.
	CodeSha256 string
	RevisionID string
}

// BootstrapBuild compiles a Go function's bootstrap binary to out.
type BootstrapBuild func(ctx context.Context, fn discovery.Function, out string) error

// state is one function's state.json.
type state struct {
	// NextVersion is never decremented: AWS never reuses a version number.
	NextVersion int              `json:"next_version"`
	RevisionID  string           `json:"revision_id"`
	Versions    []Version        `json:"versions"`
	Aliases     map[string]Alias `json:"aliases"`
}

// Store reads and writes every function's versions and aliases. It is safe
// for concurrent use by one process. Each call re-reads state.json, which
// keeps the Store free of a cache to invalidate.
type Store struct {
	dir      string
	cacheDir string

	// publishMu serializes publishes, which build and copy for a long time;
	// mu guards state.json alone, so invokes keep flowing during a publish.
	publishMu sync.Mutex
	mu        sync.RWMutex

	// random returns a value in [0, 1) for weighted routing.
	random func() float64
	// now is the clock behind LastModified.
	now func() time.Time
}

// NewStore returns a Store over cacheDir, the project's .lambdary directory.
func NewStore(cacheDir string) *Store {
	return &Store{
		dir:      filepath.Join(cacheDir, "versions"),
		cacheDir: cacheDir,
		random:   mathrand.Float64,
		now:      time.Now,
	}
}

// List returns name's published versions, oldest first.
func (s *Store) List(name string) ([]Version, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st, err := s.load(name)
	if err != nil {
		return nil, err
	}

	return st.Versions, nil
}

// LatestRevision returns the RevisionId of name's $LATEST: a stable value
// that changes each time a version is published from it.
func (s *Store) LatestRevision(name string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st, err := s.load(name)
	if err != nil {
		return "", err
	}

	return st.RevisionID, nil
}

// CreateAlias creates an alias. weights may be nil.
func (s *Store) CreateAlias(name, alias, version, description string, weights map[string]float64) (Alias, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.load(name)
	if err != nil {
		return Alias{}, err
	}

	if !validAliasName(alias) {
		return Alias{}, fmt.Errorf("%w: alias name %q must be 1-128 letters, digits, - or _, and not all digits", ErrInvalid, alias)
	}

	if _, exists := st.Aliases[alias]; exists {
		return Alias{}, fmt.Errorf("%w: alias already exists: %s", ErrConflict, alias)
	}

	a := Alias{Name: alias, Description: description, RevisionID: newRevisionID()}

	if err := applyTarget(st, &a, version, weights); err != nil {
		return Alias{}, err
	}

	st.Aliases[alias] = a

	if err := s.save(name, st); err != nil {
		return Alias{}, err
	}

	return a, nil
}

// GetAlias returns an alias, or ErrNotFound.
func (s *Store) GetAlias(name, alias string) (Alias, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st, err := s.load(name)
	if err != nil {
		return Alias{}, err
	}

	a, ok := st.Aliases[alias]
	if !ok {
		return Alias{}, fmt.Errorf("%w: alias not found: %s", ErrNotFound, alias)
	}

	return a, nil
}

// UpdateAlias applies u to an existing alias.
func (s *Store) UpdateAlias(name, alias string, u AliasUpdate) (Alias, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.load(name)
	if err != nil {
		return Alias{}, err
	}

	a, ok := st.Aliases[alias]
	if !ok {
		return Alias{}, fmt.Errorf("%w: alias not found: %s", ErrNotFound, alias)
	}

	if u.RevisionID != "" && u.RevisionID != a.RevisionID {
		return Alias{}, fmt.Errorf("%w: alias revision id %s does not match the supplied one", ErrPrecondition, a.RevisionID)
	}

	version := a.Version
	if u.Version != nil {
		version = *u.Version
	}

	var weights map[string]float64
	switch {
	case u.Weights != nil:
		weights = *u.Weights
	case a.CanaryVersion != "":
		weights = map[string]float64{a.CanaryVersion: a.CanaryWeight}
	}

	if u.Description != nil {
		a.Description = *u.Description
	}

	if err := applyTarget(st, &a, version, weights); err != nil {
		return Alias{}, err
	}

	a.RevisionID = newRevisionID()
	st.Aliases[alias] = a

	if err := s.save(name, st); err != nil {
		return Alias{}, err
	}

	return a, nil
}

// Resolve turns a qualifier (a version number, an alias name, or Latest)
// into the version to run, drawing the weighted choice for a canary alias.
func (s *Store) Resolve(name, qualifier string) (string, error) {
	if qualifier == "" || qualifier == Latest {
		return Latest, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	st, err := s.load(name)
	if err != nil {
		return "", err
	}

	if versionPattern.MatchString(qualifier) {
		if !st.hasVersion(qualifier) {
			return "", fmt.Errorf("%w: version not found: %s", ErrNotFound, qualifier)
		}

		return qualifier, nil
	}

	a, ok := st.Aliases[qualifier]
	if !ok {
		return "", fmt.Errorf("%w: alias not found: %s", ErrNotFound, qualifier)
	}

	if a.CanaryVersion != "" && s.random() < a.CanaryWeight {
		return a.CanaryVersion, nil
	}

	return a.Version, nil
}

// Function returns the discovery.Function that runs a published version of
// name.
func (s *Store) Function(name, version string) (discovery.Function, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st, err := s.load(name)
	if err != nil {
		return discovery.Function{}, err
	}

	for _, v := range st.Versions {
		if v.Version == version {
			return discovery.Function{
				Name:     name,
				Dir:      filepath.Join(s.functionDir(name), version, "code"),
				Runtime:  v.Runtime,
				Handler:  v.Handler,
				Backend:  v.Backend,
				Image:    v.Image,
				Manifest: v.Manifest,
				Version:  version,
			}, nil
		}
	}

	return discovery.Function{}, fmt.Errorf("%w: version not found: %s", ErrNotFound, version)
}

// applyTarget validates version and weights against st and points a at them.
func applyTarget(st *state, a *Alias, version string, weights map[string]float64) error {
	if version == "" {
		return fmt.Errorf("%w: FunctionVersion is required", ErrInvalid)
	}

	if version != Latest && !versionPattern.MatchString(version) {
		return fmt.Errorf("%w: invalid function version %q", ErrInvalid, version)
	}

	if version != Latest && !st.hasVersion(version) {
		return fmt.Errorf("%w: function version not found: %s", ErrNotFound, version)
	}

	if len(weights) > 1 {
		return fmt.Errorf("%w: RoutingConfig supports only one additional version, got %d", ErrInvalid, len(weights))
	}

	a.Version = version
	a.CanaryVersion = ""
	a.CanaryWeight = 0

	for canary, weight := range weights {
		if version == Latest {
			return fmt.Errorf("%w: an alias on %s cannot have a RoutingConfig; its version must be published", ErrInvalid, Latest)
		}

		if !versionPattern.MatchString(canary) {
			return fmt.Errorf("%w: additional version %q must be a published version", ErrInvalid, canary)
		}

		if !st.hasVersion(canary) {
			return fmt.Errorf("%w: function version not found: %s", ErrNotFound, canary)
		}

		if canary == version {
			return fmt.Errorf("%w: additional version %s is already the alias's primary version", ErrInvalid, canary)
		}

		if weight < 0 || weight > 1 {
			return fmt.Errorf("%w: additional version weight %v must be between 0 and 1", ErrInvalid, weight)
		}

		a.CanaryVersion = canary
		a.CanaryWeight = weight
	}

	return nil
}

func validAliasName(name string) bool {
	return aliasNamePattern.MatchString(name) && !versionPattern.MatchString(name)
}

func (st *state) hasVersion(version string) bool {
	for _, v := range st.Versions {
		if v.Version == version {
			return true
		}
	}

	return false
}

func (s *Store) functionDir(name string) string {
	return filepath.Join(s.dir, name)
}

// load reads name's state; a function that never published has an empty one.
// Callers hold mu.
func (s *Store) load(name string) (*state, error) {
	st := &state{NextVersion: 1, Aliases: map[string]Alias{}}

	data, err := os.ReadFile(filepath.Join(s.functionDir(name), "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		// Derived, not random: nothing is saved yet, and two reads of a
		// never-published function must report the same revision.
		st.RevisionID = initialRevisionID(name)
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("versions: %s: %w", name, err)
	}

	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("versions: %s: corrupt state.json: %w", name, err)
	}

	if st.Aliases == nil {
		st.Aliases = map[string]Alias{}
	}

	return st, nil
}

// save writes name's state atomically, so a crash never leaves a torn
// state.json. Callers hold mu for writing.
func (s *Store) save(name string, st *state) error {
	dir := s.functionDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("versions: %s: %w", name, err)
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("versions: %s: %w", name, err)
	}

	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return fmt.Errorf("versions: %s: %w", name, err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after a successful rename.

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // already failing.
		return fmt.Errorf("versions: %s: %w", name, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("versions: %s: %w", name, err)
	}

	if err := os.Rename(tmp.Name(), filepath.Join(dir, "state.json")); err != nil {
		return fmt.Errorf("versions: %s: %w", name, err)
	}

	return nil
}

// newRevisionID returns a random UUID-shaped id, as AWS's RevisionId is.
func newRevisionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) //nolint:errcheck // crypto/rand.Read never fails on supported platforms.

	return formatRevisionID(b[:])
}

// initialRevisionID is the revision of a function that has never published.
func initialRevisionID(name string) string {
	sum := sha256.Sum256([]byte(name))

	return formatRevisionID(sum[:16])
}

func formatRevisionID(b []byte) string {
	h := hex.EncodeToString(b)

	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// nextVersion returns st's next version number as a string.
func (st *state) nextVersion() string {
	return strconv.Itoa(st.NextVersion)
}
