package api

import (
	"encoding/json"
	"errors"
	"image"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"

	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/reid"
	"github.com/rvben/vedetta/internal/storage"
)

// objectEmbedder is the subset of detect.ObjectEmbedder used by HTTP handlers.
// Defined as an interface so tests can substitute a stub.
type objectEmbedder interface {
	Embed(frame *image.RGBA, box [4]int) ([]float32, error)
	SaveCrop(frame *image.RGBA, box [4]int, dir string, objectID int64) string
}

func (s *Server) ListObjects(w http.ResponseWriter, r *http.Request) {
	objects, err := s.db.ListKnownObjects()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if objects == nil {
		objects = []storage.KnownObject{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    objects,
		"total":    len(objects),
		"limit":    len(objects),
		"offset":   0,
		"has_more": false,
	})
}

func (s *Server) CreateObject(w http.ResponseWriter, r *http.Request) {
	if s.objectEmbedder == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "object re-identification not available"})
		return
	}

	var req struct {
		EventID string `json:"event_id"`
		Name    string `json:"name"`
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
	if req.EventID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "event_id and name are required"})
		return
	}

	event, err := s.db.GetEventByID(req.EventID)
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

	obj := storage.KnownObject{
		Name:     name,
		Label:    event.Label,
		Centroid: detect.Float32ToBytes(embedding),
	}
	objID, err := s.db.SaveKnownObject(obj)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	cropDir := filepath.Join(s.snapshotPath, "objects")
	cropPath := s.objectEmbedder.SaveCrop(img, event.Box, cropDir, objID)
	if cropPath != "" {
		_ = s.db.UpdateKnownObjectCrop(objID, cropPath)
	}

	// Save as first reference
	s.db.SaveObjectReference(storage.ObjectReference{
		ObjectID:  objID,
		EventID:   req.EventID,
		Embedding: detect.Float32ToBytes(embedding),
		CropPath:  cropPath,
	})

	// Background re-match: tag recent events with this new object
	s.scheduleObjectRematch(objID)

	obj.ID = objID
	obj.CropPath = cropPath
	writeJSON(w, http.StatusCreated, obj)
}

// CreateObjectFromCameraTrack names a live tracked object directly from the
// camera overlay. The request carries a normalized 0..1 box (matching what
// the SSE detection stream emits). The handler grabs the camera's most recent
// decoded frame, computes a pixel-space box from the normalized coords,
// embeds the crop with OSNet, and persists a new KnownObject. It also pushes
// the name back to the camera so the live overlay reflects the change on the
// very next frame, and schedules a re-match of recent unmatched events.
func (s *Server) CreateObjectFromCameraTrack(w http.ResponseWriter, r *http.Request, cameraName string) {
	// Validate the request before any availability checks. Bad JSON, empty
	// name, missing label, or an out-of-range box should always come back as
	// a 400 — even if the embedder happens to be offline.
	var req CreateObjectFromTrackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	name, err := normalizeRequiredDisplayName(req.Name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "label is required"})
		return
	}
	if req.X1 < 0 || req.Y1 < 0 || req.X2 > 1 || req.Y2 > 1 || req.X2 <= req.X1 || req.Y2 <= req.Y1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid normalized box"})
		return
	}

	cam := s.cameras.GetCamera(cameraName)
	if cam == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "camera not found"})
		return
	}

	if s.objectEmbedder == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "object re-identification not available"})
		return
	}

	frame := cam.LiveFrame()
	if frame == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no live frame available"})
		return
	}

	bounds := frame.Bounds()
	fw := float32(bounds.Dx())
	fh := float32(bounds.Dy())
	box := [4]int{
		int(req.X1 * fw),
		int(req.Y1 * fh),
		int(req.X2 * fw),
		int(req.Y2 * fh),
	}

	embedding, err := s.objectEmbedder.Embed(frame, box)
	if err != nil {
		s.serverErrorMsg(w, r, err, "embedding failed")
		return
	}

	obj := storage.KnownObject{
		Name:     name,
		Label:    req.Label,
		Centroid: detect.Float32ToBytes(embedding),
	}
	objID, err := s.db.SaveKnownObject(obj)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	cropDir := filepath.Join(s.snapshotPath, "objects")
	cropPath := s.objectEmbedder.SaveCrop(frame, box, cropDir, objID)
	if cropPath != "" {
		_ = s.db.UpdateKnownObjectCrop(objID, cropPath)
	}

	_, _ = s.db.SaveObjectReference(storage.ObjectReference{
		ObjectID:  objID,
		Embedding: detect.Float32ToBytes(embedding),
		CropPath:  cropPath,
	})

	cam.SetTrackName(req.TrackId, name)
	s.scheduleObjectRematch(objID)

	obj.ID = objID
	obj.CropPath = cropPath
	writeJSON(w, http.StatusCreated, obj)
}

func (s *Server) UpdateObject(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Name           *string  `json:"name"`
		MatchThreshold *float64 `json:"match_threshold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name != nil {
		name, err := normalizeRequiredDisplayName(*req.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.db.UpdateKnownObjectName(id, name); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if req.MatchThreshold != nil {
		if err := s.db.UpdateKnownObjectThreshold(id, req.MatchThreshold); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) DismissSighting(w http.ResponseWriter, r *http.Request, id int64) {
	sighting, err := s.db.GetObjectSighting(id)
	if err != nil || sighting == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sighting not found"})
		return
	}
	// Clear the sub_label and object_name on the event
	if sighting.EventID != "" {
		_ = s.db.UpdateEventObjectName(sighting.EventID, "")
		_ = s.db.UpdateEventSubLabel(sighting.EventID, "")
	}
	if err := s.db.DeleteObjectSighting(id); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
}

func (s *Server) DeleteObject(w http.ResponseWriter, r *http.Request, id int64) {
	obj, err := s.db.GetKnownObject(id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if obj == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "object not found"})
		return
	}

	// Drop the row first. The row is what the UI reads, so if the two steps
	// cannot both succeed, an orphaned crop file costs disk space while a
	// surviving row pointing at a deleted file breaks every page that renders
	// the object.
	if err := s.db.DeleteKnownObject(id); err != nil {
		s.serverError(w, r, err)
		return
	}
	if obj.CropPath != "" {
		// A crop that is already gone is the outcome this asked for. Anything
		// else leaves a file nothing references, which no later pass will
		// collect, so report it rather than failing a delete that has already
		// taken effect.
		if err := os.Remove(obj.CropPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("failed to remove object crop", "object", id, "path", obj.CropPath, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) ListObjectSightings(w http.ResponseWriter, r *http.Request, id int64, params ListObjectSightingsParams) {
	limit := 50
	if params.Limit != nil && *params.Limit > 0 {
		limit = *params.Limit
	}
	sightings, err := s.db.ListObjectSightings(id, limit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if sightings == nil {
		sightings = []storage.ObjectSighting{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    sightings,
		"total":    len(sightings),
		"limit":    limit,
		"offset":   0,
		"has_more": false,
	})
}

func (s *Server) GetObjectCrop(w http.ResponseWriter, r *http.Request, id int64, params GetObjectCropParams) {
	// Serve specific reference crop if ref param is provided
	if params.Ref != nil {
		refs, err := s.db.ListObjectReferences(id)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		refID := *params.Ref
		for _, ref := range refs {
			if ref.ID == refID && ref.CropPath != "" && fileExists(ref.CropPath) {
				http.ServeFile(w, r, ref.CropPath)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "reference crop not found"})
		return
	}

	obj, err := s.db.GetKnownObject(id)
	if err != nil || obj == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "crop not found"})
		return
	}
	if obj.CropPath != "" && fileExists(obj.CropPath) {
		http.ServeFile(w, r, obj.CropPath)
		return
	}

	// Canonical crop file is missing — fall back to the first reference whose
	// crop is still on disk. This keeps thumbnails working when the snapshots
	// volume has been pruned but the database still has the old absolute path.
	if refs, refErr := s.db.ListObjectReferences(id); refErr == nil {
		for _, ref := range refs {
			if ref.CropPath != "" && fileExists(ref.CropPath) {
				http.ServeFile(w, r, ref.CropPath)
				return
			}
		}
	}

	// Last resort: regenerate from the most recent sighting's event snapshot.
	// Walk newest-first; the first event whose snapshot is on disk wins.
	if s.objectEmbedder != nil {
		if newPath := s.regenerateObjectCrop(id); newPath != "" {
			http.ServeFile(w, r, newPath)
			return
		}
	}

	writeJSON(w, http.StatusNotFound, map[string]string{"error": "crop not found"})
}

// regenerateObjectCrop re-cuts a crop for the given object from a recent
// sighting's event snapshot, persists the new path, and returns it. Returns
// "" if no sighting has a usable snapshot on disk.
func (s *Server) regenerateObjectCrop(id int64) string {
	sightings, err := s.db.ListObjectSightings(id, 20)
	if err != nil || len(sightings) == 0 {
		return ""
	}
	cropDir := filepath.Join(s.snapshotPath, "objects")
	for _, sg := range sightings {
		if sg.EventID == "" {
			continue
		}
		event, err := s.db.GetEventByID(sg.EventID)
		if err != nil || event == nil || !event.SnapshotAvailable || event.SnapshotPath == "" {
			continue
		}
		if !fileExists(event.SnapshotPath) {
			continue
		}
		img, err := loadSnapshotImage(event.SnapshotPath)
		if err != nil {
			continue
		}
		newPath := s.objectEmbedder.SaveCrop(img, event.Box, cropDir, id)
		if newPath == "" {
			continue
		}
		if err := s.db.UpdateKnownObjectCrop(id, newPath); err != nil {
			slog.Warn("regenerateObjectCrop: persist crop_path", "object_id", id, "error", err)
		}
		slog.Info("regenerateObjectCrop: healed missing crop",
			"object_id", id, "event_id", sg.EventID, "path", newPath)
		return newPath
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (s *Server) ListObjectReferences(w http.ResponseWriter, r *http.Request, id int64) {
	refs, err := s.db.ListObjectReferences(id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if refs == nil {
		refs = []storage.ObjectReference{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    refs,
		"total":    len(refs),
		"limit":    len(refs),
		"offset":   0,
		"has_more": false,
	})
}

func (s *Server) AddObjectReference(w http.ResponseWriter, r *http.Request, id int64) {
	if s.objectEmbedder == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "object re-identification not available"})
		return
	}

	objectID := id

	obj, err := s.db.GetKnownObject(objectID)
	if err != nil || obj == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "object not found"})
		return
	}

	var req struct {
		EventID string `json:"event_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.EventID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "event_id is required"})
		return
	}

	event, err := s.db.GetEventByID(req.EventID)
	if err != nil || event == nil || !event.SnapshotAvailable {
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

	cropDir := filepath.Join(s.snapshotPath, "objects")
	cropPath := s.objectEmbedder.SaveCrop(img, event.Box, cropDir, objectID)

	refID, err := s.db.SaveObjectReference(storage.ObjectReference{
		ObjectID:  objectID,
		EventID:   req.EventID,
		Embedding: detect.Float32ToBytes(embedding),
		CropPath:  cropPath,
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Recompute centroid from all references
	s.recomputeObjectCentroid(objectID)

	// Background re-match: scan recent unmatched events for this object
	s.scheduleObjectRematch(objectID)

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        refID,
		"object_id": objectID,
		"crop_path": cropPath,
	})
}

// SetObjectThumbnail replaces the avatar of a known object using either an
// event snapshot (re-cuts the bbox) or an existing reference (copies its crop).
func (s *Server) SetObjectThumbnail(w http.ResponseWriter, r *http.Request, id int64) {
	var req SetObjectThumbnailRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	hasEvent := req.EventId != nil && *req.EventId != ""
	hasRef := req.ReferenceId != nil
	if hasEvent == hasRef {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide exactly one of event_id or reference_id"})
		return
	}

	obj, err := s.db.GetKnownObject(id)
	if err != nil || obj == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "object not found"})
		return
	}

	var newPath string
	if hasRef {
		refs, err := s.db.ListObjectReferences(id)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		var match *storage.ObjectReference
		for i := range refs {
			if refs[i].ID == *req.ReferenceId {
				match = &refs[i]
				break
			}
		}
		if match == nil || match.CropPath == "" || !fileExists(match.CropPath) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "reference crop not found"})
			return
		}
		newPath = match.CropPath
	} else {
		if s.objectEmbedder == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "object re-identification not available"})
			return
		}
		event, err := s.db.GetEventByID(*req.EventId)
		if err != nil || event == nil || !event.SnapshotAvailable || event.SnapshotPath == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "event snapshot not found"})
			return
		}
		if !fileExists(event.SnapshotPath) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "event snapshot not on disk"})
			return
		}
		img, err := loadSnapshotImage(event.SnapshotPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load snapshot"})
			return
		}
		cropDir := filepath.Join(s.snapshotPath, "objects")
		newPath = s.objectEmbedder.SaveCrop(img, event.Box, cropDir, id)
		if newPath == "" {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save crop"})
			return
		}
	}

	if err := s.db.UpdateKnownObjectCrop(id, newPath); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, SetObjectThumbnailResponse{
		ObjectId: id,
		CropPath: newPath,
	})
}

func (s *Server) DeleteObjectReference(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.db.DeleteObjectReference(id); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) recomputeObjectCentroid(objectID int64) {
	refs, err := s.db.ListObjectReferences(objectID)
	if err != nil || len(refs) == 0 {
		return
	}

	var centroid []float32
	for _, ref := range refs {
		emb := detect.BytesToFloat32(ref.Embedding)
		if centroid == nil {
			centroid = make([]float32, len(emb))
		}
		for i := range emb {
			centroid[i] += emb[i]
		}
	}

	n := float32(len(refs))
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

	_ = s.db.UpdateKnownObjectCentroid(objectID, detect.Float32ToBytes(centroid))
}

func (s *Server) rematchRecentEvents(objectID int64) {
	if s.objectEmbedder == nil {
		return
	}
	obj, err := s.db.GetKnownObject(objectID)
	if err != nil || obj == nil || len(obj.Centroid) == 0 {
		return
	}

	centroid := detect.BytesToFloat32(obj.Centroid)
	threshold := s.ObjectMatchThreshold
	candidate := reid.Candidate{ID: obj.ID, Centroid: centroid}
	if obj.MatchThreshold != nil {
		candidate.Threshold = *obj.MatchThreshold
	}

	events, err := s.db.RecentUnmatchedEventsByLabel(obj.Label, 200)
	if err != nil {
		slog.Error("rematch: failed to query events", "error", err)
		return
	}

	var matched int
	for _, ev := range events {
		if !ev.SnapshotAvailable || ev.SnapshotPath == "" {
			continue
		}
		img, err := loadSnapshotImage(ev.SnapshotPath)
		if err != nil {
			continue
		}
		embedding, err := s.objectEmbedder.Embed(img, ev.Box)
		if err != nil {
			continue
		}
		matchedID, similarity := reid.BestMatch(embedding, []reid.Candidate{candidate}, threshold)
		if matchedID != 0 {
			_, err = s.db.SaveObjectRecognition(storage.ObjectSighting{
				EventID:    ev.ID,
				Camera:     ev.CameraName,
				ObjectID:   objectID,
				ObjectName: obj.Name,
				Similarity: similarity,
				Timestamp:  ev.Timestamp,
			})
			if err != nil {
				slog.Warn("rematch: failed to save recognition", "event", ev.ID, "object", obj.Name, "error", err)
				continue
			}
			matched++
		}
	}
	if matched > 0 {
		slog.Info("rematch: retroactively tagged events", "object", obj.Name, "matched", matched)
	}
}
