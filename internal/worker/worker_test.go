package worker

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"austro-os/internal/event"

	amqp "github.com/rabbitmq/amqp091-go"
)

// fakeAcknowledger records how a delivery was settled so the tests can assert
// the exact ack/nack decision without a live broker.
type fakeAcknowledger struct {
	acked        int
	nacked       int
	nackRequeue  bool
	rejected     int
	lastMultiple bool
}

func (f *fakeAcknowledger) Ack(tag uint64, multiple bool) error {
	f.acked++
	f.lastMultiple = multiple
	return nil
}

func (f *fakeAcknowledger) Nack(tag uint64, multiple, requeue bool) error {
	f.nacked++
	f.nackRequeue = requeue
	return nil
}

func (f *fakeAcknowledger) Reject(tag uint64, requeue bool) error {
	f.rejected++
	return nil
}

func delivery(t *testing.T, ack *fakeAcknowledger, body string) amqp.Delivery {
	t.Helper()
	return amqp.Delivery{
		Acknowledger: ack,
		DeliveryTag:  1,
		MessageId:    "test-message",
		Body:         []byte(body),
	}
}

func envelopeBody(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(event.NewEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(b)
}

// TestProcessMessageSettling covers the ack/nack contract. A permanent failure
// must be dropped rather than requeued, because with a prefetch of 1 a message
// that can never be processed blocks every message behind it. A transient
// failure must still be requeued: dropping it would lose work.
func TestProcessMessageSettling(t *testing.T) {
	cases := []struct {
		name        string
		handler     func(*event.UniversalEnvelope) error
		body        string
		wantAck     bool
		wantNack    bool
		wantRequeue bool
	}{
		{
			name:    "success is acknowledged",
			handler: func(*event.UniversalEnvelope) error { return nil },
			wantAck: true,
		},
		{
			name:        "transient failure is requeued",
			handler:     func(*event.UniversalEnvelope) error { return errors.New("connection reset") },
			wantNack:    true,
			wantRequeue: true,
		},
		{
			name: "permanent failure is dropped, not requeued",
			handler: func(*event.UniversalEnvelope) error {
				return PermanentErrorf("unknown stage %q", "nonsense")
			},
			wantAck: true,
		},
		{
			name: "a wrapped permanent failure is still recognised",
			handler: func(*event.UniversalEnvelope) error {
				return errors.Join(errors.New("context"), PermanentErrorf("bad payload"))
			},
			wantAck: true,
		},
		{
			name:    "undecodable body is drained rather than requeued",
			handler: func(*event.UniversalEnvelope) error { return nil },
			body:    "{not json",
			wantAck: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if body == "" {
				body = envelopeBody(t)
			}
			ack := &fakeAcknowledger{}
			w := NewWorker(WorkerConfig{QueueName: "test"}, tc.handler)
			w.processMessage(delivery(t, ack, body))

			if tc.wantAck && ack.acked != 1 {
				t.Errorf("expected exactly one ack, got %d", ack.acked)
			}
			if !tc.wantAck && ack.acked != 0 {
				t.Errorf("expected no ack, got %d", ack.acked)
			}
			if tc.wantNack && ack.nacked != 1 {
				t.Errorf("expected exactly one nack, got %d", ack.nacked)
			}
			if !tc.wantNack && ack.nacked != 0 {
				t.Errorf("expected no nack, got %d", ack.nacked)
			}
			if tc.wantNack && ack.nackRequeue != tc.wantRequeue {
				t.Errorf("requeue=%v, want %v", ack.nackRequeue, tc.wantRequeue)
			}
			if ack.acked+ack.nacked != 1 {
				t.Errorf("the delivery must be settled exactly once; ack=%d nack=%d", ack.acked, ack.nacked)
			}
		})
	}
}

// TestPermanentErrorIsIdentifiable pins the sentinel contract handlers rely on.
func TestPermanentErrorIsIdentifiable(t *testing.T) {
	if !errors.Is(PermanentErrorf("bad %s", "payload"), ErrPermanent) {
		t.Error("PermanentErrorf must wrap ErrPermanent")
	}
	if errors.Is(errors.New("transient"), ErrPermanent) {
		t.Error("an ordinary error must not be classified as permanent")
	}
	if !strings.Contains(PermanentErrorf("unknown stage %q", "x").Error(), "unknown stage") {
		t.Error("the formatted reason must survive the wrap")
	}
}

// TestWithTraceIDRejectsMalformedInput is the regression test for a panic:
// uuid.MustParse aborts the process when the id is not a UUID.
func TestWithTraceIDRejectsMalformedInput(t *testing.T) {
	ev := event.NewEnvelope()
	fn := WithTraceID("not-a-uuid", "worker-1")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithTraceID panicked on malformed input: %v", r)
		}
	}()
	if err := fn(ev); err == nil {
		t.Fatal("a malformed trace id must produce an error")
	}

	ok := WithTraceID("11111111-1111-1111-1111-111111111111", "worker-1")
	if err := ok(ev); err != nil {
		t.Fatalf("a valid trace id must be accepted: %v", err)
	}
	if ev.TraceID.String() != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("trace id not stamped: %s", ev.TraceID)
	}
}

// TestWithCorrelationIDProducesValidJSON proves the payload is marshalled
// rather than concatenated, so a hostile correlation id cannot break out of
// the JSON document.
func TestWithCorrelationIDProducesValidJSON(t *testing.T) {
	hostile := `x","injected":"yes`
	ev := event.NewEnvelope()
	if err := WithCorrelationID(hostile)(ev); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var decoded map[string]string
	if err := json.Unmarshal(ev.Details, &decoded); err != nil {
		t.Fatalf("details are not valid JSON: %v (raw: %s)", err, string(ev.Details))
	}
	if len(decoded) != 1 {
		t.Errorf("the correlation id must not be able to add fields: %v", decoded)
	}
	if decoded["correlation_id"] != hostile {
		t.Errorf("correlation id round-trip mismatch: %q", decoded["correlation_id"])
	}
	if _, injected := decoded["injected"]; injected {
		t.Error("a hostile correlation id injected an extra field")
	}
}
