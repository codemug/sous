package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
)

// hfView is everything about the token that may leave this process.
//
// NO FULL VALUE, ever, on any path. An API key page can reveal a secret once
// because Sous minted it and the operator has nowhere else to get it; this one
// came from the operator, who still has it wherever they copied it from. There
// is nothing to recover here, so reading it back is all risk and no use.
type hfView struct {
	Configured bool   `json:"configured"`
	Hint       string `json:"hint,omitempty"`
}

func (s *Server) hfView() hfView {
	if s.hf == nil {
		return hfView{}
	}
	return hfView{Configured: s.hf.Configured(), Hint: s.hf.Hint()}
}

// pageAdmin renders the node's own settings.
//
// A SECTION RATHER THAN A PANEL ON ANOTHER PAGE. The token first lived on the
// Larder because that is where a failed gated download is discovered, but where
// something is diagnosed and where it is configured are different questions.
func (s *Server) pageAdmin(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, "admin", "Admin", func(d *pageData) error {
		d.HF = s.hfView()
		d.ReqLog = s.reqLogView()
		return nil
	})
}

// getHFToken reports whether a token is installed, never what it is.
func (s *Server) getHFToken(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hfView())
}

// setHFToken installs a token.
//
// Accepts a form field or a JSON body, because this is reached both from the
// dashboard and from a script setting up a node.
func (s *Server) setHFToken(w http.ResponseWriter, r *http.Request) {
	if s.hf == nil {
		writeErr(w, http.StatusInternalServerError, "no token store")
		return
	}
	tok := ""
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "body: "+err.Error())
			return
		}
		tok = body.Token
	} else {
		_ = r.ParseForm()
		tok = r.PostFormValue("token")
	}

	if err := s.hf.Set(tok); err != nil {
		// The validation message names what is wrong with the value - a missing
		// hf_ prefix, embedded whitespace - because the alternative is finding
		// out as an opaque 401 inside a download container ten minutes later.
		if wantsHTML(r) {
			s.redirect(w, r, "/admin", err.Error(), true)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/admin", "HuggingFace token saved as "+s.hf.Hint(), false)
		return
	}
	writeJSON(w, http.StatusOK, s.hfView())
}

// clearHFToken removes the token. Idempotent: the point is the end state.
func (s *Server) clearHFToken(w http.ResponseWriter, r *http.Request) {
	if s.hf == nil {
		writeErr(w, http.StatusInternalServerError, "no token store")
		return
	}
	// FOUND WHILE REPLACING THE TYPED-NAME FLOW: this was the one destructive
	// route the shared check never reached. The Admin page rendered a
	// confirmation drawer for it like the other three, but the handler
	// deleted the token on any POST regardless of what the form carried - the
	// confirmation was decoration.
	if wantsHTML(r) && !s.requireConfirm(w, r, "the HuggingFace token", "/admin") {
		return
	}
	if err := s.hf.Clear(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if wantsHTML(r) {
		// Says what STOPS working rather than just confirming the delete.
		// Public weights keep downloading; gated ones start failing at 401,
		// and that is the whole difference.
		s.redirect(w, r, "/admin",
			"HuggingFace token removed - gated repos will now fail with 401", false)
		return
	}
	writeJSON(w, http.StatusOK, s.hfView())
}
