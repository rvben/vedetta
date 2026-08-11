package event

import (
	"fmt"
	"log/slog"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/reid"
	"github.com/rvben/vedetta/internal/storage"
)

func matchFaceToPerson(db *storage.DB, embedding []float32, recognizer FaceRecognizer) (int64, float64) {
	if recognizer == nil {
		return 0, 0
	}
	people, err := db.ListPeople()
	if err != nil {
		slog.Error("failed to list people for face matching", "error", err)
		return 0, 0
	}
	candidates := make([]reid.Candidate, len(people))
	for i, person := range people {
		candidates[i] = reid.Candidate{
			ID:       person.ID,
			Centroid: detect.BytesToFloat32(person.Centroid),
			Ignore:   person.Ignore,
		}
	}
	return reid.BestMatch(embedding, candidates, recognizer.MatchThreshold())
}

func updatePersonCentroid(db *storage.DB, personID int64, embedding []float32) {
	person, err := db.GetPerson(personID)
	if err != nil || person == nil {
		return
	}
	old := detect.BytesToFloat32(person.Centroid)
	merged := reid.BlendCentroid(old, embedding, reid.PersonCentroidUpdateWeight)
	_ = db.UpdatePersonCentroid(personID, detect.Float32ToBytes(merged))
}

func clusterUnmatchedFace(db *storage.DB, newFaceID int64, embedding []float32, cameraName string) {
	unmatched, err := db.ListUnmatchedFaces(200)
	if err != nil || len(unmatched) == 0 {
		return
	}

	candidates := make([]reid.Candidate, 0, len(unmatched)-1)
	for i := range unmatched {
		if unmatched[i].ID == newFaceID {
			continue
		}
		other := detect.BytesToFloat32(unmatched[i].Embedding)
		if len(other) > 0 {
			candidates = append(candidates, reid.Candidate{ID: unmatched[i].ID, Centroid: other})
		}
	}
	bestID, bestSimilarity := reid.BestMatch(embedding, candidates, reid.FaceClusterThreshold)
	if bestID == 0 {
		return
	}
	var bestFace *storage.Face
	for i := range unmatched {
		if unmatched[i].ID == bestID {
			bestFace = &unmatched[i]
			break
		}
	}
	if bestFace == nil {
		return
	}

	centroid := reid.AverageNormalized(embedding, detect.BytesToFloat32(bestFace.Embedding))
	personID, err := db.SavePerson("", false, detect.Float32ToBytes(centroid))
	if err != nil {
		slog.Error("failed to create person from cluster", "error", err)
		return
	}
	_ = db.UpdateFacePerson(bestFace.ID, personID, bestSimilarity)
	_ = db.UpdateFacePerson(newFaceID, personID, 1.0)
	slog.Info("auto-clustered faces into new person", "person_id", personID,
		"similarity", fmt.Sprintf("%.3f", bestSimilarity), "camera", cameraName)
}

func matchEventToKnownObjects(db *storage.DB, embedder ObjectEmbedder, submitted camera.Event, threshold float64) []string {
	knownObjects, err := db.ListKnownObjectsByLabel(submitted.Label)
	if err != nil || len(knownObjects) == 0 {
		return nil
	}
	embedding, err := embedder.Embed(submitted.SnapshotImage, submitted.Box)
	if err != nil {
		slog.Error("object re-ID embed failed", "event", submitted.ID, "error", err)
		return nil
	}

	candidates := make([]reid.Candidate, 0, len(knownObjects))
	for _, object := range knownObjects {
		centroid := detect.BytesToFloat32(object.Centroid)
		if len(centroid) == 0 {
			continue
		}
		candidate := reid.Candidate{ID: object.ID, Centroid: centroid}
		if object.MatchThreshold != nil {
			candidate.Threshold = *object.MatchThreshold
		}
		candidates = append(candidates, candidate)
	}

	bestID, similarity := reid.BestMatch(embedding, candidates, threshold)
	if bestID == 0 {
		return nil
	}
	for _, object := range knownObjects {
		if object.ID != bestID {
			continue
		}
		if _, err := db.SaveObjectRecognition(storage.ObjectSighting{
			EventID:    submitted.ID,
			Camera:     submitted.CameraName,
			ObjectID:   object.ID,
			ObjectName: object.Name,
			Similarity: similarity,
			Timestamp:  submitted.Timestamp,
		}); err != nil {
			slog.Error("failed to save object sighting", "error", err)
			return nil
		}
		slog.Info("object recognized", "object", object.Name, "event", submitted.ID,
			"similarity", fmt.Sprintf("%.3f", similarity))
		return []string{object.Name}
	}
	return nil
}
