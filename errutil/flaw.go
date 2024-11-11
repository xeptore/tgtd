package errutil

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	"github.com/xeptore/flaw/v8"
	"gopkg.in/yaml.v3"
)

func HTTPResponseFlawPayload(res *http.Response) flaw.P {
	out := make(flaw.P, 7)
	out["status"] = res.Status
	out["status_code"] = res.StatusCode
	out["content_length"] = res.ContentLength
	out["proto"] = res.Proto
	out["proto_major"] = res.ProtoMajor
	out["proto_minor"] = res.ProtoMinor
	headers := make(flaw.P, len(res.Header))
	for k, v := range res.Header {
		headers[k] = v
	}
	out["headers"] = headers
	return out
}

type Flaw struct {
	Inner        string        `toml:"inner"`
	Records      []Record      `toml:"records"`
	JoinedErrors []JoinedError `toml:"joined_errors"`
	StackTrace   []StackTrace  `toml:"stack_trace"`
}

type Record struct {
	Function string                 `toml:"function"`
	Payload  map[string]interface{} `toml:"payload"`
}

type JoinedError struct {
	Message          string      `toml:"message"`
	CallerStackTrace *StackTrace `toml:"caller_stack_trace"`
}

type StackTrace struct {
	File     string `toml:"file"`
	Line     int    `toml:"line"`
	Function string `toml:"function"`
}

func FlawToYAML(f *flaw.Flaw) ([]byte, error) {
	records := make([]Record, len(f.Records))
	for i, v := range f.Records {
		records[i] = Record{
			Function: v.Function,
			Payload:  v.Payload,
		}
	}

	joinedErrors := make([]JoinedError, len(f.JoinedErrors))
	for i, v := range f.JoinedErrors {
		je := JoinedError{
			Message:          v.Message,
			CallerStackTrace: nil,
		}
		if v.CallerStackTrace != nil {
			je.CallerStackTrace = &StackTrace{
				File:     v.CallerStackTrace.File,
				Line:     v.CallerStackTrace.Line,
				Function: v.CallerStackTrace.Function,
			}
		}
		joinedErrors[i] = je
	}

	stackTraces := make([]StackTrace, len(f.StackTrace))
	for i, v := range f.StackTrace {
		stackTraces[i] = StackTrace{
			File:     v.File,
			Line:     v.Line,
			Function: v.Function,
		}
	}

	fl := Flaw{
		Inner:        f.Inner,
		Records:      records,
		JoinedErrors: joinedErrors,
		StackTrace:   stackTraces,
	}
	var buf bytes.Buffer
	if err := yaml.NewEncoder(&buf).Encode(fl); err != nil {
		flawP := flaw.P{"err_debug_tree": Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to encode flaw to yaml: %v", err)).Append(flawP)
	}

	return buf.Bytes(), nil
}

func IsFlaw(err error) bool {
	if flawErr := new(flaw.Flaw); errors.As(err, &flawErr) {
		return true
	}
	return false
}
