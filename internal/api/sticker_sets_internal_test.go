package api

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestStickerSetCatalogIdentitiesAreStableAndCollisionChecked(t *testing.T) {
	first, err := buildStickerSetCatalog()
	if err != nil {
		t.Fatalf("build first catalogue: %v", err)
	}
	second, err := buildStickerSetCatalog()
	if err != nil {
		t.Fatalf("build second catalogue: %v", err)
	}
	firstSet := first.byShortName[twemojiReactionSetShortName]
	secondSet := second.byShortName[twemojiReactionSetShortName]
	if firstSet.set.ID != secondSet.set.ID || firstSet.set.AccessHash != secondSet.set.AccessHash || firstSet.set.Hash != secondSet.set.Hash {
		t.Fatalf("set identity changed across catalogue builds: first=%+v second=%+v", firstSet.set, secondSet.set)
	}
	if len(firstSet.documents) != 73 || len(secondSet.documents) != 73 {
		t.Fatalf("catalogue sizes = %d and %d, want 73", len(firstSet.documents), len(secondSet.documents))
	}
	for i, document := range firstSet.documents {
		other := secondSet.documents[i]
		if document.ID != other.ID || document.AccessHash != other.AccessHash {
			t.Errorf("document %d identity changed across catalogue builds: first=(%d,%d) second=(%d,%d)", i, document.ID, document.AccessHash, other.ID, other.AccessHash)
		}
	}

	reactions, err := getReactionDocumentCatalog()
	if err != nil {
		t.Fatalf("load reaction document catalogue: %v", err)
	}
	duplicate := *firstSet
	duplicate.documents = append(append([]*tg.Document(nil), firstSet.documents...), firstSet.documents[0])
	duplicate.set.Count = len(duplicate.documents)
	if err := validateStickerSetCatalog(&duplicate, reactions); err == nil {
		t.Fatal("catalogue validation accepted a duplicate sticker document identity")
	}
}
