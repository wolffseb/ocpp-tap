package cpms

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// ControlHandler lets a test or an operator drive a standalone harness CPMS:
//
//	POST /mode?m=<mode>                      switch failure mode
//	POST /call?charger=<id>&action=<action>  send a CSMS-initiated CALL; the
//	                                         body is the payload, the reply
//	                                         is the charger's answer
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mode", func(w http.ResponseWriter, r *http.Request) {
		m, err := ParseMode(r.URL.Query().Get("m"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.SetMode(m)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /call", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) == 0 {
			body = []byte("{}")
		}
		if !json.Valid(body) {
			http.Error(w, "payload is not JSON", http.StatusBadRequest)
			return
		}
		res, err := s.Call(r.Context(), q.Get("charger"), q.Get("action"), json.RawMessage(body))
		switch {
		case errors.Is(err, ErrNotConnected):
			http.Error(w, err.Error(), http.StatusNotFound)
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadGateway)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(append(res, '\n'))
		}
	})
	return mux
}
