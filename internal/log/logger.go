package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type Fields map[string]interface{}

func (f Fields) String(key, value string) Fields {
	f[key] = value
	return f
}

func (f Fields) Int(key string, value int) Fields {
	f[key] = value
	return f
}

func (f Fields) UUID(key string, value interface{}) Fields {
	f[key] = value
	return f
}

type Entry struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	Fields    Fields    `json:"fields,omitempty"`
}

func NewEntry(message string) *Entry {
	return &Entry{
		Timestamp: time.Now().UTC(),
		Level:     "info",
		Message:   message,
		Fields:    make(Fields),
	}
}

func (e *Entry) With(key string, value interface{}) *Entry {
	e.Fields[key] = value
	return e
}

// WithError records a non-secret error message in the entry fields.
func (e *Entry) WithError(err error) *Entry {
	if err != nil {
		e.Fields["error"] = err.Error()
	}
	return e
}

// SetLevel sets the entry severity (e.g. "info", "warn", "error") before
// logging. Defaults to "info".
func (e *Entry) SetLevel(level string) *Entry {
	e.Level = level
	return e
}

func (e *Entry) Log() {
	data, _ := json.Marshal(e)
	fmt.Fprintln(os.Stdout, string(data))
}

func init() {
	// Ensure UTC output
}