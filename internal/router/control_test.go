package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
	"github.com/open-southeners/lambdary/internal/versions"
)

const ctlPrefix = "/2015-03-31/functions/"

// versionBackend is a fake backend keyed by function name plus version: each
// instance answers with its own key, so a test can tell which one ran.
type versionBackend struct {
	mu        sync.Mutex
	instances map[string]*fakeInstance
	body      func(key string) (status int, body []byte)
}

func newVersionBackend() *versionBackend {
	return &versionBackend{instances: map[string]*fakeInstance{}}
}

func instanceKey(fn discovery.Function) string {
	if fn.Version == "" {
		return fn.Name
	}

	return fn.Name + ":" + fn.Version
}

func (b *versionBackend) Start(_ context.Context, fn discovery.Function) (backend.Instance, error) {
	key := instanceKey(fn)

	status, body := http.StatusOK, []byte(key)
	if b.body != nil {
		status, body = b.body(key)
	}

	inst := newFakeInstance(status, body, "text/plain", 0)

	b.mu.Lock()
	b.instances[key] = inst
	b.mu.Unlock()

	return inst, nil
}

func (b *versionBackend) instance(key string) *fakeInstance {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.instances[key]
}

type ctlEnv struct {
	h     http.Handler
	store *versions.Store
	mgr   *Manager
	b     *versionBackend
	fn    discovery.Function
}

// newCtlEnv builds a router over a real store and a real function directory.
func newCtlEnv(t *testing.T) *ctlEnv {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "hello")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	setCode(t, dir, "one")

	f := discovery.Function{
		Name:     "hello",
		Dir:      dir,
		Route:    "/hello",
		Runtime:  "nodejs22.x",
		Handler:  "index.handler",
		Backend:  "process",
		Manifest: &manifest.Manifest{},
	}

	b := newVersionBackend()
	fns := []discovery.Function{f}
	mgr := NewManager(b, fns)
	store := versions.NewStore(t.TempDir())

	t.Cleanup(func() { _ = mgr.StopAll(context.Background()) })

	return &ctlEnv{h: NewWithVersions(mgr, fns, store), store: store, mgr: mgr, b: b, fn: f}
}

func setCode(t *testing.T, dir, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *ctlEnv) do(method, path, body string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	return rec
}

func (e *ctlEnv) publish(t *testing.T, code string) map[string]any {
	t.Helper()

	setCode(t, e.fn.Dir, code)

	rec := e.do(http.MethodPost, ctlPrefix+"hello/versions", `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish status = %d: %s", rec.Code, rec.Body.String())
	}

	var m map[string]any
	decodeJSON(t, rec, &m)

	return m
}

func (e *ctlEnv) createAlias(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()

	rec := e.do(http.MethodPost, ctlPrefix+"hello/aliases", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create alias status = %d: %s", rec.Code, rec.Body.String())
	}

	return rec
}

func assertAWSError(t *testing.T, rec *httptest.ResponseRecorder, status int, errType string) {
	t.Helper()

	if rec.Code != status {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, status, rec.Body.String())
	}
	if got := rec.Header().Get("x-amzn-ErrorType"); got != errType {
		t.Errorf("x-amzn-ErrorType = %q, want %q", got, errType)
	}

	var body map[string]string
	decodeJSON(t, rec, &body)

	if body["Type"] != "User" || body["Message"] == "" {
		t.Errorf("body = %v, want Type=User and a Message", body)
	}
}

func TestControlPublishAndGet(t *testing.T) {
	e := newCtlEnv(t)

	setCode(t, e.fn.Dir, "one")
	rec := e.do(http.MethodPost, ctlPrefix+"hello/versions", `{"Description":"first"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var v struct {
		FunctionName string
		FunctionArn  string
		Version      string
		Description  string
		CodeSha256   string
		CodeSize     int64
		RevisionId   string
		LastModified string
		Runtime      string
		Handler      string
		State        string
	}
	decodeJSON(t, rec, &v)

	if v.FunctionName != "hello" || v.Version != "1" || v.Description != "first" {
		t.Errorf("response = %+v", v)
	}
	if v.FunctionArn != "arn:aws:lambda:us-east-1:000000000000:function:hello:1" {
		t.Errorf("FunctionArn = %q", v.FunctionArn)
	}
	if v.CodeSha256 == "" || v.RevisionId == "" || v.CodeSize != 3 {
		t.Errorf("CodeSha256/RevisionId/CodeSize = %q/%q/%d", v.CodeSha256, v.RevisionId, v.CodeSize)
	}
	if v.Runtime != "nodejs22.x" || v.Handler != "index.handler" {
		t.Errorf("Runtime/Handler = %q/%q", v.Runtime, v.Handler)
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000-0700", v.LastModified); err != nil {
		t.Errorf("LastModified %q: %v", v.LastModified, err)
	}

	// An unchanged publish returns the same version (201 is the existing contract).
	rec = e.do(http.MethodPost, ctlPrefix+"hello/versions", ``)
	var again struct{ Version string }
	decodeJSON(t, rec, &again)
	if again.Version != "1" {
		t.Errorf("unchanged publish version = %q, want 1", again.Version)
	}
}

func TestControlListVersions(t *testing.T) {
	e := newCtlEnv(t)
	e.publish(t, "a")
	e.publish(t, "b")

	rec := e.do(http.MethodGet, ctlPrefix+"hello/versions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var list struct {
		Versions []struct {
			Version      string
			LastModified string
			FunctionArn  string
		}
		NextMarker *string
	}
	decodeJSON(t, rec, &list)

	var got []string
	for _, v := range list.Versions {
		got = append(got, v.Version)

		if _, err := time.Parse("2006-01-02T15:04:05.000-0700", v.LastModified); err != nil {
			t.Errorf("version %s LastModified %q: %v", v.Version, v.LastModified, err)
		}
	}

	if want := []string{"$LATEST", "1", "2"}; !equalStrings(got, want) {
		t.Errorf("versions = %v, want %v", got, want)
	}
	if list.NextMarker != nil {
		t.Errorf("NextMarker = %q, want absent", *list.NextMarker)
	}
	if strings.Contains(rec.Body.String(), "NextMarker") {
		t.Error("NextMarker key present on the last page")
	}
}

func TestControlListPagination(t *testing.T) {
	e := newCtlEnv(t)
	for _, c := range []string{"a", "b", "c", "d"} {
		e.publish(t, c)
	}

	var seen []string
	marker := ""
	pages := 0

	for {
		path := ctlPrefix + "hello/versions?MaxItems=2"
		if marker != "" {
			path += "&Marker=" + url.QueryEscape(marker)
		}

		rec := e.do(http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}

		var page struct {
			Versions []struct{ Version string }
			Marker   string `json:"NextMarker"`
		}
		decodeJSON(t, rec, &page)

		if len(page.Versions) > 2 {
			t.Fatalf("page has %d items, MaxItems=2", len(page.Versions))
		}
		for _, v := range page.Versions {
			seen = append(seen, v.Version)
		}

		pages++
		if pages > 10 {
			t.Fatal("pagination does not terminate")
		}

		if page.Marker == "" {
			if strings.Contains(rec.Body.String(), "NextMarker") {
				t.Error("last page carries a NextMarker key")
			}
			break
		}

		marker = page.Marker
	}

	if want := []string{"$LATEST", "1", "2", "3", "4"}; !equalStrings(seen, want) {
		t.Errorf("walked %v, want %v", seen, want)
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}
}

func TestControlListInvalidParams(t *testing.T) {
	e := newCtlEnv(t)

	for _, q := range []string{"MaxItems=0", "MaxItems=-1", "MaxItems=abc", "MaxItems=10001", "Marker=abc", "Marker=-3"} {
		rec := e.do(http.MethodGet, ctlPrefix+"hello/versions?"+q, "")
		t.Run(q, func(t *testing.T) {
			assertAWSError(t, rec, http.StatusBadRequest, "InvalidParameterValueException")
		})
	}
}

func TestControlAliasLifecycle(t *testing.T) {
	e := newCtlEnv(t)
	e.publish(t, "a")
	e.publish(t, "b")

	rec := e.createAlias(t, `{"Name":"live","FunctionVersion":"1","Description":"d"}`)

	var a struct {
		AliasArn        string
		Name            string
		FunctionVersion string
		Description     string
		RevisionId      string
		RoutingConfig   *struct{ AdditionalVersionWeights map[string]float64 }
	}
	decodeJSON(t, rec, &a)

	if a.Name != "live" || a.FunctionVersion != "1" || a.Description != "d" || a.RevisionId == "" {
		t.Errorf("created = %+v", a)
	}
	if a.AliasArn != "arn:aws:lambda:us-east-1:000000000000:function:hello:live" {
		t.Errorf("AliasArn = %q", a.AliasArn)
	}
	if a.RoutingConfig != nil {
		t.Errorf("RoutingConfig = %+v, want absent", a.RoutingConfig)
	}

	rec = e.do(http.MethodGet, ctlPrefix+"hello/aliases/live", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", rec.Code, rec.Body.String())
	}

	rec = e.do(http.MethodPut, ctlPrefix+"hello/aliases/live", `{"FunctionVersion":"2","RoutingConfig":{"AdditionalVersionWeights":{"1":0.1}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", rec.Code, rec.Body.String())
	}

	var updated struct {
		FunctionVersion string
		Description     string
		RevisionId      string
		RoutingConfig   struct{ AdditionalVersionWeights map[string]float64 }
	}
	decodeJSON(t, rec, &updated)

	if updated.FunctionVersion != "2" || updated.Description != "d" || updated.RevisionId == a.RevisionId {
		t.Errorf("updated = %+v", updated)
	}
	if updated.RoutingConfig.AdditionalVersionWeights["1"] != 0.1 {
		t.Errorf("RoutingConfig = %+v", updated.RoutingConfig)
	}
}

func TestControlErrors(t *testing.T) {
	e := newCtlEnv(t)
	e.publish(t, "a")
	e.publish(t, "b")
	e.createAlias(t, `{"Name":"live","FunctionVersion":"1"}`)

	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		status  int
		errType string
	}{
		{"unknown function publish", http.MethodPost, "nope/versions", `{}`, 404, "ResourceNotFoundException"},
		{"unknown function list", http.MethodGet, "nope/versions", ``, 404, "ResourceNotFoundException"},
		{"unknown function alias", http.MethodPost, "nope/aliases", `{"Name":"x","FunctionVersion":"1"}`, 404, "ResourceNotFoundException"},
		{"unknown function get alias", http.MethodGet, "nope/aliases/x", ``, 404, "ResourceNotFoundException"},
		{"unknown alias get", http.MethodGet, "hello/aliases/zzz", ``, 404, "ResourceNotFoundException"},
		{"unknown alias update", http.MethodPut, "hello/aliases/zzz", `{}`, 404, "ResourceNotFoundException"},
		{"alias to unknown version", http.MethodPost, "hello/aliases", `{"Name":"x","FunctionVersion":"9"}`, 404, "ResourceNotFoundException"},
		{"duplicate alias", http.MethodPost, "hello/aliases", `{"Name":"live","FunctionVersion":"1"}`, 409, "ResourceConflictException"},
		{"bad alias name", http.MethodPost, "hello/aliases", `{"Name":"123","FunctionVersion":"1"}`, 400, "InvalidParameterValueException"},
		{"missing version", http.MethodPost, "hello/aliases", `{"Name":"x"}`, 400, "InvalidParameterValueException"},
		{"latest with weights", http.MethodPost, "hello/aliases", `{"Name":"x","FunctionVersion":"$LATEST","RoutingConfig":{"AdditionalVersionWeights":{"1":0.1}}}`, 400, "InvalidParameterValueException"},
		{"two weights", http.MethodPost, "hello/aliases", `{"Name":"x","FunctionVersion":"1","RoutingConfig":{"AdditionalVersionWeights":{"2":0.1,"3":0.1}}}`, 400, "InvalidParameterValueException"},
		{"canary is primary", http.MethodPut, "hello/aliases/live", `{"RoutingConfig":{"AdditionalVersionWeights":{"1":0.1}}}`, 400, "InvalidParameterValueException"},
		{"weight out of range", http.MethodPut, "hello/aliases/live", `{"RoutingConfig":{"AdditionalVersionWeights":{"2":1.5}}}`, 400, "InvalidParameterValueException"},
		{"alias revision mismatch", http.MethodPut, "hello/aliases/live", `{"RevisionId":"bogus"}`, 412, "PreconditionFailedException"},
		{"publish revision mismatch", http.MethodPost, "hello/versions", `{"RevisionId":"bogus"}`, 412, "PreconditionFailedException"},
		{"publish sha mismatch", http.MethodPost, "hello/versions", `{"CodeSha256":"bogus"}`, 412, "PreconditionFailedException"},
		{"malformed publish", http.MethodPost, "hello/versions", `{not json`, 400, "InvalidRequestContentException"},
		{"malformed create alias", http.MethodPost, "hello/aliases", `{"Name":`, 400, "InvalidRequestContentException"},
		{"malformed update alias", http.MethodPut, "hello/aliases/live", `[1,`, 400, "InvalidRequestContentException"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := e.do(tt.method, ctlPrefix+tt.path, tt.body)
			assertAWSError(t, rec, tt.status, tt.errType)
		})
	}
}

func TestControlAcceptsARNs(t *testing.T) {
	e := newCtlEnv(t)
	e.publish(t, "a")

	for _, ref := range []string{
		"hello",
		"arn:aws:lambda:us-east-1:123456789012:function:hello",
		"123456789012:function:hello",
	} {
		rec := e.do(http.MethodGet, ctlPrefix+ref+"/versions", "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d: %s", ref, rec.Code, rec.Body.String())
		}
	}
}

func TestControlRoutingConfigClearAndKeep(t *testing.T) {
	e := newCtlEnv(t)
	e.publish(t, "a")
	e.publish(t, "b")
	e.createAlias(t, `{"Name":"live","FunctionVersion":"1","RoutingConfig":{"AdditionalVersionWeights":{"2":0.3}}}`)

	getConfig := func() (hasRouting bool, weights map[string]float64) {
		rec := e.do(http.MethodGet, ctlPrefix+"hello/aliases/live", "")

		var m struct {
			RoutingConfig *struct{ AdditionalVersionWeights map[string]float64 }
		}
		decodeJSON(t, rec, &m)

		if m.RoutingConfig == nil {
			return false, nil
		}

		return true, m.RoutingConfig.AdditionalVersionWeights
	}

	// A PUT with no RoutingConfig keeps the canary.
	rec := e.do(http.MethodPut, ctlPrefix+"hello/aliases/live", `{"Description":"changed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if has, w := getConfig(); !has || w["2"] != 0.3 {
		t.Errorf("canary lost after unrelated PUT: %v %v", has, w)
	}

	// An empty weights map clears it.
	rec = e.do(http.MethodPut, ctlPrefix+"hello/aliases/live", `{"RoutingConfig":{"AdditionalVersionWeights":{}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "RoutingConfig") {
		t.Errorf("update response still has RoutingConfig: %s", rec.Body.String())
	}
	if has, _ := getConfig(); has {
		t.Error("GetAlias still has RoutingConfig after clearing")
	}

	// A null weights map inside RoutingConfig also clears.
	e.do(http.MethodPut, ctlPrefix+"hello/aliases/live", `{"RoutingConfig":{"AdditionalVersionWeights":{"2":0.5}}}`)
	e.do(http.MethodPut, ctlPrefix+"hello/aliases/live", `{"RoutingConfig":{}}`)
	if has, _ := getConfig(); has {
		t.Error("RoutingConfig:{} did not clear the canary")
	}
}

func TestInvokeQualifiers(t *testing.T) {
	e := newCtlEnv(t)
	e.publish(t, "a")
	e.publish(t, "b")
	e.createAlias(t, `{"Name":"live","FunctionVersion":"1"}`)
	e.createAlias(t, `{"Name":"dev","FunctionVersion":"$LATEST"}`)

	tests := []struct {
		name     string
		path     string
		wantBody string
		wantExec string
	}{
		{"unqualified", "hello/invocations", "hello", "$LATEST"},
		{"explicit latest", "hello/invocations?Qualifier=%24LATEST", "hello", "$LATEST"},
		{"query version", "hello/invocations?Qualifier=1", "hello:1", "1"},
		{"query version 2", "hello/invocations?Qualifier=2", "hello:2", "2"},
		{"query alias", "hello/invocations?Qualifier=live", "hello:1", "1"},
		{"path alias", "hello:live/invocations", "hello:1", "1"},
		{"path version", "hello:2/invocations", "hello:2", "2"},
		{"full arn alias", "arn:aws:lambda:us-east-1:123456789012:function:hello:live/invocations", "hello:1", "1"},
		{"partial arn alias", "123456789012:function:hello:live/invocations", "hello:1", "1"},
		{"agreeing qualifiers", "hello:live/invocations?Qualifier=live", "hello:1", "1"},
		{"alias on latest", "hello/invocations?Qualifier=dev", "hello", "$LATEST"},
		{"path alias on latest", "hello:dev/invocations", "hello", "$LATEST"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := e.do(http.MethodPost, ctlPrefix+tt.path, `{}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if rec.Body.String() != tt.wantBody {
				t.Errorf("ran %q, want %q", rec.Body.String(), tt.wantBody)
			}
			if got := rec.Header().Get("X-Amz-Executed-Version"); got != tt.wantExec {
				t.Errorf("X-Amz-Executed-Version = %q, want %q", got, tt.wantExec)
			}
			if got := rec.Header().Get("X-Amz-Function-Error"); got != "" {
				t.Errorf("X-Amz-Function-Error = %q, want empty", got)
			}
		})
	}
}

func TestInvokeVersionRunsFrozenCode(t *testing.T) {
	e := newCtlEnv(t)
	e.publish(t, "a")

	var seen []discovery.Function
	b := &recordingBackend{inner: e.b, seen: &seen}
	e.mgr.resolve = staticResolver(b)

	e.do(http.MethodPost, ctlPrefix+"hello/invocations?Qualifier=1", `{}`)
	e.do(http.MethodPost, ctlPrefix+"hello/invocations", `{}`)

	if len(seen) != 2 {
		t.Fatalf("Start called %d times, want 2", len(seen))
	}

	v1, latest := seen[0], seen[1]
	if v1.Version != "1" || v1.Name != "hello" || v1.InstanceName() != "hello@1" {
		t.Errorf("version fn = %+v", v1)
	}
	if !strings.Contains(v1.Dir, filepath.Join("versions", "hello", "1", "code")) {
		t.Errorf("version Dir = %q, want snapshot code dir", v1.Dir)
	}
	if latest.Version != "" || latest.Dir != e.fn.Dir {
		t.Errorf("$LATEST fn = %+v", latest)
	}
}

type recordingBackend struct {
	inner backend.Backend
	mu    sync.Mutex
	seen  *[]discovery.Function
}

func (r *recordingBackend) Start(ctx context.Context, fn discovery.Function) (backend.Instance, error) {
	r.mu.Lock()
	*r.seen = append(*r.seen, fn)
	r.mu.Unlock()

	return r.inner.Start(ctx, fn)
}

func TestInvokeQualifierErrors(t *testing.T) {
	e := newCtlEnv(t)
	e.publish(t, "a")
	e.createAlias(t, `{"Name":"live","FunctionVersion":"1"}`)

	rec := e.do(http.MethodPost, ctlPrefix+"hello:live/invocations?Qualifier=1", `{}`)
	assertAWSError(t, rec, http.StatusBadRequest, "InvalidParameterValueException")

	for _, q := range []string{"nope", "7"} {
		rec = e.do(http.MethodPost, ctlPrefix+"hello/invocations?Qualifier="+q, `{}`)
		assertAWSError(t, rec, http.StatusNotFound, "ResourceNotFoundException")
	}

	rec = e.do(http.MethodPost, ctlPrefix+"hello:nope/invocations", `{}`)
	assertAWSError(t, rec, http.StatusNotFound, "ResourceNotFoundException")
}

func TestInvokeUnknownFunctionKeepsBody(t *testing.T) {
	e := newCtlEnv(t)

	rec := e.do(http.MethodPost, ctlPrefix+"missing/invocations", `{}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("x-amzn-ErrorType"); got != "ResourceNotFoundException" {
		t.Errorf("x-amzn-ErrorType = %q", got)
	}

	var body struct {
		Message   string   `json:"message"`
		Functions []string `json:"functions"`
	}
	decodeJSON(t, rec, &body)

	if body.Message != "function not found: missing" || !equalStrings(body.Functions, []string{"hello"}) {
		t.Errorf("body = %+v", body)
	}
}

func TestInvokeFunctionErrorHeader(t *testing.T) {
	e := newCtlEnv(t)

	e.b.body = func(string) (int, []byte) {
		return http.StatusOK, []byte(`{"errorType":"x","errorMessage":"y"}`)
	}

	rec := e.do(http.MethodPost, ctlPrefix+"hello/invocations", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Amz-Function-Error"); got != "Unhandled" {
		t.Errorf("X-Amz-Function-Error = %q, want Unhandled", got)
	}
	if got := rec.Header().Get("X-Amz-Executed-Version"); got != "$LATEST" {
		t.Errorf("X-Amz-Executed-Version = %q", got)
	}

	// Same for a published version.
	e.publish(t, "a")
	rec = e.do(http.MethodPost, ctlPrefix+"hello/invocations?Qualifier=1", `{}`)
	if got := rec.Header().Get("X-Amz-Function-Error"); got != "Unhandled" {
		t.Errorf("version X-Amz-Function-Error = %q, want Unhandled", got)
	}

	// A normal JSON response has no flag.
	e2 := newCtlEnv(t)
	e2.b.body = func(string) (int, []byte) { return http.StatusOK, []byte(`{"ok":true,"errorType":"x"}`) }
	rec = e2.do(http.MethodPost, ctlPrefix+"hello/invocations", `{}`)
	if got := rec.Header().Get("X-Amz-Function-Error"); got != "" {
		t.Errorf("partial envelope flagged: %q", got)
	}
}

func TestRouterWithoutStore(t *testing.T) {
	b := newVersionBackend()
	fns := []discovery.Function{fn("hello")}
	h := New(NewManager(b, fns), fns)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		return rec
	}

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "hello/versions"},
		{http.MethodGet, "hello/versions"},
		{http.MethodPost, "hello/aliases"},
		{http.MethodGet, "hello/aliases/live"},
		{http.MethodPut, "hello/aliases/live"},
	} {
		rec := do(c.method, ctlPrefix+c.path, `{}`)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: status = %d, want 501", c.method, c.path, rec.Code)
		}
	}

	for _, path := range []string{"hello/invocations?Qualifier=1", "hello:live/invocations"} {
		rec := do(http.MethodPost, ctlPrefix+path, `{}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}

	// $LATEST still works.
	rec := do(http.MethodPost, ctlPrefix+"hello/invocations", `{}`)
	if rec.Code != http.StatusOK {
		t.Errorf("unqualified invoke status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Amz-Executed-Version"); got != "$LATEST" {
		t.Errorf("X-Amz-Executed-Version = %q", got)
	}
}

func TestHotReloadLeavesVersionsAlone(t *testing.T) {
	e := newCtlEnv(t)
	e.publish(t, "a")

	e.do(http.MethodPost, ctlPrefix+"hello/invocations?Qualifier=1", `{}`)
	e.do(http.MethodPost, ctlPrefix+"hello/invocations", `{}`)

	v1, latest := e.b.instance("hello:1"), e.b.instance("hello")
	if v1 == nil || latest == nil {
		t.Fatal("instances were not started")
	}

	if err := e.mgr.Restart(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}

	stopped := func(fi *fakeInstance) bool {
		fi.mu.Lock()
		defer fi.mu.Unlock()

		return fi.stopped
	}

	if !stopped(latest) {
		t.Error("$LATEST instance was not stopped by Restart")
	}
	if stopped(v1) {
		t.Error("Restart(name) stopped the version's instance")
	}

	// The version still answers on the same instance.
	rec := e.do(http.MethodPost, ctlPrefix+"hello/invocations?Qualifier=1", `{}`)
	if rec.Body.String() != "hello:1" || e.b.instance("hello:1") != v1 {
		t.Errorf("version instance replaced or broken: %q", rec.Body.String())
	}
}

func TestManagerRegister(t *testing.T) {
	b := newVersionBackend()
	m := NewManager(b, []discovery.Function{fn("hello")})

	v := fn("hello")
	v.Version = "3"

	key := m.Register(v)
	if key != "hello:3" {
		t.Fatalf("key = %q, want hello:3", key)
	}
	if m.Register(v) != key {
		t.Error("registering twice changed the key")
	}

	url1, err := m.Ensure(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	url2, _ := m.Ensure(context.Background(), key)
	if url1 != url2 {
		t.Error("second Ensure started another instance")
	}
	if _, err := m.Ensure(context.Background(), "hello:4"); err == nil {
		t.Error("unregistered version should not be startable")
	}

	_ = m.StopAll(context.Background())
}

func TestManagerBuildBootstrap(t *testing.T) {
	m := NewManager(newVersionBackend(), []discovery.Function{fn("hello")})

	// The fake backend is not a BootstrapBuilder.
	if err := m.BuildBootstrap(context.Background(), fn("hello"), filepath.Join(t.TempDir(), "bootstrap")); err == nil {
		t.Error("expected an error from a backend that cannot build")
	}
}
