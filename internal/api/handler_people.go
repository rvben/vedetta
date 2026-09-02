package api

import (
	"encoding/json"
	"image"
	"image/draw"
	"image/jpeg"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/reid"
	"github.com/rvben/vedetta/internal/storage"
)

func (s *Server) ListPeople(w http.ResponseWriter, r *http.Request) {
	people, err := s.db.ListPeople()
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	type personResponse struct {
		ID              int64     `json:"id"`
		Name            string    `json:"name"`
		Ignore          bool      `json:"ignore"`
		FaceCount       int       `json:"face_count"`
		AppearanceCount int       `json:"appearance_count"`
		BestFaceID      int64     `json:"best_face_id,omitempty"`
		SourceEventID   string    `json:"source_event_id,omitempty"`
		CreatedAt       time.Time `json:"created_at"`
	}

	resp := make([]personResponse, 0, len(people))
	for _, p := range people {
		// Faces are loaded in full because the thumbnail is the
		// highest-confidence one; the appearance count is a COUNT so the page
		// never loads every event a person appears in just to measure it.
		faces, err := s.db.ListFacesByPerson(p.ID, 0)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		var appearanceCount int
		if p.Name != "" {
			appearanceCount, err = s.db.CountEventsFiltered(storage.EventFilters{Object: p.Name})
			if err != nil {
				s.serverError(w, r, err)
				return
			}
		}
		// Pick highest-confidence face for thumbnail
		var bestFaceID int64
		var bestConf float64
		for _, f := range faces {
			if f.Confidence > bestConf {
				bestConf = f.Confidence
				bestFaceID = f.ID
			}
		}
		resp = append(resp, personResponse{
			ID:              p.ID,
			Name:            p.Name,
			Ignore:          p.Ignore,
			FaceCount:       len(faces),
			AppearanceCount: appearanceCount,
			BestFaceID:      bestFaceID,
			SourceEventID:   p.SourceEventID,
			CreatedAt:       p.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    resp,
		"total":    len(resp),
		"limit":    len(resp),
		"offset":   0,
		"has_more": false,
	})
}

func (s *Server) GetPerson(w http.ResponseWriter, r *http.Request, id int64) {
	person, err := s.db.GetPerson(id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if person == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "person not found"})
		return
	}
	faces, err := s.db.ListFacesByPerson(id, 20)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	type faceResponse struct {
		ID         int64     `json:"id"`
		Camera     string    `json:"camera"`
		Confidence float64   `json:"confidence"`
		Similarity *float64  `json:"similarity,omitempty"`
		CropPath   string    `json:"crop_path"`
		Timestamp  time.Time `json:"timestamp"`
	}
	faceResp := make([]faceResponse, 0, len(faces))
	for _, f := range faces {
		faceResp = append(faceResp, faceResponse{
			ID:         f.ID,
			Camera:     f.Camera,
			Confidence: f.Confidence,
			Similarity: f.Similarity,
			CropPath:   f.CropPath,
			Timestamp:  f.Timestamp,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         person.ID,
		"name":       person.Name,
		"ignore":     person.Ignore,
		"face_count": len(faces),
		"created_at": person.CreatedAt,
		"faces":      faceResp,
	})
}

func (s *Server) UpdatePerson(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Name   *string `json:"name"`
		Ignore *bool   `json:"ignore"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name != nil {
		name, err := normalizeOptionalDisplayName(*req.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.db.UpdatePersonName(id, name); err != nil {
			s.serverError(w, r, err)
			return
		}
		_ = s.db.UpdateSubLabelsForPerson(id, name)
	}
	if req.Ignore != nil {
		if err := s.db.SetPersonIgnore(id, *req.Ignore); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) DeletePerson(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.db.DeletePerson(id); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) MergePeople(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TargetID int64 `json:"target_id"`
		SourceID int64 `json:"source_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.TargetID == 0 || req.SourceID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target_id and source_id are required"})
		return
	}
	if req.TargetID == req.SourceID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot merge a person with themselves"})
		return
	}

	if err := s.db.MergePeople(req.TargetID, req.SourceID); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Recompute target person's centroid from all their faces
	faces, _ := s.db.ListFacesByPerson(req.TargetID, 0)
	if len(faces) > 0 {
		var centroid []float32
		for _, f := range faces {
			emb := detect.BytesToFloat32(f.Embedding)
			if centroid == nil {
				centroid = make([]float32, len(emb))
			}
			for i := range emb {
				centroid[i] += emb[i]
			}
		}
		n := float32(len(faces))
		var norm float64
		for i := range centroid {
			centroid[i] /= n
			norm += float64(centroid[i]) * float64(centroid[i])
		}
		if norm > 1e-10 {
			invNorm := float32(1.0 / math.Sqrt(norm))
			for i := range centroid {
				centroid[i] *= invNorm
			}
		}
		_ = s.db.UpdatePersonCentroid(req.TargetID, detect.Float32ToBytes(centroid))
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "merged"})
}

func (s *Server) ListPersonFaces(w http.ResponseWriter, r *http.Request, id int64, params ListPersonFacesParams) {
	limit := 50
	if params.Limit != nil && *params.Limit > 0 {
		limit = *params.Limit
	}
	faces, err := s.db.ListFacesByPerson(id, limit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if faces == nil {
		faces = []storage.Face{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    faces,
		"total":    len(faces),
		"limit":    limit,
		"offset":   0,
		"has_more": false,
	})
}

func (s *Server) ListPersonEvents(w http.ResponseWriter, r *http.Request, id int64) {
	person, err := s.db.GetPerson(id)
	if err != nil || person == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "person not found"})
		return
	}
	if person.Name == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"items":    []camera.Event{},
			"total":    0,
			"limit":    20,
			"offset":   0,
			"has_more": false,
		})
		return
	}
	events, err := s.db.QueryEventsFiltered(storage.EventFilters{Object: person.Name}, 20, 0)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if events == nil {
		events = []camera.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    events,
		"total":    len(events),
		"limit":    20,
		"offset":   0,
		"has_more": false,
	})
}

func (s *Server) ListUnmatchedFaces(w http.ResponseWriter, r *http.Request, params ListUnmatchedFacesParams) {
	limit := 50
	if params.Limit != nil && *params.Limit > 0 {
		limit = *params.Limit
	}
	faces, err := s.db.ListUnmatchedFaces(limit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if faces == nil {
		faces = []storage.Face{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    faces,
		"total":    len(faces),
		"limit":    limit,
		"offset":   0,
		"has_more": false,
	})
}

func (s *Server) AssignFace(w http.ResponseWriter, r *http.Request, id int64) {
	faceID := id
	var req struct {
		PersonID int64  `json:"person_id"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	personID := req.PersonID
	if personID == 0 {
		name, err := normalizeOptionalDisplayName(req.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if name == "" {
			name = "Unknown"
		}
		newID, err := s.db.SavePerson(name, false, nil)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		personID = newID
	}

	if err := s.db.UpdateFacePerson(faceID, personID, 1.0); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Set sub_label on the face's event
	if p, _ := s.db.GetPerson(personID); p != nil && p.Name != "" {
		_ = s.db.UpdateSubLabelsForPerson(personID, p.Name)
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "assigned", "person_id": personID})
}

func (s *Server) IgnoreFace(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.db.DeleteFace(id); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
}

func (s *Server) GetFaceCrop(w http.ResponseWriter, r *http.Request, id int64) {
	cropPath, err := s.db.GetFaceCropPath(id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if cropPath == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no crop available"})
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, cropPath)
}

func (s *Server) BackfillFaces(w http.ResponseWriter, r *http.Request) {
	if s.faceRecognizer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "face recognition not available"})
		return
	}
	if s.snapshotPath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no snapshot path configured"})
		return
	}
	if !s.beginFaceBackfill() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "face backfill already running"})
		return
	}
	defer s.endFaceBackfill()

	// Get already-processed snapshot files to skip
	processedEvents, err := s.db.FaceEventIDs()
	if err != nil {
		slog.Warn("could not load processed face events", "error", err)
	}
	processed := make(map[string]bool, len(processedEvents))
	for _, eid := range processedEvents {
		processed[eid] = true
	}

	// Scan all snapshot JPEGs on disk (not just person events)
	type snapshotEntry struct {
		camera  string
		eventID string
		path    string
	}
	var snapshots []snapshotEntry
	camDirs, _ := os.ReadDir(s.snapshotPath)
	for _, camDir := range camDirs {
		if !camDir.IsDir() {
			continue
		}
		camName := camDir.Name()
		camPath := filepath.Join(s.snapshotPath, camName)
		files, _ := os.ReadDir(camPath)
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jpg") {
				continue
			}
			eventID := strings.TrimSuffix(f.Name(), ".jpg")
			if processed[eventID] {
				continue
			}
			snapshots = append(snapshots, snapshotEntry{
				camera:  camName,
				eventID: eventID,
				path:    filepath.Join(camPath, f.Name()),
			})
		}
	}

	// Stream progress as JSON lines
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	var totalScanned, totalFaces, totalNewPeople, totalMatched, skipped int
	skipped = len(processedEvents)

	for _, snap := range snapshots {
		select {
		case <-r.Context().Done():
			slog.Info("face backfill cancelled by client", "scanned", totalScanned)
			return
		default:
		}

		f, err := os.Open(snap.path)
		if err != nil {
			continue
		}

		img, err := jpeg.Decode(f)
		f.Close()
		if err != nil {
			continue
		}

		// Convert to RGBA
		rgba, ok := img.(*image.RGBA)
		if !ok {
			bounds := img.Bounds()
			rgba = image.NewRGBA(bounds)
			draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)
		}

		// Use full image — let SCRFD find all faces
		bounds := rgba.Bounds()
		fullBox := [4]int{bounds.Min.X, bounds.Min.Y, bounds.Max.X, bounds.Max.Y}

		results := s.faceRecognizer.DetectAndEmbed(rgba, fullBox, s.faceCropDir)
		totalScanned++

		if len(results) > 0 {
			slog.Info("backfill: faces detected", "snapshot", snap.eventID, "count", len(results))
		}

		for _, result := range results {
			personID, similarity := s.matchFaceToPerson(result.Embedding)

			face := storage.Face{
				EventID:    snap.eventID,
				Camera:     snap.camera,
				Embedding:  detect.Float32ToBytes(result.Embedding),
				CropPath:   result.CropPath,
				Confidence: float64(result.Confidence),
				Timestamp:  time.Now(),
			}
			if personID > 0 {
				face.PersonID = &personID
				face.Similarity = &similarity
			}

			faceID, saveErr := s.db.SaveFace(face)
			if saveErr != nil {
				slog.Error("backfill: failed to save face", "error", saveErr)
				continue
			}

			totalFaces++

			if personID > 0 {
				s.updatePersonCentroid(personID, result.Embedding)
				totalMatched++
			} else {
				if s.clusterUnmatchedFace(faceID, result.Embedding) {
					totalNewPeople++
				}
			}
		}

		// Stream progress every 20 snapshots
		if totalScanned%20 == 0 && canFlush {
			progress := map[string]int{
				"scanned":    totalScanned,
				"faces":      totalFaces,
				"matched":    totalMatched,
				"new_people": totalNewPeople,
				"skipped":    skipped,
			}
			json.NewEncoder(w).Encode(progress)
			flusher.Flush()
		}
	}

	// Final result
	result := map[string]int{
		"scanned":    totalScanned,
		"faces":      totalFaces,
		"matched":    totalMatched,
		"new_people": totalNewPeople,
		"skipped":    skipped,
	}
	json.NewEncoder(w).Encode(result)
	if canFlush {
		flusher.Flush()
	}
}

func (s *Server) clusterUnmatchedFace(newFaceID int64, embedding []float32) bool {
	unmatched, err := s.db.ListUnmatchedFaces(200)
	if err != nil || len(unmatched) == 0 {
		return false
	}

	candidates := make([]reid.Candidate, 0, len(unmatched)-1)
	for i := range unmatched {
		if unmatched[i].ID == newFaceID {
			continue
		}
		other := detect.BytesToFloat32(unmatched[i].Embedding)
		if len(other) == 0 {
			continue
		}
		candidates = append(candidates, reid.Candidate{ID: unmatched[i].ID, Centroid: other})
	}

	bestID, bestSim := reid.BestMatch(embedding, candidates, reid.FaceClusterThreshold)
	if bestID == 0 {
		return false
	}
	var bestFace *storage.Face
	for i := range unmatched {
		if unmatched[i].ID == bestID {
			bestFace = &unmatched[i]
			break
		}
	}
	if bestFace == nil {
		return false
	}

	centroid := reid.AverageNormalized(embedding, detect.BytesToFloat32(bestFace.Embedding))
	personID, err := s.db.SavePerson("", false, detect.Float32ToBytes(centroid))
	if err != nil {
		return false
	}
	_ = s.db.UpdateFacePerson(bestFace.ID, personID, bestSim)
	_ = s.db.UpdateFacePerson(newFaceID, personID, 1.0)
	return true
}
