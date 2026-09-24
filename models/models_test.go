package models

import (
	"testing"

	"github.com/google/uuid"
)

func TestBeforeCreateAssignsID(t *testing.T) {
	var feed Feed
	if err := feed.BeforeCreate(nil); err != nil {
		t.Fatal(err)
	}
	if feed.ID == uuid.Nil {
		t.Error("ID not assigned")
	}

	given := uuid.New()
	episode := Episode{Base: Base{ID: given}}
	if err := episode.BeforeCreate(nil); err != nil {
		t.Fatal(err)
	}
	if episode.ID != given {
		t.Errorf("ID = %s, want the given %s kept", episode.ID, given)
	}
}
