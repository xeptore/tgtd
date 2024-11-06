package errutil

import (
	"net/http"

	"github.com/xeptore/flaw/v8"
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
