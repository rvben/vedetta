package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rvben/vedetta/internal/storage"
)

// seedObjectWithCrop stores a known object together with the crop file the row
// points at, which is the state a real object reaches once a reference image
// has been saved.
func seedObjectWithCrop(t *testing.T, db *storage.DB, name string) (int64, string) {
	t.Helper()
	crop := filepath.Join(t.TempDir(), "crop.jpg")
	if err := os.WriteFile(crop, []byte("not really a jpeg"), 0o644); err != nil {
		t.Fatalf("write crop: %v", err)
	}
	id, err := db.SaveKnownObject(storage.KnownObject{Name: name, Label: "car", CropPath: crop})
	if err != nil {
		t.Fatalf("save object: %v", err)
	}
	if err := db.UpdateKnownObjectCrop(id, crop); err != nil {
		t.Fatalf("set crop path: %v", err)
	}
	return id, crop
}

func deleteObject(t *testing.T, srv *Server, id int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/objects/0", nil)
	w := httptest.NewRecorder()
	srv.DeleteObject(w, req, id)
	return w
}

func TestDeleteObjectRemovesTheRowAndTheCrop(t *testing.T) {
	srv, db := newTestServer(t)
	id, crop := seedObjectWithCrop(t, db, "Renault Trafic")

	w := deleteObject(t, srv, id)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	obj, err := db.GetKnownObject(id)
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	if obj != nil {
		t.Fatal("the object row survived the delete")
	}
	if _, err := os.Stat(crop); !os.IsNotExist(err) {
		t.Fatalf("the crop file survived the delete: stat err %v", err)
	}
}

// TestDeleteObjectSucceedsWhenTheCropIsAlreadyGone covers the case a checked
// os.Remove introduces: a missing crop is the state the caller asked for, so it
// must not turn a successful delete into an error.
func TestDeleteObjectSucceedsWhenTheCropIsAlreadyGone(t *testing.T) {
	srv, db := newTestServer(t)
	id, crop := seedObjectWithCrop(t, db, "Renault Trafic")
	if err := os.Remove(crop); err != nil {
		t.Fatalf("remove crop: %v", err)
	}

	w := deleteObject(t, srv, id)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	obj, err := db.GetKnownObject(id)
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	if obj != nil {
		t.Fatal("the object row survived the delete")
	}
}

// TestDeleteObjectKeepsTheCropWhenTheRowCannotBeDeleted pins the ordering. The
// row is what every page reads, so the crop may only be removed once the row is
// gone; removing it first leaves the UI rendering an object whose image 404s.
// A trigger is the injection because it fails only the delete: closing the
// database instead fails the read that precedes it, so the handler returns
// before either step and the test proves nothing.
func TestDeleteObjectKeepsTheCropWhenTheRowCannotBeDeleted(t *testing.T) {
	srv, db := newTestServer(t)
	id, crop := seedObjectWithCrop(t, db, "Renault Trafic")

	if _, err := db.Raw().Exec(`
		CREATE TRIGGER block_object_delete BEFORE DELETE ON known_objects
		BEGIN SELECT RAISE(ABORT, 'delete blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/objects/0", nil)
	w := httptest.NewRecorder()
	srv.DeleteObject(w, req, id)

	if w.Code == http.StatusOK {
		t.Fatalf("delete reported success while the row could not be removed: %s", w.Body.String())
	}
	obj, err := db.GetKnownObject(id)
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	if obj == nil {
		t.Fatal("the trigger did not block the delete, so this cannot observe the ordering")
	}
	if _, err := os.Stat(crop); err != nil {
		t.Fatalf("the crop was removed even though the row survived: %v", err)
	}
}
