package note

import (
	"encoding/json"
	"testing"
)

func TestPersistenceModelCannotBeUsedAsPublicDTO(t *testing.T) {
	encoded, err := json.Marshal(Note{ID: 1, OwnerID: 2, Slug: "private", Title: "private", Content: "private", Revision: 1})
	if err != nil || string(encoded) != "{}" {
		t.Fatal("persistence model accidentally became an API response")
	}
}
