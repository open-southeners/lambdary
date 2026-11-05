package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SQS's AWS JSON 1.0 protocol, which current AWS SDKs and the AWS CLI speak:
// every call is a POST to "/" naming its operation in X-Amz-Target, with a
// JSON body. The older query protocol is not served.
const (
	targetHeader = "X-Amz-Target"
	targetPrefix = "AmazonSQS."
	contentType  = "application/x-amz-json-1.0"
)

// maxBatch is the most entries one batch call accepts.
const maxBatch = 10

// API serves SQS calls against a Store.
type API struct {
	store *Store
}

// NewAPI returns an API over store.
func NewAPI(store *Store) *API { return &API{store: store} }

// Handles reports whether r is an SQS call.
func Handles(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get(targetHeader), targetPrefix)
}

// apiError is an SQS error: its code, the legacy query-protocol code SDKs
// with query compatibility read from x-amzn-query-error, and its message.
type apiError struct {
	status    int
	code      string
	queryCode string
	message   string
}

func (e *apiError) Error() string { return e.message }

func invalidParameter(format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, code: "InvalidParameterValue", queryCode: "InvalidParameterValue", message: fmt.Sprintf(format, args...)}
}

// toAPIError maps a Store error to its SQS error.
func toAPIError(err error) *apiError {
	var ae *apiError

	switch {
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, ErrNotFound):
		return &apiError{status: http.StatusBadRequest, code: "QueueDoesNotExist", queryCode: "AWS.SimpleQueueService.NonExistentQueue", message: "The specified queue does not exist."}
	case errors.Is(err, ErrInvalidReceipt):
		return &apiError{status: http.StatusBadRequest, code: "ReceiptHandleIsInvalid", queryCode: "ReceiptHandleIsInvalid", message: err.Error()}
	case errors.Is(err, ErrNotInflight):
		return &apiError{status: http.StatusBadRequest, code: "MessageNotInflight", queryCode: "AWS.SimpleQueueService.MessageNotInflight", message: err.Error()}
	case errors.Is(err, ErrInvalidParameter):
		return invalidParameter("%s", err.Error())
	default:
		return &apiError{status: http.StatusInternalServerError, code: "InternalError", queryCode: "InternalError", message: err.Error()}
	}
}

// ServeHTTP implements http.Handler.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.Header.Get(targetHeader), targetPrefix)

	body, err := io.ReadAll(io.LimitReader(r.Body, 2*MaxBodyBytes))
	if err != nil {
		writeError(w, invalidParameter("reading the request: %s", err))
		return
	}

	if len(body) == 0 {
		body = []byte("{}")
	}

	c := call{api: a, r: r, body: body}

	var resp any

	switch op {
	case "CreateQueue":
		resp, err = c.createQueue()
	case "GetQueueUrl":
		resp, err = c.getQueueURL()
	case "ListQueues":
		resp, err = c.listQueues()
	case "DeleteQueue":
		resp, err = c.withQueue(a.store.Delete)
	case "PurgeQueue":
		resp, err = c.withQueue(a.store.Purge)
	case "GetQueueAttributes":
		resp, err = c.getQueueAttributes()
	case "SetQueueAttributes":
		resp, err = c.setQueueAttributes()
	case "SendMessage":
		resp, err = c.sendMessage()
	case "SendMessageBatch":
		resp, err = c.sendMessageBatch()
	case "ReceiveMessage":
		resp, err = c.receiveMessage()
	case "DeleteMessage":
		resp, err = c.deleteMessage()
	case "DeleteMessageBatch":
		resp, err = c.deleteMessageBatch()
	case "ChangeMessageVisibility":
		resp, err = c.changeVisibility()
	case "ChangeMessageVisibilityBatch":
		resp, err = c.changeVisibilityBatch()
	default:
		err = &apiError{status: http.StatusBadRequest, code: "UnsupportedOperation", queryCode: "AWS.SimpleQueueService.UnsupportedOperation", message: fmt.Sprintf("%s is not supported by Lambdary's local SQS", op)}
	}

	if err != nil {
		writeError(w, toAPIError(err))
		return
	}

	w.Header().Set("Content-Type", contentType)
	json.NewEncoder(w).Encode(resp) //nolint:errcheck // best-effort; a client disconnect leaves nothing to do.
}

func writeError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("x-amzn-query-error", e.queryCode+";Sender")
	w.WriteHeader(e.status)
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck // best-effort.
		"__type":  "com.amazonaws.sqs#" + e.code,
		"message": e.message,
	})
}

// call is one request in flight.
type call struct {
	api  *API
	r    *http.Request
	body []byte
}

func (c call) decode(v any) error {
	if err := json.Unmarshal(c.body, v); err != nil {
		return invalidParameter("the request body is not valid: %s", err)
	}

	return nil
}

// queueURL builds the URL clients address name by: this server's own
// address, so it works from wherever the client reached it.
func (c call) queueURL(name string) string {
	scheme := "http"
	if c.r.TLS != nil {
		scheme = "https"
	}

	return scheme + "://" + c.r.Host + "/" + AccountID + "/" + name
}

// queueName takes the queue name from a queue URL. Any host is accepted, so
// a URL written for AWS (https://sqs.<region>.amazonaws.com/<account>/<name>)
// works as well.
func queueName(url string) (string, error) {
	if url == "" {
		return "", invalidParameter("QueueUrl is required")
	}

	name := url[strings.LastIndexByte(url, '/')+1:]
	if !ValidName(name) {
		return "", toAPIError(ErrNotFound)
	}

	return name, nil
}

type queueRef struct {
	QueueURL string `json:"QueueUrl"`
}

func (c call) withQueue(fn func(string) error) (any, error) {
	var req queueRef
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	name, err := queueName(req.QueueURL)
	if err != nil {
		return nil, err
	}

	return struct{}{}, fn(name)
}

func (c call) createQueue() (any, error) {
	var req struct {
		QueueName  string            `json:"QueueName"`
		Attributes map[string]string `json:"Attributes"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	settings, err := parseAttributes(Settings{}, req.Attributes)
	if err != nil {
		return nil, err
	}

	if err := c.api.store.Create(req.QueueName, settings); err != nil {
		return nil, err
	}

	return map[string]string{"QueueUrl": c.queueURL(req.QueueName)}, nil
}

func (c call) getQueueURL() (any, error) {
	var req struct {
		QueueName string `json:"QueueName"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	if !c.api.store.Exists(req.QueueName) {
		return nil, ErrNotFound
	}

	return map[string]string{"QueueUrl": c.queueURL(req.QueueName)}, nil
}

func (c call) listQueues() (any, error) {
	var req struct {
		QueueNamePrefix string `json:"QueueNamePrefix"`
		MaxResults      int    `json:"MaxResults"`
		NextToken       string `json:"NextToken"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	names := c.api.store.List(req.QueueNamePrefix)

	start := 0
	if req.NextToken != "" {
		n, err := strconv.Atoi(req.NextToken)
		if err != nil || n < 0 || n > len(names) {
			return nil, invalidParameter("NextToken is not valid")
		}
		start = n
	}

	resp := struct {
		QueueUrls []string `json:"QueueUrls,omitempty"`
		NextToken string   `json:"NextToken,omitempty"`
	}{}

	end := len(names)
	if req.MaxResults > 0 && start+req.MaxResults < end {
		end = start + req.MaxResults
		resp.NextToken = strconv.Itoa(end)
	}

	for _, name := range names[start:end] {
		resp.QueueUrls = append(resp.QueueUrls, c.queueURL(name))
	}

	return resp, nil
}

// redrivePolicy is the RedrivePolicy queue attribute. maxReceiveCount
// arrives as a string or a number, depending on the client.
type redrivePolicy struct {
	DeadLetterTargetArn string          `json:"deadLetterTargetArn"`
	MaxReceiveCount     json.RawMessage `json:"maxReceiveCount"`
}

// parseAttributes applies CreateQueue/SetQueueAttributes attributes to base.
func parseAttributes(base Settings, attrs map[string]string) (Settings, error) {
	s := base

	seconds := func(name, v string, dst *time.Duration) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return &apiError{status: http.StatusBadRequest, code: "InvalidAttributeValue", queryCode: "InvalidAttributeValue", message: fmt.Sprintf("%s must be a whole number of seconds, got %q", name, v)}
		}
		*dst = time.Duration(n) * time.Second
		return nil
	}

	for name, v := range attrs {
		var err error

		switch name {
		case "VisibilityTimeout":
			err = seconds(name, v, &s.VisibilityTimeout)
		case "DelaySeconds":
			err = seconds(name, v, &s.Delay)
		case "MessageRetentionPeriod":
			err = seconds(name, v, &s.Retention)
		case "ReceiveMessageWaitTimeSeconds":
			err = seconds(name, v, &s.ReceiveWait)
		case "RedrivePolicy":
			if v == "" {
				s.DeadLetterQueue, s.MaxReceiveCount = "", 0
				continue
			}

			var p redrivePolicy
			if jsonErr := json.Unmarshal([]byte(v), &p); jsonErr != nil {
				return s, &apiError{status: http.StatusBadRequest, code: "InvalidAttributeValue", queryCode: "InvalidAttributeValue", message: "RedrivePolicy is not valid JSON"}
			}

			count, convErr := strconv.Atoi(strings.Trim(string(p.MaxReceiveCount), `"`))
			if convErr != nil {
				return s, &apiError{status: http.StatusBadRequest, code: "InvalidAttributeValue", queryCode: "InvalidAttributeValue", message: "RedrivePolicy maxReceiveCount must be a number"}
			}

			s.DeadLetterQueue = p.DeadLetterTargetArn[strings.LastIndexByte(p.DeadLetterTargetArn, ':')+1:]
			s.MaxReceiveCount = count
		case "FifoQueue", "ContentBasedDeduplication", "DeduplicationScope", "FifoThroughputLimit":
			if v == "true" || name != "FifoQueue" {
				return s, &apiError{status: http.StatusBadRequest, code: "InvalidAttributeName", queryCode: "InvalidAttributeName", message: "FIFO queues are not supported by Lambdary's local SQS"}
			}
		case "MaximumMessageSize", "Policy", "KmsMasterKeyId", "KmsDataKeyReusePeriodSeconds", "SqsManagedSseEnabled", "RedriveAllowPolicy":
			// Accepted so deploy-shaped calls work; nothing to enforce locally.
		default:
			return s, &apiError{status: http.StatusBadRequest, code: "InvalidAttributeName", queryCode: "InvalidAttributeName", message: fmt.Sprintf("unknown attribute %s", name)}
		}

		if err != nil {
			return s, err
		}
	}

	return s, nil
}

func (c call) getQueueAttributes() (any, error) {
	var req struct {
		QueueURL       string   `json:"QueueUrl"`
		AttributeNames []string `json:"AttributeNames"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	name, err := queueName(req.QueueURL)
	if err != nil {
		return nil, err
	}

	info, err := c.api.store.Info(name)
	if err != nil {
		return nil, err
	}

	all := map[string]string{
		"QueueArn":                              ARN(name),
		"ApproximateNumberOfMessages":           strconv.Itoa(info.Visible),
		"ApproximateNumberOfMessagesNotVisible": strconv.Itoa(info.InFlight),
		"ApproximateNumberOfMessagesDelayed":    strconv.Itoa(info.Delayed),
		"CreatedTimestamp":                      strconv.FormatInt(info.Created.Unix(), 10),
		"LastModifiedTimestamp":                 strconv.FormatInt(info.Modified.Unix(), 10),
		"VisibilityTimeout":                     strconv.Itoa(int(info.Settings.VisibilityTimeout / time.Second)),
		"DelaySeconds":                          strconv.Itoa(int(info.Settings.Delay / time.Second)),
		"MessageRetentionPeriod":                strconv.Itoa(int(info.Settings.Retention / time.Second)),
		"ReceiveMessageWaitTimeSeconds":         strconv.Itoa(int(info.Settings.ReceiveWait / time.Second)),
		"MaximumMessageSize":                    strconv.Itoa(MaxBodyBytes),
		"SqsManagedSseEnabled":                  "false",
	}

	if info.Settings.DeadLetterQueue != "" {
		policy, _ := json.Marshal(map[string]any{ //nolint:errcheck // marshaling a map of a string and an int cannot fail.
			"deadLetterTargetArn": ARN(info.Settings.DeadLetterQueue),
			"maxReceiveCount":     info.Settings.MaxReceiveCount,
		})
		all["RedrivePolicy"] = string(policy)
	}

	out := map[string]string{}

	for _, want := range req.AttributeNames {
		if want == "All" {
			out = all
			break
		}

		if v, ok := all[want]; ok {
			out[want] = v
		}
	}

	return map[string]any{"Attributes": out}, nil
}

func (c call) setQueueAttributes() (any, error) {
	var req struct {
		QueueURL   string            `json:"QueueUrl"`
		Attributes map[string]string `json:"Attributes"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	name, err := queueName(req.QueueURL)
	if err != nil {
		return nil, err
	}

	info, err := c.api.store.Info(name)
	if err != nil {
		return nil, err
	}

	settings, err := parseAttributes(info.Settings, req.Attributes)
	if err != nil {
		return nil, err
	}

	return struct{}{}, c.api.store.SetSettings(name, settings)
}

// sendEntry is SendMessage's body, and one SendMessageBatch entry.
type sendEntry struct {
	ID                string               `json:"Id"`
	MessageBody       string               `json:"MessageBody"`
	DelaySeconds      *int                 `json:"DelaySeconds"`
	MessageAttributes map[string]Attribute `json:"MessageAttributes"`
}

func (e sendEntry) input() SendInput {
	in := SendInput{Body: e.MessageBody, Attributes: e.MessageAttributes}

	if e.DelaySeconds != nil {
		d := time.Duration(*e.DelaySeconds) * time.Second
		in.Delay = &d
	}

	return in
}

func (c call) sendMessage() (any, error) {
	var req struct {
		QueueURL string `json:"QueueUrl"`
		sendEntry
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	name, err := queueName(req.QueueURL)
	if err != nil {
		return nil, err
	}

	results, err := c.api.store.Send(name, req.input())
	if err != nil {
		return nil, err
	}

	return sendResult(results[0], ""), nil
}

func sendResult(r SendResult, id string) map[string]string {
	out := map[string]string{"MessageId": r.ID, "MD5OfMessageBody": r.MD5OfBody}
	if r.MD5OfMessageAttributes != "" {
		out["MD5OfMessageAttributes"] = r.MD5OfMessageAttributes
	}
	if id != "" {
		out["Id"] = id
	}

	return out
}

// batchFailure is one failed batch entry.
type batchFailure struct {
	ID          string `json:"Id"`
	SenderFault bool   `json:"SenderFault"`
	Code        string `json:"Code"`
	Message     string `json:"Message"`
}

func failure(id string, err error) batchFailure {
	ae := toAPIError(err)
	return batchFailure{ID: id, SenderFault: ae.status < 500, Code: ae.code, Message: ae.message}
}

// checkBatch applies SQS's rules for batch entry IDs.
func checkBatch(ids []string) error {
	if len(ids) == 0 {
		return &apiError{status: http.StatusBadRequest, code: "EmptyBatchRequest", queryCode: "AWS.SimpleQueueService.EmptyBatchRequest", message: "the batch request does not contain any entries"}
	}
	if len(ids) > maxBatch {
		return &apiError{status: http.StatusBadRequest, code: "TooManyEntriesInBatchRequest", queryCode: "AWS.SimpleQueueService.TooManyEntriesInBatchRequest", message: "a batch takes at most 10 entries"}
	}

	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || len(id) > 80 || strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
			return &apiError{status: http.StatusBadRequest, code: "InvalidBatchEntryId", queryCode: "AWS.SimpleQueueService.InvalidBatchEntryId", message: fmt.Sprintf("batch entry ID %q is not valid", id)}
		}
		if seen[id] {
			return &apiError{status: http.StatusBadRequest, code: "BatchEntryIdsNotDistinct", queryCode: "AWS.SimpleQueueService.BatchEntryIdsNotDistinct", message: fmt.Sprintf("batch entry ID %q is used twice", id)}
		}
		seen[id] = true
	}

	return nil
}

func (c call) sendMessageBatch() (any, error) {
	var req struct {
		QueueURL string      `json:"QueueUrl"`
		Entries  []sendEntry `json:"Entries"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	name, err := queueName(req.QueueURL)
	if err != nil {
		return nil, err
	}

	ids := make([]string, len(req.Entries))
	for i, e := range req.Entries {
		ids[i] = e.ID
	}

	if err := checkBatch(ids); err != nil {
		return nil, err
	}

	resp := batchResponse()

	for _, e := range req.Entries {
		results, err := c.api.store.Send(name, e.input())

		switch {
		case errors.Is(err, ErrNotFound):
			return nil, err
		case err != nil:
			resp.Failed = append(resp.Failed, failure(e.ID, err))
		default:
			resp.Successful = append(resp.Successful, sendResult(results[0], e.ID))
		}
	}

	return resp, nil
}

func (c call) receiveMessage() (any, error) {
	var req struct {
		QueueURL                    string   `json:"QueueUrl"`
		MaxNumberOfMessages         *int     `json:"MaxNumberOfMessages"`
		WaitTimeSeconds             *int     `json:"WaitTimeSeconds"`
		VisibilityTimeout           *int     `json:"VisibilityTimeout"`
		AttributeNames              []string `json:"AttributeNames"`
		MessageSystemAttributeNames []string `json:"MessageSystemAttributeNames"`
		MessageAttributeNames       []string `json:"MessageAttributeNames"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	name, err := queueName(req.QueueURL)
	if err != nil {
		return nil, err
	}

	in := ReceiveInput{Max: 1}

	if req.MaxNumberOfMessages != nil {
		in.Max = *req.MaxNumberOfMessages
	}
	if req.WaitTimeSeconds != nil {
		d := time.Duration(*req.WaitTimeSeconds) * time.Second
		in.Wait = &d
	}
	if req.VisibilityTimeout != nil {
		d := time.Duration(*req.VisibilityTimeout) * time.Second
		in.VisibilityTimeout = &d
	}

	msgs, err := c.api.store.Receive(c.r.Context(), name, in)
	if err != nil {
		return nil, err
	}

	systemNames := append(append([]string(nil), req.AttributeNames...), req.MessageSystemAttributeNames...)

	type wireMessage struct {
		MessageID              string               `json:"MessageId"`
		ReceiptHandle          string               `json:"ReceiptHandle"`
		MD5OfBody              string               `json:"MD5OfBody"`
		Body                   string               `json:"Body"`
		Attributes             map[string]string    `json:"Attributes,omitempty"`
		MD5OfMessageAttributes string               `json:"MD5OfMessageAttributes,omitempty"`
		MessageAttributes      map[string]Attribute `json:"MessageAttributes,omitempty"`
	}

	resp := struct {
		Messages []wireMessage `json:"Messages,omitempty"`
	}{}

	for _, m := range msgs {
		attrs := FilterAttributes(m.Attributes, req.MessageAttributeNames)

		resp.Messages = append(resp.Messages, wireMessage{
			MessageID:              m.ID,
			ReceiptHandle:          m.ReceiptHandle,
			MD5OfBody:              m.MD5OfBody,
			Body:                   m.Body,
			Attributes:             filterSystem(SystemAttributes(m), systemNames),
			MD5OfMessageAttributes: attributesMD5(attrs),
			MessageAttributes:      attrs,
		})
	}

	return resp, nil
}

// SystemAttributes returns m's message system attributes, as SQS names and
// formats them.
func SystemAttributes(m Message) map[string]string {
	return map[string]string{
		"SenderId":                         m.SenderID,
		"SentTimestamp":                    strconv.FormatInt(m.SentTimestamp.UnixMilli(), 10),
		"ApproximateReceiveCount":          strconv.Itoa(m.ReceiveCount),
		"ApproximateFirstReceiveTimestamp": strconv.FormatInt(m.FirstReceiveTimestamp.UnixMilli(), 10),
	}
}

func filterSystem(all map[string]string, names []string) map[string]string {
	out := map[string]string{}

	for _, n := range names {
		if n == "All" {
			return all
		}
		if v, ok := all[n]; ok {
			out[n] = v
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// FilterAttributes keeps the message attributes a receive asked for: "All"
// or ".*" for every one, "prefix.*" for those starting with prefix, or exact
// names.
func FilterAttributes(attrs map[string]Attribute, names []string) map[string]Attribute {
	if len(attrs) == 0 || len(names) == 0 {
		return nil
	}

	out := map[string]Attribute{}

	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, want := range names {
		for _, k := range keys {
			switch {
			case want == "All" || want == ".*":
				out[k] = attrs[k]
			case strings.HasSuffix(want, ".*") && strings.HasPrefix(k, strings.TrimSuffix(want, "*")):
				out[k] = attrs[k]
			case want == k:
				out[k] = attrs[k]
			}
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func (c call) deleteMessage() (any, error) {
	var req struct {
		QueueURL      string `json:"QueueUrl"`
		ReceiptHandle string `json:"ReceiptHandle"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	name, err := queueName(req.QueueURL)
	if err != nil {
		return nil, err
	}

	return struct{}{}, c.api.store.DeleteMessage(name, req.ReceiptHandle)
}

func (c call) deleteMessageBatch() (any, error) {
	var req struct {
		QueueURL string `json:"QueueUrl"`
		Entries  []struct {
			ID            string `json:"Id"`
			ReceiptHandle string `json:"ReceiptHandle"`
		} `json:"Entries"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	name, err := queueName(req.QueueURL)
	if err != nil {
		return nil, err
	}

	ids := make([]string, len(req.Entries))
	for i, e := range req.Entries {
		ids[i] = e.ID
	}

	if err := checkBatch(ids); err != nil {
		return nil, err
	}

	resp := batchResponse()

	for _, e := range req.Entries {
		err := c.api.store.DeleteMessage(name, e.ReceiptHandle)

		switch {
		case errors.Is(err, ErrNotFound):
			return nil, err
		case err != nil:
			resp.Failed = append(resp.Failed, failure(e.ID, err))
		default:
			resp.Successful = append(resp.Successful, map[string]string{"Id": e.ID})
		}
	}

	return resp, nil
}

type batchResult struct {
	Successful []map[string]string `json:"Successful"`
	Failed     []batchFailure      `json:"Failed"`
}

func batchResponse() batchResult {
	return batchResult{Successful: []map[string]string{}, Failed: []batchFailure{}}
}

func (c call) changeVisibility() (any, error) {
	var req struct {
		QueueURL          string `json:"QueueUrl"`
		ReceiptHandle     string `json:"ReceiptHandle"`
		VisibilityTimeout int    `json:"VisibilityTimeout"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	name, err := queueName(req.QueueURL)
	if err != nil {
		return nil, err
	}

	return struct{}{}, c.api.store.ChangeVisibility(name, req.ReceiptHandle, time.Duration(req.VisibilityTimeout)*time.Second)
}

func (c call) changeVisibilityBatch() (any, error) {
	var req struct {
		QueueURL string `json:"QueueUrl"`
		Entries  []struct {
			ID                string `json:"Id"`
			ReceiptHandle     string `json:"ReceiptHandle"`
			VisibilityTimeout int    `json:"VisibilityTimeout"`
		} `json:"Entries"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}

	name, err := queueName(req.QueueURL)
	if err != nil {
		return nil, err
	}

	ids := make([]string, len(req.Entries))
	for i, e := range req.Entries {
		ids[i] = e.ID
	}

	if err := checkBatch(ids); err != nil {
		return nil, err
	}

	resp := batchResponse()

	for _, e := range req.Entries {
		err := c.api.store.ChangeVisibility(name, e.ReceiptHandle, time.Duration(e.VisibilityTimeout)*time.Second)

		switch {
		case errors.Is(err, ErrNotFound):
			return nil, err
		case err != nil:
			resp.Failed = append(resp.Failed, failure(e.ID, err))
		default:
			resp.Successful = append(resp.Successful, map[string]string{"Id": e.ID})
		}
	}

	return resp, nil
}
