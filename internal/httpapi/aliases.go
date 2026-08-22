package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
)

// aliasBody is the JSON shape for setting a model's aliases.
type aliasBody struct {
	Aliases []string `json:"aliases"`
}

// getAliases lists every alias on the node, by recipe id.
//
// The whole map rather than one model's: the question an operator actually has
// is "what names are taken", and answering it one model at a time turns that
// into a loop over the catalog.
func (s *Server) getAliases(w http.ResponseWriter, r *http.Request) {
	all, err := s.alias.All()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if all == nil {
		all = map[string][]string{}
	}
	writeJSON(w, http.StatusOK, all)
}

// setAliases replaces the alias list for one model.
//
// REPLACES, not appends. An append-only endpoint needs a delete endpoint to be
// usable, and two endpoints that must agree about validation is how the two
// drift apart. One list in, one list stored.
func (s *Server) setAliases(w http.ResponseWriter, r *http.Request) {
	id, ok := id(r, w)
	if !ok {
		return
	}

	var names []string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var b aliasBody
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil {
			writeErr(w, http.StatusBadRequest, "body: "+err.Error())
			return
		}
		names = b.Aliases
	} else {
		_ = r.ParseForm()
		// A form submits one text field. Commas and whitespace both separate,
		// because both are what people type into a box labelled "names".
		names = strings.FieldsFunc(r.PostFormValue("aliases"), func(c rune) bool {
			return c == ',' || c == ' ' || c == '\n' || c == '\t' || c == '\r'
		})
	}

	if err := s.alias.Set(id, names); err != nil {
		// 409, not 400: a collision is not a malformed request. The caller is
		// asking for something reasonable that conflicts with the current state,
		// and the difference matters to anything deciding whether to retry.
		code := http.StatusConflict
		if strings.Contains(err.Error(), "cannot carry") ||
			strings.Contains(err.Error(), "longer than") ||
			strings.Contains(err.Error(), "listed twice") {
			code = http.StatusBadRequest
		}
		if wantsHTML(r) {
			s.redirect(w, r, "/model/"+id, err.Error(), true)
			return
		}
		writeErr(w, code, err.Error())
		return
	}

	if wantsHTML(r) {
		msg := "aliases cleared"
		if got := s.alias.Of(id); len(got) > 0 {
			msg = "now also reachable as " + strings.Join(got, ", ")
		}
		s.redirect(w, r, "/model/"+id, msg, false)
		return
	}
	writeJSON(w, http.StatusOK, aliasBody{Aliases: s.alias.Of(id)})
}
