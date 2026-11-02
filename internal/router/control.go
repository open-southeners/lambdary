package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/versions"
)

// The Lambda control plane: the subset of AWS's REST API that publishes
// versions and manages aliases, so the AWS CLI and SDKs work unchanged with an
// endpoint override. Errors follow AWS's REST-JSON convention, which is what
// makes an SDK surface them as its typed exceptions.
const (
	errorTypeHeader = "x-amzn-ErrorType"

	errResourceNotFound = "ResourceNotFoundException"
	errConflict         = "ResourceConflictException"
	errInvalidParameter = "InvalidParameterValueException"
	errInvalidContent   = "InvalidRequestContentException"
	errPrecondition     = "PreconditionFailedException"
	errService          = "ServiceException"
)

// Fixed ARN parts: nothing local has a region or account, and SDKs only need
// the ARN to be well formed.
const (
	arnPrefix = "arn:aws:lambda:us-east-1:000000000000:function:"
	localRole = "arn:aws:iam::000000000000:role/lambdary"
)

// lastModifiedFormat is the timestamp layout AWS's API returns.
const lastModifiedFormat = "2006-01-02T15:04:05.000-0700"

// defaultListPage and maxListPage bound ListVersionsByFunction's MaxItems, as
// on AWS.
const (
	defaultListPage = 50
	maxListPage     = 10000
)

// functionConfiguration is AWS's FunctionConfiguration response shape.
type functionConfiguration struct {
	FunctionName     string       `json:"FunctionName"`
	FunctionArn      string       `json:"FunctionArn"`
	Runtime          string       `json:"Runtime,omitempty"`
	Role             string       `json:"Role"`
	Handler          string       `json:"Handler,omitempty"`
	CodeSize         int64        `json:"CodeSize"`
	Description      string       `json:"Description"`
	Timeout          int          `json:"Timeout"`
	MemorySize       int          `json:"MemorySize"`
	LastModified     string       `json:"LastModified"`
	CodeSha256       string       `json:"CodeSha256,omitempty"`
	Version          string       `json:"Version"`
	Environment      *environment `json:"Environment,omitempty"`
	RevisionID       string       `json:"RevisionId"`
	State            string       `json:"State"`
	LastUpdateStatus string       `json:"LastUpdateStatus"`
	PackageType      string       `json:"PackageType"`
	Architectures    []string     `json:"Architectures"`
}

type environment struct {
	Variables map[string]string `json:"Variables"`
}

// aliasConfiguration is AWS's AliasConfiguration response shape.
type aliasConfiguration struct {
	AliasArn        string         `json:"AliasArn"`
	Name            string         `json:"Name"`
	FunctionVersion string         `json:"FunctionVersion"`
	Description     string         `json:"Description"`
	RoutingConfig   *routingConfig `json:"RoutingConfig,omitempty"`
	RevisionID      string         `json:"RevisionId"`
}

type routingConfig struct {
	AdditionalVersionWeights map[string]float64 `json:"AdditionalVersionWeights"`
}

// aliasRequest is the body of CreateAlias and UpdateAlias. Pointers tell an
// absent field, which UpdateAlias keeps, from an empty one.
type aliasRequest struct {
	Name            string         `json:"Name"`
	FunctionVersion *string        `json:"FunctionVersion"`
	Description     *string        `json:"Description"`
	RoutingConfig   *routingConfig `json:"RoutingConfig"`
	RevisionID      string         `json:"RevisionId"`
}

// handleControl serves the control-plane routes, reporting false for any
// other request so ServeHTTP keeps looking.
func (rt *Router) handleControl(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, invokePrefix) {
		return false
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, invokePrefix), "/")

	var handler func(http.ResponseWriter, *http.Request, discovery.Function)

	switch {
	case len(parts) == 2 && parts[1] == "versions" && r.Method == http.MethodPost:
		handler = rt.publishVersion
	case len(parts) == 2 && parts[1] == "versions" && r.Method == http.MethodGet:
		handler = rt.listVersions
	case len(parts) == 2 && parts[1] == "aliases" && r.Method == http.MethodPost:
		handler = rt.createAlias
	case len(parts) == 3 && parts[1] == "aliases" && r.Method == http.MethodGet:
		handler = func(w http.ResponseWriter, _ *http.Request, fn discovery.Function) {
			rt.getAlias(w, fn, parts[2])
		}
	case len(parts) == 3 && parts[1] == "aliases" && r.Method == http.MethodPut:
		handler = func(w http.ResponseWriter, r *http.Request, fn discovery.Function) {
			rt.updateAlias(w, r, fn, parts[2])
		}
	default:
		return false
	}

	if rt.versions == nil {
		writeAWSError(w, http.StatusNotImplemented, errService, "this server has no version store")
		return true
	}

	name, _ := parseFunctionRef(parts[0])

	fns := rt.byName[name]

	switch len(fns) {
	case 0:
		writeAWSError(w, http.StatusNotFound, errResourceNotFound, "Function not found: "+arnPrefix+name)
	case 1:
		handler(w, r, fns[0])
	default:
		writeCollision(w, "function name", name, fns)
	}

	return true
}

func (rt *Router) publishVersion(w http.ResponseWriter, r *http.Request, fn discovery.Function) {
	var req struct {
		Description string `json:"Description"`
		CodeSha256  string `json:"CodeSha256"`
		RevisionID  string `json:"RevisionId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	v, err := rt.versions.Publish(r.Context(), fn, versions.PublishInput{
		Description: req.Description,
		CodeSha256:  req.CodeSha256,
		RevisionID:  req.RevisionID,
	}, rt.mgr.BuildBootstrap)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, versionConfiguration(fn.Name, v))
}

func (rt *Router) listVersions(w http.ResponseWriter, r *http.Request, fn discovery.Function) {
	start, limit, ok := pageParams(w, r)
	if !ok {
		return
	}

	published, err := rt.versions.List(fn.Name)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	latest, err := rt.latestConfiguration(fn)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	all := make([]functionConfiguration, 0, len(published)+1)
	all = append(all, latest)

	for _, v := range published {
		all = append(all, versionConfiguration(fn.Name, v))
	}

	resp := struct {
		Versions   []functionConfiguration `json:"Versions"`
		NextMarker string                  `json:"NextMarker,omitempty"`
	}{Versions: []functionConfiguration{}}

	if start < len(all) {
		end := min(start+limit, len(all))
		resp.Versions = all[start:end]

		if end < len(all) {
			resp.NextMarker = strconv.Itoa(end)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (rt *Router) createAlias(w http.ResponseWriter, r *http.Request, fn discovery.Function) {
	var req aliasRequest
	if !decodeBody(w, r, &req) {
		return
	}

	var version, description string
	if req.FunctionVersion != nil {
		version = *req.FunctionVersion
	}
	if req.Description != nil {
		description = *req.Description
	}

	var weights map[string]float64
	if req.RoutingConfig != nil {
		weights = req.RoutingConfig.AdditionalVersionWeights
	}

	a, err := rt.versions.CreateAlias(fn.Name, req.Name, version, description, weights)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, aliasResponse(fn.Name, a))
}

func (rt *Router) getAlias(w http.ResponseWriter, fn discovery.Function, alias string) {
	a, err := rt.versions.GetAlias(fn.Name, alias)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, aliasResponse(fn.Name, a))
}

func (rt *Router) updateAlias(w http.ResponseWriter, r *http.Request, fn discovery.Function, alias string) {
	var req aliasRequest
	if !decodeBody(w, r, &req) {
		return
	}

	update := versions.AliasUpdate{
		Version:     req.FunctionVersion,
		Description: req.Description,
		RevisionID:  req.RevisionID,
	}

	if req.RoutingConfig != nil {
		weights := req.RoutingConfig.AdditionalVersionWeights
		if weights == nil {
			weights = map[string]float64{}
		}

		update.Weights = &weights
	}

	a, err := rt.versions.UpdateAlias(fn.Name, alias, update)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, aliasResponse(fn.Name, a))
}

// resolveTarget picks what an invoke runs: the Manager key to send it to, and
// the version to report as executed. refQualifier comes from the function
// name and queryQualifier from ?Qualifier=; AWS accepts either, but rejects
// two that disagree.
func (rt *Router) resolveTarget(name, refQualifier, queryQualifier string) (target, executed string, err error) {
	qualifier := queryQualifier
	if refQualifier != "" {
		if queryQualifier != "" && queryQualifier != refQualifier {
			return "", "", fmt.Errorf("%w: the qualifier in the function name, %s, does not match Qualifier %s", versions.ErrInvalid, refQualifier, queryQualifier)
		}

		qualifier = refQualifier
	}

	if qualifier == "" || qualifier == versions.Latest {
		return name, versions.Latest, nil
	}

	if rt.versions == nil {
		return "", "", fmt.Errorf("%w: function not found: %s:%s", versions.ErrNotFound, name, qualifier)
	}

	version, err := rt.versions.Resolve(name, qualifier)
	if err != nil {
		return "", "", err
	}

	// An alias may point at $LATEST.
	if version == versions.Latest {
		return name, versions.Latest, nil
	}

	fn, err := rt.versions.Function(name, version)
	if err != nil {
		return "", "", err
	}

	return rt.mgr.Register(fn), version, nil
}

// parseFunctionRef splits what AWS accepts as a function name (a plain name,
// a full ARN or a partial ARN, each with an optional :qualifier) into the name
// and qualifier.
func parseFunctionRef(ref string) (name, qualifier string) {
	if _, after, ok := strings.Cut(ref, ":function:"); ok {
		ref = after
	}

	name, qualifier, _ = strings.Cut(ref, ":")

	return name, qualifier
}

// pageParams reads ListVersionsByFunction's paging parameters, answering 400
// itself when they are invalid. Marker is the index to resume from.
func pageParams(w http.ResponseWriter, r *http.Request) (start, limit int, ok bool) {
	limit = defaultListPage

	if raw := r.URL.Query().Get("MaxItems"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxListPage {
			writeAWSError(w, http.StatusBadRequest, errInvalidParameter, fmt.Sprintf("MaxItems must be between 1 and %d", maxListPage))
			return 0, 0, false
		}

		limit = n
	}

	if raw := r.URL.Query().Get("Marker"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeAWSError(w, http.StatusBadRequest, errInvalidParameter, "invalid Marker")
			return 0, 0, false
		}

		start = n
	}

	return start, limit, true
}

// decodeBody parses r's JSON body into v, answering 400 itself on failure. An
// empty body is valid: PublishVersion's fields are all optional.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeAWSError(w, http.StatusBadRequest, errInvalidContent, fmt.Sprintf("reading request body: %s", err))
		return false
	}

	if len(data) == 0 {
		return true
	}

	if err := json.Unmarshal(data, v); err != nil {
		writeAWSError(w, http.StatusBadRequest, errInvalidContent, fmt.Sprintf("could not parse request body: %s", err))
		return false
	}

	return true
}

// latestConfiguration describes fn's unpublished code. It leaves CodeSha256
// and CodeSize unset: computing them means a Go build or a full tree hash on
// every list, for values nothing reads on $LATEST.
func (rt *Router) latestConfiguration(fn discovery.Function) (functionConfiguration, error) {
	revision, err := rt.versions.LatestRevision(fn.Name)
	if err != nil {
		return functionConfiguration{}, err
	}

	modified := time.Now()
	if info, err := os.Stat(fn.Dir); err == nil {
		modified = info.ModTime()
	}

	cfg := baseConfiguration(fn.Name, fn.Runtime, fn.Handler, fn)
	cfg.FunctionArn = arnPrefix + fn.Name + ":" + versions.Latest
	cfg.Version = versions.Latest
	cfg.LastModified = modified.UTC().Format(lastModifiedFormat)
	cfg.RevisionID = revision

	return cfg, nil
}

func versionConfiguration(name string, v versions.Version) functionConfiguration {
	cfg := baseConfiguration(name, v.Runtime, v.Handler, discovery.Function{Manifest: v.Manifest})
	cfg.FunctionArn = arnPrefix + name + ":" + v.Version
	cfg.CodeSize = v.CodeSize
	cfg.Description = v.Description
	cfg.LastModified = v.LastModified.Format(lastModifiedFormat)
	cfg.CodeSha256 = v.CodeSha256
	cfg.Version = v.Version
	cfg.RevisionID = v.RevisionID

	return cfg
}

// baseConfiguration fills what $LATEST and a published version share.
func baseConfiguration(name, runtime, handler string, fn discovery.Function) functionConfiguration {
	cfg := functionConfiguration{
		FunctionName:     name,
		Runtime:          runtime,
		Role:             localRole,
		Handler:          handler,
		Timeout:          int(defaultInvokeTimeout / time.Second),
		MemorySize:       128,
		State:            "Active",
		LastUpdateStatus: "Successful",
		PackageType:      "Zip",
		Architectures:    []string{"x86_64"},
	}

	if m := fn.Manifest; m != nil {
		if m.Timeout > 0 {
			cfg.Timeout = m.Timeout
		}
		if m.Memory > 0 {
			cfg.MemorySize = m.Memory
		}
		if len(m.Architectures) > 0 {
			cfg.Architectures = m.Architectures
		}
		if len(m.Environment) > 0 {
			cfg.Environment = &environment{Variables: m.Environment}
		}
	}

	return cfg
}

func aliasResponse(name string, a versions.Alias) aliasConfiguration {
	resp := aliasConfiguration{
		AliasArn:        arnPrefix + name + ":" + a.Name,
		Name:            a.Name,
		FunctionVersion: a.Version,
		Description:     a.Description,
		RevisionID:      a.RevisionID,
	}

	if a.CanaryVersion != "" {
		resp.RoutingConfig = &routingConfig{
			AdditionalVersionWeights: map[string]float64{a.CanaryVersion: a.CanaryWeight},
		}
	}

	return resp
}

// writeStoreError reports an internal/versions error as its AWS error type.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, versions.ErrNotFound):
		writeAWSError(w, http.StatusNotFound, errResourceNotFound, err.Error())
	case errors.Is(err, versions.ErrConflict):
		writeAWSError(w, http.StatusConflict, errConflict, err.Error())
	case errors.Is(err, versions.ErrInvalid):
		writeAWSError(w, http.StatusBadRequest, errInvalidParameter, err.Error())
	case errors.Is(err, versions.ErrPrecondition):
		writeAWSError(w, http.StatusPreconditionFailed, errPrecondition, err.Error())
	default:
		writeAWSError(w, http.StatusInternalServerError, errService, err.Error())
	}
}

// writeAWSError writes an error in AWS's REST-JSON shape.
func writeAWSError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set(errorTypeHeader, errType)
	writeJSON(w, status, map[string]string{"Type": "User", "Message": message})
}
