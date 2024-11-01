package log

import (
	"errors"
	"fmt"

	"github.com/goccy/go-json"
	"github.com/rs/zerolog"
	"github.com/xeptore/flaw/v8"
)

func FlawError(err error) func(e *zerolog.Event) {
	return func(e *zerolog.Event) {
		if flawErr := new(flaw.Flaw); errors.As(err, &flawErr) {
			e.Str("inner_error", flawErr.Inner)

			records := zerolog.Arr()
			for _, v := range flawErr.Records {
				if b, err := json.MarshalWithOption(v.Payload, json.UnorderedMap(), json.DisableNormalizeUTF8(), json.DisableHTMLEscape()); nil != err {
					records.Dict(zerolog.Dict().Str("function", v.Function).Dict("payload", zerolog.Dict().Str("error", err.Error()).Str("raw", fmt.Sprintf("%#+v", v.Payload))))
				} else {
					records.Dict(zerolog.Dict().Str("function", v.Function).RawJSON("payload", b))
				}
			}
			e.Array("records", records)

			joined := zerolog.Arr()
			for _, v := range flawErr.JoinedErrors {
				d := zerolog.
					Dict().
					Str("message", v.Message)
				if st := v.CallerStackTrace; nil != st {
					d.Dict(
						"caller_stack_trace",
						zerolog.
							Dict().
							Str("location", fmt.Sprintf("%s:%d", st.File, st.Line)).
							Str("function", st.Function),
					)
				} else {
					d.Stringer("caller_stack_trace", nil)
				}
				joined.Dict(d)
			}
			e.Array("joined_errors", joined)

			stackTraces := zerolog.Arr()
			for _, v := range flawErr.StackTrace {
				stackTraces.Dict(zerolog.Dict().Str("location", fmt.Sprintf("%s:%d", v.File, v.Line)).Str("function", v.Function))
			}
			e.Array("stack_traces", stackTraces)

			return
		}
		e.Err(err)
	}
}
