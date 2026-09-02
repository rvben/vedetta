package api

import (
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/reid"
	"github.com/rvben/vedetta/internal/safepath"
	"github.com/rvben/vedetta/internal/snapshot"
	"github.com/rvben/vedetta/internal/storage"
)

func (s *Server) ListEvents(w http.ResponseWriter, r *http.Request, params ListEventsParams) {
	var cameraFilter, labelFilter, zoneFilter, objectFilter string
	if params.Camera != nil {
		cameraFilter = *params.Camera
	}
	if params.Label != nil {
		labelFilter = *params.Label
	}
	if params.Zone != nil {
		zoneFilter = *params.Zone
	}
	if params.Object != nil {
		objectFilter = *params.Object
	}
	limit := 50
	if params.Limit != nil && *params.Limit > 0 {
		limit = *params.Limit
	}

	offset := 0
	if params.Offset != nil && *params.Offset >= 0 {
		offset = *params.Offset
	}

	var sinceTime time.Time
	if params.Since != nil {
		sinceTime = *params.Since
	}

	var kindFilter string
	if params.Kind != nil {
		kindFilter = *params.Kind
	}

	// since is part of the SQL WHERE clause: filtering after the LIMIT would
	// shorten the page and leave total counting rows the caller excluded. It
	// binds to Since, the exclusive bound, because a poller passes the
	// timestamp of the newest event it holds and must not be handed it again.
	filters := storage.EventFilters{
		Camera: cameraFilter,
		Label:  labelFilter,
		Zone:   zoneFilter,
		Object: objectFilter,
		Kind:   kindFilter,
		Since:  sinceTime,
	}
	events, err := s.db.QueryEventsFiltered(filters, limit, offset)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	if events == nil {
		events = []camera.Event{}
	}

	total, err := s.db.CountEventsFiltered(filters)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	hasMore := offset+len(events) < total

	writeJSON(w, http.StatusOK, map[string]any{
		"items":    events,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"has_more": hasMore,
	})
}

func (s *Server) GetEvent(w http.ResponseWriter, r *http.Request, id string) {
	event, err := s.db.GetEventByID(id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if event == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "event not found"})
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (s *Server) GetEventSnapshot(w http.ResponseWriter, r *http.Request, id string, params GetEventSnapshotParams) {
	event, err := s.db.GetEventByID(id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if event == nil || event.SnapshotPath == "" || !event.SnapshotAvailable {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "snapshot not found"})
		return
	}

	// Serve raw file for downloads or if ?raw=1
	if params.Download != nil || params.Raw != nil {
		filename := safepath.FileComponent(event.ID+"_"+event.Label) + ".jpg"
		if params.Download != nil {
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		}
		http.ServeFile(w, r, event.SnapshotPath)
		return
	}

	// Draw bounding box on-the-fly
	img, err := loadSnapshotImage(event.SnapshotPath)
	if err != nil {
		http.ServeFile(w, r, event.SnapshotPath)
		return
	}

	label := event.Label
	if event.SubLabel != "" {
		label = event.SubLabel
	}
	det := detect.Detection{
		Label: label,
		Score: event.Score,
		Box:   event.Box,
	}
	snapshot.DrawDetectionsInPlace(img, []detect.Detection{det})

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	jpeg.Encode(w, img, &jpeg.Options{Quality: 85})
}

func (s *Server) GetEventClip(w http.ResponseWriter, r *http.Request, id string) {
	event, err := s.db.GetEventByID(id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if event == nil || event.ClipPath == "" || !event.ClipAvailable {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "clip not found"})
		return
	}
	// Only set Content-Disposition: attachment when the client explicitly
	// asks for a download (?download=1). The default is inline playback so
	// a <video src="/api/events/<id>/clip"> element on the event detail
	// page can actually render the clip — a hardcoded attachment header
	// makes the browser treat the response as a file download and the
	// video element stays black on iOS Safari.
	if r.URL.Query().Get("download") == "1" {
		filename := safepath.FileComponent(event.ID+"_"+event.Label) + ".mp4"
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	}
	http.ServeFile(w, r, event.ClipPath)
}

func (s *Server) GetEventDetectionCrop(w http.ResponseWriter, r *http.Request, id string) {
	event, err := s.db.GetEventByID(id)
	if err != nil || event == nil || !event.SnapshotAvailable {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "snapshot not found"})
		return
	}

	img, err := loadSnapshotImage(event.SnapshotPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load snapshot"})
		return
	}

	// Crop to the event's bounding box with padding
	bounds := img.Bounds()
	box := event.Box
	pad := 20
	x1 := max(box[0]-pad, bounds.Min.X)
	y1 := max(box[1]-pad, bounds.Min.Y)
	x2 := min(box[2]+pad, bounds.Max.X)
	y2 := min(box[3]+pad, bounds.Max.Y)

	if x2 <= x1 || y2 <= y1 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "invalid bounding box"})
		return
	}

	crop := img.SubImage(image.Rect(x1, y1, x2, y2))
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	jpeg.Encode(w, crop, &jpeg.Options{Quality: 90})
}

func (s *Server) ReextractClip(w http.ResponseWriter, r *http.Request, id string) {
	event, err := s.db.GetEventByID(id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if event == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "event not found"})
		return
	}
	if err := s.recorder.ReextractClip(r.Context(), *event); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "event": id})
}

// GetEventCounts reports event totals. A count that cannot be read is an error,
// never a zero: "the database is unreachable" and "nothing happened" must not
// render the same.
func (s *Server) GetEventCounts(w http.ResponseWriter, r *http.Request, params GetEventCountsParams) {
	kind := ""
	if params.Kind != nil {
		kind = *params.Kind
	}
	total, err := s.db.CountEvents(kind)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	today, err := s.db.CountEventsToday(kind)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	byLabel, err := s.db.CountEventsByLabel(kind)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	byCamera, err := s.db.CountEventsByCamera(kind)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total":     total,
		"today":     today,
		"by_label":  byLabel,
		"by_camera": byCamera,
	})
}

func (s *Server) GetEventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 32)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()

	defer func() {
		s.sseMu.Lock()
		delete(s.sseClients, ch)
		s.sseMu.Unlock()
	}()

	// Send initial keepalive
	fmt.Fprintf(w, ": keepalive\n\n")
	flusher.Flush()

	ctx := r.Context()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			w.Write(msg)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) broadcastSSE(eventType string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, payload)
	msgBytes := []byte(msg)

	s.sseMu.Lock()
	defer s.sseMu.Unlock()

	for ch := range s.sseClients {
		select {
		case ch <- msgBytes:
		default:
			// Drop message if client buffer is full
		}
	}
}

func (s *Server) IdentifyEvent(w http.ResponseWriter, r *http.Request, id string) {
	if s.objectEmbedder == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "object re-identification not available"})
		return
	}

	eventID := id
	event, err := s.db.GetEventByID(eventID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if event == nil || !event.SnapshotAvailable {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "event snapshot not found"})
		return
	}

	img, err := loadSnapshotImage(event.SnapshotPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load snapshot"})
		return
	}

	embedding, err := s.objectEmbedder.Embed(img, event.Box)
	if err != nil {
		s.serverErrorMsg(w, r, err, "embedding failed")
		return
	}

	knownObjects, err := s.db.ListKnownObjectsByLabel(event.Label)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	candidates := make([]reid.Candidate, 0, len(knownObjects))
	for _, obj := range knownObjects {
		centroid := detect.BytesToFloat32(obj.Centroid)
		if len(centroid) == 0 {
			continue
		}
		candidate := reid.Candidate{ID: obj.ID, Centroid: centroid}
		if obj.MatchThreshold != nil {
			candidate.Threshold = *obj.MatchThreshold
		}
		candidates = append(candidates, candidate)
	}

	var matches []storage.ObjectSighting
	bestID, similarity := reid.BestMatch(embedding, candidates, s.ObjectMatchThreshold)
	for _, obj := range knownObjects {
		if obj.ID == bestID {
			sighting := storage.ObjectSighting{
				EventID:    eventID,
				Camera:     event.CameraName,
				ObjectID:   obj.ID,
				ObjectName: obj.Name,
				Similarity: similarity,
				Timestamp:  event.Timestamp,
			}
			if _, err := s.db.SaveObjectRecognition(sighting); err != nil {
				s.serverError(w, r, err)
				return
			}
			matches = append(matches, sighting)
			break
		}
	}

	if matches == nil {
		matches = []storage.ObjectSighting{}
	}
	writeJSON(w, http.StatusOK, matches)
}

func (s *Server) TrackPerson(w http.ResponseWriter, r *http.Request, id string) {
	eventID := id

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	name, err := normalizeRequiredDisplayName(req.Name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	event, err := s.db.GetEventByID(eventID)
	if err != nil || event == nil || !event.SnapshotAvailable {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "event snapshot not found"})
		return
	}

	img, err := loadSnapshotImage(event.SnapshotPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load snapshot"})
		return
	}

	// Try face detection first
	var faceEmbedding []float32
	var faceCropPath string
	if s.faceRecognizer != nil {
		results := s.faceRecognizer.DetectAndEmbed(img, event.Box, s.faceCropDir)
		if len(results) > 0 {
			faceEmbedding = results[0].Embedding
			faceCropPath = results[0].CropPath
		}
	}

	// Create person record with face centroid if available
	var centroid []byte
	if len(faceEmbedding) > 0 {
		centroid = detect.Float32ToBytes(faceEmbedding)
	}
	personID, err := s.db.SavePersonWithEvent(name, false, centroid, eventID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	method := "none"

	if len(faceEmbedding) > 0 {
		// Save face record
		face := storage.Face{
			EventID:    eventID,
			Camera:     event.CameraName,
			PersonID:   &personID,
			Embedding:  centroid,
			CropPath:   faceCropPath,
			Confidence: 1.0,
			Timestamp:  event.Timestamp,
		}
		sim := 1.0
		face.Similarity = &sim
		s.db.SaveFace(face)
		method = "face"
	} else if s.objectEmbedder != nil {
		// Fall back to body re-ID via OSNet
		bodyEmb, embErr := s.objectEmbedder.Embed(img, event.Box)
		if embErr == nil && len(bodyEmb) > 0 {
			// Create a known_object for body matching
			obj := storage.KnownObject{
				Name:     name,
				Label:    "person",
				Centroid: detect.Float32ToBytes(bodyEmb),
			}
			objID, objErr := s.db.SaveKnownObject(obj)
			if objErr == nil {
				cropDir := filepath.Join(s.snapshotPath, "objects")
				cropPath := s.objectEmbedder.SaveCrop(img, event.Box, cropDir, objID)
				if cropPath != "" {
					_ = s.db.UpdateKnownObjectCrop(objID, cropPath)
				}
				s.db.SaveObjectReference(storage.ObjectReference{
					ObjectID:  objID,
					EventID:   eventID,
					Embedding: detect.Float32ToBytes(bodyEmb),
					CropPath:  cropPath,
				})
				// Also save a sighting for this event
				s.db.SaveObjectSighting(storage.ObjectSighting{
					EventID:    eventID,
					Camera:     event.CameraName,
					ObjectID:   objID,
					Similarity: 1.0,
					Timestamp:  event.Timestamp,
				})
				method = "body"
			}
		}
	}

	// Set sub_label on the event
	_ = s.db.UpdateEventSubLabel(eventID, name)

	slog.Info("person tracked from event", "person_id", personID, "name", name,
		"event", eventID, "method", method)

	writeJSON(w, http.StatusCreated, map[string]any{
		"person_id": personID,
		"name":      name,
		"method":    method,
	})
}

func (s *Server) AssignPersonToEvent(w http.ResponseWriter, r *http.Request, id string) {
	eventID := id

	var req struct {
		PersonID int64 `json:"person_id"`
		Ignore   bool  `json:"ignore"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.Ignore {
		_ = s.db.UpdateEventSubLabel(eventID, "_ignored")
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	if req.PersonID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "person_id is required"})
		return
	}

	person, err := s.db.GetPerson(req.PersonID)
	if err != nil || person == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "person not found"})
		return
	}

	event, err := s.db.GetEventByID(eventID)
	if err != nil || event == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "event not found"})
		return
	}

	// Try face detection if snapshot available
	if event.SnapshotAvailable && event.SnapshotPath != "" && s.faceRecognizer != nil {
		if img, err := loadSnapshotImage(event.SnapshotPath); err == nil {
			results := s.faceRecognizer.DetectAndEmbed(img, event.Box, s.faceCropDir)
			if len(results) > 0 {
				face := storage.Face{
					EventID:    eventID,
					Camera:     event.CameraName,
					PersonID:   &req.PersonID,
					Embedding:  detect.Float32ToBytes(results[0].Embedding),
					CropPath:   results[0].CropPath,
					Confidence: float64(results[0].Confidence),
					Timestamp:  event.Timestamp,
				}
				sim := 1.0
				face.Similarity = &sim
				s.db.SaveFace(face)
				s.updatePersonCentroid(req.PersonID, results[0].Embedding)
			}
		}
	}

	_ = s.db.UpdateEventSubLabel(eventID, person.Name)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "assigned",
		"person_id": req.PersonID,
		"name":      person.Name,
	})
}

// loadSnapshotImage loads a JPEG snapshot and returns it as *image.RGBA.
func loadSnapshotImage(path string) (*image.RGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	img, err := jpeg.Decode(f)
	if err != nil {
		return nil, err
	}

	rgba, ok := img.(*image.RGBA)
	if !ok {
		bounds := img.Bounds()
		rgba = image.NewRGBA(bounds)
		draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)
	}
	return rgba, nil
}

// matchFaceToPerson finds the best matching person for a face embedding.
func (s *Server) matchFaceToPerson(embedding []float32) (int64, float64) {
	if s.faceRecognizer == nil {
		return 0, 0
	}
	people, err := s.db.ListPeople()
	if err != nil {
		return 0, 0
	}

	candidates := make([]reid.Candidate, len(people))
	for i, p := range people {
		candidates[i] = reid.Candidate{
			ID:       p.ID,
			Centroid: detect.BytesToFloat32(p.Centroid),
			Ignore:   p.Ignore,
		}
	}
	return reid.BestMatch(embedding, candidates, s.faceRecognizer.MatchThreshold())
}

// updatePersonCentroid updates a person's centroid with a running average.
func (s *Server) updatePersonCentroid(personID int64, newEmbedding []float32) {
	p, err := s.db.GetPerson(personID)
	if err != nil || p == nil {
		return
	}

	old := detect.BytesToFloat32(p.Centroid)
	merged := reid.BlendCentroid(old, newEmbedding, reid.PersonCentroidUpdateWeight)
	_ = s.db.UpdatePersonCentroid(personID, detect.Float32ToBytes(merged))
}
