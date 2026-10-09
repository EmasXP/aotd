package web

import (
	"net/http"
	"time"
)

func (s *Server) notifications(w http.ResponseWriter, r *http.Request) {
	ns, err := s.Store.Notifications(currentUser(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "notifications", map[string]any{"Title": "Notifications", "Notifications": ns})
}

// openNotification marks a notification read and goes to what it's about.
func (s *Server) openNotification(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	target, err := s.Store.OpenNotification(currentUser(r).ID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, target)
}

func (s *Server) readNotification(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	if err := s.Store.MarkNotificationRead(currentUser(r).ID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/notifications")
}

func (s *Server) readAllNotifications(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.MarkAllNotificationsRead(currentUser(r).ID); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/notifications")
}

// PurgeNotifications periodically deletes old notifications.
func (s *Server) PurgeNotifications(every time.Duration) {
	for range time.Tick(every) {
		if err := s.Store.PurgeNotifications(); err != nil {
			s.Log.Warn("purge notifications", "err", err)
		}
	}
}
