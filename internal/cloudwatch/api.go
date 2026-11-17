// Package cloudwatch serves the one CloudWatch call Lambdary supports,
// GetMetricData, over the figures an internal/metrics Recorder keeps, so an
// AWS SDK pointed at the dev server's endpoint reads a function's
// AWS/Lambda metrics the way it would from AWS.
//
// Current AWS SDKs speak CloudWatch over Smithy RPCv2 with CBOR bodies: every
// call is a POST to /service/GraniteServiceVersion20100801/operation/{name}
// carrying "Smithy-Protocol: rpc-v2-cbor". Only GetMetricData is served, and
// only the AWS/Lambda namespace with a FunctionName dimension plus an
// optional Resource one; anything else is answered with an
// InvalidParameterValueException, in RPCv2's error shape (a CBOR map with
// "__type" and "message").
package cloudwatch

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/smithy-go/encoding/cbor"

	"github.com/open-southeners/lambdary/internal/metrics"
)

const (
	servicePath    = "/service/GraniteServiceVersion20100801/operation/"
	operationGet   = "GetMetricData"
	protocolHeader = "Smithy-Protocol"
	protocolValue  = "rpc-v2-cbor"
	contentType    = "application/cbor"

	// namespace is the only CloudWatch namespace Lambdary has data for.
	namespace = "AWS/Lambda"

	// maxBody caps the request body, far above what any real query needs.
	maxBody = 8 << 20

	errInvalidParameter = "InvalidParameterValueException"
	errNotImplemented   = "ServiceUnavailableException"
)

// timestampTag is CBOR's tag for an epoch-seconds timestamp.
const timestampTag = 1

// API serves CloudWatch calls against a Recorder.
type API struct {
	recorder *metrics.Recorder
}

// NewAPI returns an API over recorder. A nil recorder is allowed: every call
// then answers 501, so a server that records nothing says so instead of
// returning empty data.
func NewAPI(recorder *metrics.Recorder) *API { return &API{recorder: recorder} }

// Handles reports whether r is an RPCv2 CloudWatch call.
func Handles(r *http.Request) bool {
	return r.Method == http.MethodPost &&
		strings.HasPrefix(r.URL.Path, servicePath) &&
		r.Header.Get(protocolHeader) == protocolValue
}

// ServeHTTP answers one CloudWatch call.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.recorder == nil {
		writeError(w, http.StatusNotImplemented, errNotImplemented, "this server does not record metrics")
		return
	}

	op := strings.TrimPrefix(r.URL.Path, servicePath)
	if op != operationGet {
		writeError(w, http.StatusBadRequest, errInvalidParameter, fmt.Sprintf("unsupported operation %q: only %s is served", op, operationGet))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, errInvalidParameter, fmt.Sprintf("reading request body: %s", err))
		return
	}

	out, err := a.getMetricData(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, errInvalidParameter, err.Error())
		return
	}

	writeCBOR(w, http.StatusOK, out)
}

// metricQuery is one decoded MetricDataQuery.
type metricQuery struct {
	id         string
	returnData bool
	metric     string
	function   string
	resource   string
	period     time.Duration
	stat       string
}

// getMetricData decodes a GetMetricData request and builds its response.
func (a *API) getMetricData(body []byte) (cbor.Value, error) {
	root, err := decodeMap(body)
	if err != nil {
		return nil, err
	}

	start, err := timeField(root, "StartTime")
	if err != nil {
		return nil, err
	}

	end, err := timeField(root, "EndTime")
	if err != nil {
		return nil, err
	}

	if !end.After(start) {
		return nil, fmt.Errorf("EndTime must be later than StartTime")
	}

	// CloudWatch lists newest datapoints first unless asked otherwise.
	ascending := false
	if v, ok := root["ScanBy"].(cbor.String); ok && string(v) == "TimestampAscending" {
		ascending = true
	}

	list, ok := root["MetricDataQueries"].(cbor.List)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("MetricDataQueries is required")
	}

	results := make(cbor.List, 0, len(list))

	for i, item := range list {
		q, err := parseQuery(item)
		if err != nil {
			return nil, fmt.Errorf("MetricDataQueries.%d: %w", i+1, err)
		}

		if !q.returnData {
			continue
		}

		points := a.recorder.Query(metrics.Query{
			Metric:   q.metric,
			Function: q.function,
			Resource: q.resource,
			Start:    start,
			End:      end,
			Period:   effectivePeriod(start, end, q.period),
			Stat:     q.stat,
		})

		timestamps := make(cbor.List, 0, len(points))
		values := make(cbor.List, 0, len(points))

		for j := range points {
			p := points[j]
			if !ascending {
				p = points[len(points)-1-j]
			}

			timestamps = append(timestamps, &cbor.Tag{ID: timestampTag, Value: cbor.Float64(float64(p.Timestamp.UnixMilli()) / 1000)})
			values = append(values, cbor.Float64(p.Value))
		}

		results = append(results, cbor.Map{
			"Id":         cbor.String(q.id),
			"Label":      cbor.String(q.metric),
			"StatusCode": cbor.String("Complete"),
			"Timestamps": timestamps,
			"Values":     values,
		})
	}

	return cbor.Map{
		"MetricDataResults": results,
		"Messages":          cbor.List{},
	}, nil
}

// effectivePeriod widens period to cover a query range that overshoots it by
// less than a minute. Start is rounded down to the minute, so a client asking
// for "the last hour as one datapoint" (period 3600s, StartTime now-1h) would
// otherwise get a second datapoint holding only the current minute; CloudWatch
// avoids that by aligning periods to its own clock. Widening keeps such a
// query a single datapoint.
func effectivePeriod(start, end time.Time, period time.Duration) time.Duration {
	span := end.Sub(start.UTC().Truncate(time.Minute))
	if span > period && span <= period+time.Minute {
		return span.Truncate(time.Minute) + time.Minute
	}

	return period
}

// parseQuery decodes and validates one MetricDataQuery.
func parseQuery(item cbor.Value) (metricQuery, error) {
	m, ok := item.(cbor.Map)
	if !ok {
		return metricQuery{}, fmt.Errorf("must be a structure")
	}

	q := metricQuery{returnData: true}

	id, ok := m["Id"].(cbor.String)
	if !ok || id == "" {
		return q, fmt.Errorf("Id is required")
	}
	q.id = string(id)

	if rd, ok := m["ReturnData"].(cbor.Bool); ok {
		q.returnData = bool(rd)
	}

	if _, ok := m["Expression"]; ok {
		return q, fmt.Errorf("Expression is not supported: use MetricStat")
	}

	stat, ok := m["MetricStat"].(cbor.Map)
	if !ok {
		return q, fmt.Errorf("MetricStat is required")
	}

	seconds, err := cbor.AsInt64(stat["Period"])
	if err != nil || seconds <= 0 {
		return q, fmt.Errorf("MetricStat.Period must be a positive number of seconds")
	}
	q.period = time.Duration(seconds) * time.Second

	statName, ok := stat["Stat"].(cbor.String)
	if !ok {
		return q, fmt.Errorf("MetricStat.Stat is required")
	}
	q.stat = string(statName)

	if _, err := metrics.ParseStat(q.stat); err != nil {
		return q, err
	}

	metric, ok := stat["Metric"].(cbor.Map)
	if !ok {
		return q, fmt.Errorf("MetricStat.Metric is required")
	}

	if ns, _ := metric["Namespace"].(cbor.String); string(ns) != namespace {
		return q, fmt.Errorf("unsupported namespace %q: only %s is served", string(ns), namespace)
	}

	name, ok := metric["MetricName"].(cbor.String)
	if !ok || name == "" {
		return q, fmt.Errorf("MetricStat.Metric.MetricName is required")
	}
	q.metric = string(name)

	dims, _ := metric["Dimensions"].(cbor.List)
	for _, d := range dims {
		dm, ok := d.(cbor.Map)
		if !ok {
			return q, fmt.Errorf("Dimensions must be structures")
		}

		dn, _ := dm["Name"].(cbor.String)
		dv, _ := dm["Value"].(cbor.String)

		switch string(dn) {
		case "FunctionName":
			q.function = string(dv)
		case "Resource":
			q.resource = string(dv)
		default:
			return q, fmt.Errorf("unsupported dimension %q: only FunctionName and Resource are served", string(dn))
		}
	}

	if q.function == "" {
		return q, fmt.Errorf("a FunctionName dimension is required")
	}

	return q, nil
}

// decodeMap decodes a CBOR request body that must be a top-level structure.
func decodeMap(body []byte) (cbor.Map, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("empty request body")
	}

	v, err := cbor.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("could not parse request body: %w", err)
	}

	m, ok := v.(cbor.Map)
	if !ok {
		return nil, fmt.Errorf("request body must be a structure")
	}

	return m, nil
}

// timeField reads a required epoch-seconds timestamp member of m.
func timeField(m cbor.Map, name string) (time.Time, error) {
	v, ok := m[name]
	if !ok {
		return time.Time{}, fmt.Errorf("%s is required", name)
	}

	t, err := cbor.AsTime(v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", name, err)
	}

	return t, nil
}

// writeError writes an RPCv2 error: the AWS SDK reads the exception type from
// the body's "__type" and its text from "message".
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeCBOR(w, status, cbor.Map{
		"__type":  cbor.String(code),
		"message": cbor.String(message),
	})
}

// writeCBOR writes v as an RPCv2 CBOR response.
func writeCBOR(w http.ResponseWriter, status int, v cbor.Value) {
	w.Header().Set(protocolHeader, protocolValue)
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(cbor.Encode(v)) //nolint:errcheck // best-effort; nothing to do about a write error after headers are sent.
}
