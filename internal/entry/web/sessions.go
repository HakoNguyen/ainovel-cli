package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func (s *server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.db.getBook(id); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	list, err := s.db.listSessions(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.db.getBook(id); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	sess, err := s.db.createSession(id, strings.TrimSpace(body.Title))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	sid, ok := sessionParam(w, r)
	if !ok {
		return
	}
	if _, err := s.db.getSession(sid); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	msgs, err := s.db.listMessages(sid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (s *server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	sid, ok := sessionParam(w, r)
	if !ok {
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("Tên phiên trống"))
		return
	}
	if err := s.db.renameSession(sid, title); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	sid, ok := sessionParam(w, r)
	if !ok {
		return
	}
	if err := s.db.deleteSession(sid); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func sessionParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	sid, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil || sid <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("mã phiên không hợp lệ"))
		return 0, false
	}
	return sid, true
}
