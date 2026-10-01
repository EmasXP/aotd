package web

import (
	"errors"
	"net/http"

	"github.com/EmasXP/aotd/internal/model"
	"github.com/EmasXP/aotd/internal/store"
)

func (s *Server) groups(w http.ResponseWriter, r *http.Request) {
	mine, err := s.Store.UserGroups(currentUser(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	popular, err := s.Store.PopularGroups(20)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "groups", map[string]any{"Title": "Groups", "Mine": mine, "Popular": popular})
}

func (s *Server) newGroupForm(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, http.StatusOK, "group_form", map[string]any{"Title": "New group"})
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.CreateGroup(currentUser(r).ID, r.FormValue("name"), r.FormValue("description"))
	var ve store.ValidationError
	if errors.As(err, &ve) {
		s.page(w, r, http.StatusUnprocessableEntity, "group_form", map[string]any{
			"Title": "New group", "Error": err.Error(), "Name": r.FormValue("name"), "Description": r.FormValue("description"),
		})
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/g/"+g.Slug)
}

func (s *Server) groupFromPath(w http.ResponseWriter, r *http.Request) (*model.Group, bool) {
	g, err := s.Store.GroupBySlug(r.PathValue("slug"))
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	return g, true
}

func (s *Server) showGroup(w http.ResponseWriter, r *http.Request) {
	g, ok := s.groupFromPath(w, r)
	if !ok {
		return
	}
	me := currentUser(r)
	page, err := s.Store.GroupPosts(me.ID, g.ID, store.Cursor(r.URL.Query().Get("before")))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	moreURL := "/g/" + g.Slug + "?fragment=1&before="
	if r.URL.Query().Get("fragment") == "1" {
		s.partial(w, r, "feed_items", map[string]any{"More": true, "Page": page, "MoreURL": moreURL})
		return
	}
	members, err := s.Store.GroupMembers(g.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "group", map[string]any{
		"Title": g.Name, "Group": g, "Members": members, "Page": page, "MoreURL": moreURL,
		"IsMember": s.Store.IsMember(me.ID, g.ID), "IsOwner": g.OwnerID == me.ID,
	})
}

func (s *Server) editGroupForm(w http.ResponseWriter, r *http.Request) {
	g, ok := s.groupFromPath(w, r)
	if !ok {
		return
	}
	if g.OwnerID != currentUser(r).ID {
		s.fail(w, r, store.ErrForbidden)
		return
	}
	s.page(w, r, http.StatusOK, "group_form", map[string]any{
		"Title": "Edit " + g.Name, "Group": g, "Name": g.Name, "Description": g.Description,
	})
}

func (s *Server) updateGroup(w http.ResponseWriter, r *http.Request) {
	g, ok := s.groupFromPath(w, r)
	if !ok {
		return
	}
	err := s.Store.UpdateGroup(currentUser(r).ID, g.ID, r.FormValue("name"), r.FormValue("description"))
	var ve store.ValidationError
	if errors.As(err, &ve) {
		s.page(w, r, http.StatusUnprocessableEntity, "group_form", map[string]any{
			"Title": "Edit " + g.Name, "Group": g, "Error": err.Error(),
			"Name": r.FormValue("name"), "Description": r.FormValue("description"),
		})
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/g/"+g.Slug)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	g, ok := s.groupFromPath(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteGroup(currentUser(r).ID, g.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/groups")
}

func (s *Server) joinGroup(w http.ResponseWriter, r *http.Request)  { s.setMembership(w, r, true) }
func (s *Server) leaveGroup(w http.ResponseWriter, r *http.Request) { s.setMembership(w, r, false) }

func (s *Server) setMembership(w http.ResponseWriter, r *http.Request, join bool) {
	g, ok := s.groupFromPath(w, r)
	if !ok {
		return
	}
	me := currentUser(r)
	var err error
	if join {
		err = s.Store.JoinGroup(me.ID, g.ID)
	} else {
		err = s.Store.LeaveGroup(me.ID, g.ID)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Membership changes the feed and member list, so reload the page.
	redirect(w, r, "/g/"+g.Slug)
}
