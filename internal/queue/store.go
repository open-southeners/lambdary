// Package queue is a small in-memory stand-in for Amazon SQS standard
// queues: enough of its semantics (visibility timeout, long polling, delays,
// a redrive policy to a dead-letter queue, retention) for producers to use the
// real AWS SDK against `lambdary dev`, and for SQS event source mappings to
// deliver batches to functions as AWS does. FIFO queues are not supported.
//
// Queues live in memory: they outlive a hot reload but not a restart.
package queue

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors returned by Store, matched by the API handler to SQS error codes.
var (
	ErrNotFound         = errors.New("the specified queue does not exist")
	ErrInvalidReceipt   = errors.New("the receipt handle is not valid")
	ErrNotInflight      = errors.New("the message is not in flight")
	ErrInvalidParameter = errors.New("invalid parameter")
)

// Defaults and limits, as on AWS.
const (
	DefaultVisibilityTimeout = 30 * time.Second
	DefaultRetention         = 4 * 24 * time.Hour
	MaxVisibilityTimeout     = 12 * time.Hour
	MaxReceiveBatch          = 10
	MaxWait                  = 20 * time.Second
	MaxDelay                 = 15 * time.Minute
	MaxBodyBytes             = 1024 * 1024
)

// Fixed ARN parts, matching the Lambda control plane's.
const (
	Region    = "us-east-1"
	AccountID = "000000000000"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// ValidName reports whether name is a valid standard queue name.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// ARN returns the queue's ARN.
func ARN(name string) string { return "arn:aws:sqs:" + Region + ":" + AccountID + ":" + name }

// Settings are a queue's configurable attributes.
type Settings struct {
	VisibilityTimeout time.Duration
	Delay             time.Duration
	Retention         time.Duration
	ReceiveWait       time.Duration
	// DeadLetterQueue and MaxReceiveCount form the redrive policy: a message
	// received more than MaxReceiveCount times moves to DeadLetterQueue
	// instead of being delivered again. Both are set or neither is.
	DeadLetterQueue string
	MaxReceiveCount int
}

// withDefaults fills unset durations.
func (s Settings) withDefaults() Settings {
	if s.VisibilityTimeout == 0 {
		s.VisibilityTimeout = DefaultVisibilityTimeout
	}
	if s.Retention == 0 {
		s.Retention = DefaultRetention
	}

	return s
}

// Attribute is a message attribute, as SQS models it.
type Attribute struct {
	DataType    string  `json:"DataType"`
	StringValue *string `json:"StringValue,omitempty"`
	BinaryValue []byte  `json:"BinaryValue,omitempty"`
}

// Message is a received message.
type Message struct {
	ID                     string
	ReceiptHandle          string
	Body                   string
	MD5OfBody              string
	MD5OfMessageAttributes string
	Attributes             map[string]Attribute
	SentTimestamp          time.Time
	FirstReceiveTimestamp  time.Time
	ReceiveCount           int
	SenderID               string
}

// SendInput is one message to send.
type SendInput struct {
	Body       string
	Attributes map[string]Attribute
	// Delay overrides the queue's delay when non-nil.
	Delay *time.Duration
}

// SendResult describes a sent message.
type SendResult struct {
	ID                     string
	MD5OfBody              string
	MD5OfMessageAttributes string
}

// QueueInfo is a queue's settings and counts, for GetQueueAttributes.
type QueueInfo struct {
	Name     string
	Settings Settings
	Created  time.Time
	Modified time.Time
	Visible  int
	InFlight int
	Delayed  int
}

type message struct {
	Message
	visibleAt time.Time
	inFlight  bool
}

type queue struct {
	name     string
	settings Settings
	created  time.Time
	modified time.Time
	msgs     []*message
	// wake is closed, and replaced, whenever a message may have become
	// receivable, so long polls can stop waiting.
	wake chan struct{}
}

func (q *queue) signal() {
	close(q.wake)
	q.wake = make(chan struct{})
}

// Store holds every queue. It is safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	queues map[string]*queue
	now    func() time.Time

	// stopped ends every long poll at once; see StopWaiting.
	stopped bool
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{queues: map[string]*queue{}, now: time.Now}
}

// StopWaiting ends every long poll in progress and makes later receives
// return at once, so a shutting-down server isn't held up by clients waiting
// up to 20 seconds for messages.
func (s *Store) StopWaiting() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stopped = true

	for _, q := range s.queues {
		q.signal()
	}
}

// Ensure creates name with settings, or applies settings to it when it
// exists. A dead-letter queue it names is created too.
func (s *Store) Ensure(name string, settings Settings) error {
	if err := validateSettings(name, settings); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureLocked(name, settings.withDefaults())

	if settings.DeadLetterQueue != "" {
		if _, ok := s.queues[settings.DeadLetterQueue]; !ok {
			s.ensureLocked(settings.DeadLetterQueue, Settings{}.withDefaults())
		}
	}

	return nil
}

func (s *Store) ensureLocked(name string, settings Settings) {
	now := s.now()

	if q, ok := s.queues[name]; ok {
		if q.settings != settings {
			q.settings = settings
			q.modified = now
		}
		return
	}

	s.queues[name] = &queue{name: name, settings: settings, created: now, modified: now, wake: make(chan struct{})}
}

// Create creates name, as CreateQueue does: creating an existing queue
// succeeds and leaves it as it is.
func (s *Store) Create(name string, settings Settings) error {
	if err := validateSettings(name, settings); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.queues[name]; !ok {
		s.ensureLocked(name, settings.withDefaults())
	}

	return nil
}

func validateSettings(name string, st Settings) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: queue name %q must be 1-80 letters, digits, - or _ (FIFO queues are not supported)", ErrInvalidParameter, name)
	}
	if st.VisibilityTimeout < 0 || st.VisibilityTimeout > MaxVisibilityTimeout {
		return fmt.Errorf("%w: visibility timeout must be 0-43200 seconds", ErrInvalidParameter)
	}
	if st.Delay < 0 || st.Delay > MaxDelay {
		return fmt.Errorf("%w: delay must be 0-900 seconds", ErrInvalidParameter)
	}
	if st.ReceiveWait < 0 || st.ReceiveWait > MaxWait {
		return fmt.Errorf("%w: receive wait time must be 0-20 seconds", ErrInvalidParameter)
	}
	if (st.DeadLetterQueue == "") != (st.MaxReceiveCount == 0) {
		return fmt.Errorf("%w: a redrive policy needs both a dead-letter queue and a maximum receive count", ErrInvalidParameter)
	}
	if st.MaxReceiveCount < 0 || st.MaxReceiveCount > 1000 {
		return fmt.Errorf("%w: maximum receive count must be 1-1000", ErrInvalidParameter)
	}
	if st.DeadLetterQueue != "" && !ValidName(st.DeadLetterQueue) {
		return fmt.Errorf("%w: dead-letter queue name %q is not valid", ErrInvalidParameter, st.DeadLetterQueue)
	}
	if st.DeadLetterQueue == name {
		return fmt.Errorf("%w: a queue cannot be its own dead-letter queue", ErrInvalidParameter)
	}

	return nil
}

// SetSettings replaces name's settings.
func (s *Store) SetSettings(name string, settings Settings) error {
	if err := validateSettings(name, settings); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	q, ok := s.queues[name]
	if !ok {
		return ErrNotFound
	}

	q.settings = settings.withDefaults()
	q.modified = s.now()
	q.signal()

	return nil
}

// Delete removes name and its messages.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	q, ok := s.queues[name]
	if !ok {
		return ErrNotFound
	}

	delete(s.queues, name)
	q.signal()

	return nil
}

// Purge removes every message in name.
func (s *Store) Purge(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	q, ok := s.queues[name]
	if !ok {
		return ErrNotFound
	}

	q.msgs = nil

	return nil
}

// List returns the queue names starting with prefix, sorted.
func (s *Store) List(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	names := make([]string, 0, len(s.queues))
	for name := range s.queues {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	return names
}

// Exists reports whether name exists.
func (s *Store) Exists(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.queues[name]

	return ok
}

// Info returns name's settings and message counts.
func (s *Store) Info(name string) (QueueInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	q, ok := s.queues[name]
	if !ok {
		return QueueInfo{}, ErrNotFound
	}

	now := s.now()
	s.expireLocked(q, now)

	info := QueueInfo{Name: name, Settings: q.settings, Created: q.created, Modified: q.modified}

	for _, m := range q.msgs {
		switch {
		case m.inFlight && m.visibleAt.After(now):
			info.InFlight++
		case m.visibleAt.After(now):
			info.Delayed++
		default:
			info.Visible++
		}
	}

	return info, nil
}

// Send adds messages to name.
func (s *Store) Send(name string, inputs ...SendInput) ([]SendResult, error) {
	for _, in := range inputs {
		if err := validateMessage(in); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	q, ok := s.queues[name]
	if !ok {
		return nil, ErrNotFound
	}

	now := s.now()
	results := make([]SendResult, len(inputs))

	for i, in := range inputs {
		delay := q.settings.Delay
		if in.Delay != nil {
			delay = *in.Delay
		}

		m := &message{
			Message: Message{
				ID:                     newID(),
				Body:                   in.Body,
				MD5OfBody:              md5Hex(in.Body),
				MD5OfMessageAttributes: attributesMD5(in.Attributes),
				Attributes:             in.Attributes,
				SentTimestamp:          now,
				SenderID:               AccountID,
			},
			visibleAt: now.Add(delay),
		}
		q.msgs = append(q.msgs, m)

		results[i] = SendResult{ID: m.ID, MD5OfBody: m.MD5OfBody, MD5OfMessageAttributes: m.MD5OfMessageAttributes}
	}

	q.signal()

	return results, nil
}

func validateMessage(in SendInput) error {
	if in.Body == "" {
		return fmt.Errorf("%w: the message body must not be empty", ErrInvalidParameter)
	}

	size := len(in.Body)
	for name, a := range in.Attributes {
		size += len(name) + len(a.DataType) + len(a.BinaryValue)
		if a.StringValue != nil {
			size += len(*a.StringValue)
		}

		if a.DataType == "" || (a.StringValue == nil) == (a.BinaryValue == nil) {
			return fmt.Errorf("%w: message attribute %q needs a DataType and exactly one of StringValue or BinaryValue", ErrInvalidParameter, name)
		}
	}

	if size > MaxBodyBytes {
		return fmt.Errorf("%w: the message is larger than %d bytes", ErrInvalidParameter, MaxBodyBytes)
	}

	if in.Delay != nil && (*in.Delay < 0 || *in.Delay > MaxDelay) {
		return fmt.Errorf("%w: delay must be 0-900 seconds", ErrInvalidParameter)
	}

	return nil
}

// ReceiveInput controls one receive.
type ReceiveInput struct {
	// Max is the most messages to return, 1-10.
	Max int
	// Wait is how long to wait for a message when none is available; nil
	// uses the queue's receive wait time.
	Wait *time.Duration
	// VisibilityTimeout overrides the queue's when non-nil.
	VisibilityTimeout *time.Duration
}

// Receive returns up to in.Max receivable messages from name, hiding each
// for the visibility timeout, and waits up to in.Wait for one to arrive when
// none is receivable. A message already received MaxReceiveCount times moves
// to the dead-letter queue instead. A cancelled ctx ends the wait early with
// no messages and no error.
func (s *Store) Receive(ctx context.Context, name string, in ReceiveInput) ([]Message, error) {
	if in.Max < 1 || in.Max > MaxReceiveBatch {
		return nil, fmt.Errorf("%w: the maximum number of messages must be 1-10", ErrInvalidParameter)
	}
	if in.Wait != nil && (*in.Wait < 0 || *in.Wait > MaxWait) {
		return nil, fmt.Errorf("%w: wait time must be 0-20 seconds", ErrInvalidParameter)
	}
	if in.VisibilityTimeout != nil && (*in.VisibilityTimeout < 0 || *in.VisibilityTimeout > MaxVisibilityTimeout) {
		return nil, fmt.Errorf("%w: visibility timeout must be 0-43200 seconds", ErrInvalidParameter)
	}

	var deadline time.Time

	for {
		msgs, wake, nextVisible, err := s.take(name, in)
		if err != nil || len(msgs) > 0 || s.isStopped() {
			return msgs, err
		}

		if deadline.IsZero() {
			wait := s.waitFor(name, in)
			deadline = s.now().Add(wait)
		}

		remaining := deadline.Sub(s.now())
		if remaining <= 0 {
			return nil, nil
		}

		if !nextVisible.IsZero() {
			remaining = min(remaining, max(nextVisible.Sub(s.now()), time.Millisecond))
		}

		timer := time.NewTimer(remaining)

		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil
		case <-wake:
		case <-timer.C:
		}

		timer.Stop()
	}
}

func (s *Store) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.stopped
}

// waitFor resolves the long-poll wait for one receive.
func (s *Store) waitFor(name string, in ReceiveInput) time.Duration {
	if in.Wait != nil {
		return *in.Wait
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if q, ok := s.queues[name]; ok {
		return q.settings.ReceiveWait
	}

	return 0
}

// take receives what is receivable now. When nothing is, it returns the
// queue's wake channel and the earliest time a hidden message reappears.
func (s *Store) take(name string, in ReceiveInput) ([]Message, <-chan struct{}, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	q, ok := s.queues[name]
	if !ok {
		return nil, nil, time.Time{}, ErrNotFound
	}

	now := s.now()
	s.expireLocked(q, now)

	visibility := q.settings.VisibilityTimeout
	if in.VisibilityTimeout != nil {
		visibility = *in.VisibilityTimeout
	}

	var (
		out         []Message
		kept        = q.msgs[:0]
		moved       []*message
		nextVisible time.Time
	)

	for _, m := range q.msgs {
		switch {
		case m.visibleAt.After(now):
			if nextVisible.IsZero() || m.visibleAt.Before(nextVisible) {
				nextVisible = m.visibleAt
			}
		case len(out) >= in.Max:
		case q.settings.MaxReceiveCount > 0 && m.ReceiveCount >= q.settings.MaxReceiveCount:
			moved = append(moved, m)
			continue
		default:
			m.ReceiveCount++
			if m.FirstReceiveTimestamp.IsZero() {
				m.FirstReceiveTimestamp = now
			}
			m.ReceiptHandle = newReceipt(name, m.ID)
			m.visibleAt = now.Add(visibility)
			m.inFlight = true

			out = append(out, m.Message)
		}

		kept = append(kept, m)
	}

	clear(q.msgs[len(kept):])
	q.msgs = kept

	if len(moved) > 0 {
		s.deadLetterLocked(q, moved, now)
	}

	return out, q.wake, nextVisible, nil
}

// deadLetterLocked moves msgs to q's dead-letter queue, creating it if it
// went missing. Like AWS, the message keeps its ID, body and attributes.
func (s *Store) deadLetterLocked(q *queue, msgs []*message, now time.Time) {
	dlq, ok := s.queues[q.settings.DeadLetterQueue]
	if !ok {
		s.ensureLocked(q.settings.DeadLetterQueue, Settings{}.withDefaults())
		dlq = s.queues[q.settings.DeadLetterQueue]
	}

	for _, m := range msgs {
		m.ReceiptHandle = ""
		m.inFlight = false
		m.visibleAt = now
		dlq.msgs = append(dlq.msgs, m)
	}

	dlq.signal()
}

// expireLocked drops messages older than the retention period.
func (s *Store) expireLocked(q *queue, now time.Time) {
	kept := q.msgs[:0]
	for _, m := range q.msgs {
		if now.Sub(m.SentTimestamp) < q.settings.Retention {
			kept = append(kept, m)
		}
	}

	clear(q.msgs[len(kept):])
	q.msgs = kept
}

// DeleteMessage removes the message receipt was issued for. A well-formed
// receipt for a message that is already gone, or that has since been
// received again, is a no-op, as on AWS.
func (s *Store) DeleteMessage(name, receipt string) error {
	if !validReceipt(name, receipt) {
		return ErrInvalidReceipt
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	q, ok := s.queues[name]
	if !ok {
		return ErrNotFound
	}

	for i, m := range q.msgs {
		if m.ReceiptHandle == receipt {
			q.msgs = append(q.msgs[:i], q.msgs[i+1:]...)
			break
		}
	}

	return nil
}

// ChangeVisibility sets an in-flight message's remaining visibility timeout;
// zero makes it receivable again at once.
func (s *Store) ChangeVisibility(name, receipt string, timeout time.Duration) error {
	if !validReceipt(name, receipt) {
		return ErrInvalidReceipt
	}
	if timeout < 0 || timeout > MaxVisibilityTimeout {
		return fmt.Errorf("%w: visibility timeout must be 0-43200 seconds", ErrInvalidParameter)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	q, ok := s.queues[name]
	if !ok {
		return ErrNotFound
	}

	now := s.now()

	for _, m := range q.msgs {
		if m.ReceiptHandle == receipt && m.inFlight && m.visibleAt.After(now) {
			m.visibleAt = now.Add(timeout)
			if timeout == 0 {
				q.signal()
			}
			return nil
		}
	}

	return ErrNotInflight
}

// newID returns a random UUID, the format of SQS message IDs.
func newID() string {
	var b [16]byte
	rand.Read(b[:]) //nolint:errcheck // crypto/rand.Read never fails.

	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// receiptPrefix marks Lambdary receipt handles; the queue name in them lets
// a receipt from another queue be rejected.
const receiptPrefix = "lambdary:"

func newReceipt(queue, id string) string {
	return receiptPrefix + queue + ":" + id + ":" + newID()
}

func validReceipt(queue, receipt string) bool {
	return strings.HasPrefix(receipt, receiptPrefix+queue+":")
}
