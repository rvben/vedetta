package api

import (
	"net/http"
	"time"

	"github.com/rvben/vedetta/internal/storage"
)

const maxActivityPageSize = 200

func (s *Server) ListActivities(w http.ResponseWriter, r *http.Request, params ListActivitiesParams) {
	filters := storage.ActivityFilters{}
	if params.Camera != nil {
		filters.Camera = *params.Camera
	}
	if params.Label != nil {
		filters.Label = *params.Label
	}
	if params.Zone != nil {
		filters.Zone = *params.Zone
	}
	if params.Object != nil {
		filters.Object = *params.Object
	}
	if params.Category != nil {
		filters.Category = string(*params.Category)
	}
	if params.Kind != nil {
		filters.Kind = *params.Kind
	}
	if params.Q != nil {
		filters.Search = *params.Q
	}
	if params.After != nil {
		filters.After = *params.After
	}
	if params.Before != nil {
		filters.Before = *params.Before
	}

	limit := 50
	if params.Limit != nil && *params.Limit > 0 {
		limit = min(*params.Limit, maxActivityPageSize)
	}
	offset := 0
	if params.Offset != nil && *params.Offset >= 0 {
		offset = *params.Offset
	}

	activities, err := s.db.QueryActivitiesFiltered(filters, limit, offset)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if activities == nil {
		activities = []storage.Activity{}
	}
	total, err := s.db.CountActivitiesFiltered(filters)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items":    activities,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"has_more": offset+len(activities) < total,
	})
}

func (s *Server) GetActivityCounts(w http.ResponseWriter, r *http.Request) {
	total, err := s.db.CountActivitiesFiltered(storage.ActivityFilters{})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	now := time.Now()
	today, err := s.db.CountActivitiesFiltered(storage.ActivityFilters{
		After: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()),
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"total": total, "today": today})
}

func (s *Server) GetActivity(w http.ResponseWriter, r *http.Request, id string) {
	activity, err := s.db.GetActivityByID(id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if activity == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "activity not found"})
		return
	}
	writeJSON(w, http.StatusOK, activity)
}
