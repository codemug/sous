package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/codemug/sous/internal/gateway"
	"github.com/codemug/sous/internal/reqlog"
)

// reqLogAdapter turns a *reqlog.Writer into what the gateway asks for.
//
// A SEPARATE TYPE rather than reqlog.Writer implementing gateway.RequestLog
// directly: the gateway's interface is fire-and-forget (a request must never
// fail because an audit write failed), while Writer.Log returns an error
// because that is the honest signature for something writing to disk. This
// is where that error is turned into a log line instead of a failed request.
type reqLogAdapter struct{ w *reqlog.Writer }

func (a *reqLogAdapter) Log(sender, remoteAddr, model string, body []byte) {
	err := a.w.Log(reqlog.Entry{
		Time: time.Now(), Sender: sender, RemoteAddr: remoteAddr,
		Model: model, Body: json.RawMessage(body),
	})
	if err != nil {
		log.Printf("reqlog: %v", err)
	}
}

// reqLog adapts rl for the gateway, or returns a genuinely nil interface when
// rl is nil.
//
// NOT JUST &reqLogAdapter{w: nil}: a *reqLogAdapter wrapping a nil Writer is
// a NON-NIL gateway.RequestLog - the gateway's `g.ReqLog != nil` check would
// pass and it would call Log on a nil *Writer. This is the standard Go
// nil-interface trap, worth a named function specifically so it cannot be
// gotten wrong at the call site.
func reqLog(rl *reqlog.Writer) gateway.RequestLog {
	if rl == nil {
		return nil
	}
	return &reqLogAdapter{w: rl}
}

// reqLogView is what the admin page shows about the log.
type reqLogView struct {
	RetentionDays int
	Files         []reqlog.FileInfo
	TotalBytes    int64
}

func (s *Server) reqLogView() reqLogView {
	v := reqLogView{RetentionDays: reqlog.DefaultRetentionDays}
	if s.reqLogR != nil {
		v.RetentionDays = s.reqLogR.Days()
	}
	if s.reqLogW != nil {
		if files, err := s.reqLogW.Files(); err == nil {
			v.Files = files
			for _, f := range files {
				v.TotalBytes += f.Bytes
			}
		}
	}
	return v
}

// setRetention updates how many days of request logs are kept.
func (s *Server) setRetention(w http.ResponseWriter, r *http.Request) {
	if s.reqLogR == nil {
		writeErr(w, http.StatusInternalServerError, "no retention store")
		return
	}
	_ = r.ParseForm()
	n, err := strconv.Atoi(r.PostFormValue("days"))
	if err != nil || n < 0 {
		msg := "days must be a whole number, 0 or more"
		if wantsHTML(r) {
			s.redirect(w, r, "/admin", msg, true)
			return
		}
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if err := s.reqLogR.SetDays(n); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if wantsHTML(r) {
		msg := "request log retention set to " + strconv.Itoa(n) + " days"
		if n == 0 {
			msg = "request log retention set to 0 days - only today's log is kept from the next cleanup on"
		}
		s.redirect(w, r, "/admin", msg, false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"days": n})
}

// getReqLog reports the current retention and what is actually on disk.
func (s *Server) getReqLog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.reqLogView())
}
