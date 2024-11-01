package log

import (
	"io"
	"time"

	"github.com/rs/zerolog"
	"github.com/tidwall/pretty"

	"github.com/xeptore/tgtd/constant"
)

func init() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
}

func newBaseLogger() zerolog.Logger {
	return zerolog.
		New(io.Discard).
		With().
		Dict(
			"api",
			zerolog.Dict().Str("compilation_time", constant.CompileTime.Format(time.RFC3339)),
		).
		Timestamp().
		Logger().
		Level(zerolog.TraceLevel)
}

func NewPretty(w io.Writer) zerolog.Logger {
	return newBaseLogger().Output(newPrettyWriter(w))
}

func NewPacked(w io.Writer) zerolog.Logger {
	return newBaseLogger().Output(w)
}

func newPrettyWriter(out io.Writer) prettyWriter {
	return prettyWriter{out}
}

type prettyWriter struct {
	out io.Writer
}

func (p prettyWriter) Write(line []byte) (int, error) {
	if n, err := p.out.Write(pretty.Color(pretty.Pretty(line), nil)); nil != err {
		return n, err
	}
	return len(line), nil
}
