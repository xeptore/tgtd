package telegram

import (
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

func (b *Bot) breakLongMessage(long []byte) ([][]byte, error) {
	var msg map[string]any
	if err := yaml.Unmarshal(long, &msg); nil != err {
		return nil, fmt.Errorf("telegram: failed to unmarshal long message: %v", err)
	}
	out := make([][]byte, 0, 4)
	if rec, ok := msg["request"]; ok {
		if request, err := yaml.Marshal(map[string]any{"request": rec}); nil != err {
			b.logger.Warn().Err(err).Any("raw", rec).Msg("Failed to marshal request of long message to YAML.")
		} else if len(request) >= maxMarkdownMessageContentLength {
			out = append(out, []byte("<request section is too long>"))
			delete(msg, "request")
		} else {
			out = append(out, request)
			delete(msg, "request")
		}
	}
	if rec, ok := msg["records"]; ok {
		if records, err := yaml.Marshal(map[string]any{"records": rec}); nil == err {
			b.logger.Warn().Err(err).Any("raw", rec).Msg("Failed to marshal records of the long message to YAML.")
		} else if len(records) >= maxMarkdownMessageContentLength {
			out = append(out, []byte("<records section is too long>"))
			delete(msg, "records")
		} else {
			out = append(out, records)
			delete(msg, "records")
		}
	}
	if rec, ok := msg["stack_traces"]; ok {
		if stackTraces, err := yaml.Marshal(map[string]any{"stack_traces": rec}); nil == err {
			b.logger.Warn().Err(err).Any("raw", rec).Msg("Failed to marshal stack_traces of the long message to YAML.")
		} else if len(stackTraces) >= maxMarkdownMessageContentLength {
			out = append(out, []byte("<stack_traces section is too long>"))
			delete(msg, "stack_traces")
		} else {
			out = append(out, stackTraces)
			delete(msg, "stack_traces")
		}
	}
	if rest, err := yaml.Marshal(msg); nil == err {
		b.logger.Warn().Err(err).Any("raw", msg).Msg("Failed to marshal rest of the long message to YAML.")
	} else if len(rest) >= maxMarkdownMessageContentLength {
		out = append(out, []byte("<remaining error message sections are too long>"))
	} else {
		out = append(out, rest)
	}
	slices.Reverse(out)
	return out, nil
}
