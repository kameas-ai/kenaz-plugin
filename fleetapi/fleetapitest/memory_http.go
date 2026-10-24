package fleetapitest

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// Identity is who an HTTP request authenticates as.
type Identity struct {
	OrgID, UserID string
	Staff         bool
}

// Handler serves the store over HTTP with Fleet's memory routes, bodies
// and {code,message,details} error envelopes, for an adapter's transport
// contract tests. auth maps a request (its bearer token) to an identity;
// ok=false answers 401. Bodies are decoded whatever the Content-Type.
func (f *MemoryFleet) Handler(auth func(*http.Request) (Identity, bool)) http.Handler {
	mux := http.NewServeMux()
	client := func(w http.ResponseWriter, r *http.Request) fleetapi.Memory {
		id, ok := auth(r)
		if !ok {
			writeErr(w, &fleetapi.APIError{Status: 401, Code: "unauthorized", Message: "authentication required"})
			return nil
		}
		if id.Staff {
			return f.StaffClient()
		}
		return f.server(id.OrgID, id.UserID)
	}
	decode := func(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeErr(w, &fleetapi.APIError{Status: 413, Code: fleetapi.CodePayloadTooLarge, Message: "request body exceeds the limit"})
				return false
			}
			writeErr(w, &fleetapi.APIError{Status: 400, Code: fleetapi.CodeInvalidRequestBody, Message: "could not read request body"})
			return false
		}
		if json.Unmarshal(body, v) != nil {
			writeErr(w, &fleetapi.APIError{Status: 400, Code: fleetapi.CodeInvalidRequestBody, Message: "request body must be a JSON object"})
			return false
		}
		return true
	}
	reply := func(w http.ResponseWriter, v any, err error) {
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc(fleetapi.MemoryRoutePush, func(w http.ResponseWriter, r *http.Request) {
		c := client(w, r)
		if c == nil {
			return
		}
		var req fleetapi.MemoryPushRequest
		if decode(w, r, fleetapi.MemoryMaxPushBytes, &req) {
			resp, err := c.Push(r.Context(), req)
			reply(w, resp, err)
		}
	})
	mux.HandleFunc(fleetapi.MemoryRoutePull, func(w http.ResponseWriter, r *http.Request) {
		c := client(w, r)
		if c == nil {
			return
		}
		q := r.URL.Query()
		req := fleetapi.MemoryPullRequest{Cursor: q.Get("cursor"), DeviceID: q.Get("device_id")}
		if l := q.Get("limit"); l != "" {
			n, err := strconv.Atoi(l)
			if err != nil || n < 1 {
				writeErr(w, &fleetapi.APIError{Status: 400, Code: fleetapi.CodeInvalidRequestBody, Message: "limit must be a positive integer"})
				return
			}
			req.Limit = n
		}
		resp, err := c.Pull(r.Context(), req)
		reply(w, resp, err)
	})
	mux.HandleFunc(fleetapi.MemoryRouteGetSettings, func(w http.ResponseWriter, r *http.Request) {
		if c := client(w, r); c != nil {
			resp, err := c.Settings(r.Context())
			reply(w, resp, err)
		}
	})
	mux.HandleFunc(fleetapi.MemoryRoutePutSettings, func(w http.ResponseWriter, r *http.Request) {
		c := client(w, r)
		if c == nil {
			return
		}
		var req fleetapi.MemorySettingsUpdate
		if decode(w, r, 16<<10, &req) {
			resp, err := c.PutSettings(r.Context(), req)
			reply(w, resp, err)
		}
	})
	mux.HandleFunc(fleetapi.MemoryRouteForget, func(w http.ResponseWriter, r *http.Request) {
		c := client(w, r)
		if c == nil {
			return
		}
		var req fleetapi.MemoryForgetRequest
		if decode(w, r, 256<<10, &req) {
			resp, err := c.Forget(r.Context(), req)
			reply(w, resp, err)
		}
	})
	mux.HandleFunc(fleetapi.MemoryRouteForgetAll, func(w http.ResponseWriter, r *http.Request) {
		c := client(w, r)
		if c == nil {
			return
		}
		var req fleetapi.MemoryForgetAllRequest
		if decode(w, r, 16<<10, &req) {
			resp, err := c.ForgetAll(r.Context(), req)
			reply(w, resp, err)
		}
	})
	mux.HandleFunc(fleetapi.MemoryRouteExport, func(w http.ResponseWriter, r *http.Request) {
		c := client(w, r)
		if c == nil {
			return
		}
		var recs []fleetapi.MemoryRecord
		tr, err := c.Export(r.Context(), func(rec fleetapi.MemoryRecord) error { recs = append(recs, rec); return nil })
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="kenaz-memory-export.ndjson"`)
		bw := bufio.NewWriter(w)
		enc := json.NewEncoder(bw)
		for _, rec := range recs {
			_ = enc.Encode(rec)
		}
		_ = enc.Encode(tr)
		_ = bw.Flush()
	})
	return mux
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *fleetapi.APIError
	if !errors.As(err, &ae) {
		ae = &fleetapi.APIError{Status: 500, Code: "internal_error", Message: "internal error"}
	}
	details := map[string]any{}
	for k, v := range ae.Details {
		if n, err := strconv.Atoi(v); err == nil {
			details[k] = n
		} else {
			details[k] = v
		}
	}
	if ae.Status == 429 {
		if s := ae.Details["retry_after_seconds"]; s != "" {
			w.Header().Set("Retry-After", s)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.Status)
	var d any
	if len(details) > 0 {
		d = details
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"code": ae.Code, "message": ae.Message, "details": d})
}
