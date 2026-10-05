package store

import "testing"

func TestFleetAccountCopySourceDoesNotReboxGenerationPerRow(t *testing.T) {
	accountIDs := make([]int64, 128)
	for i := range accountIDs {
		accountIDs[i] = int64(i + 1)
	}
	generation := "00000000000000000000000000000041"
	withGeneration := newFleetAccountCopySource(accountIDs, &generation)
	withoutGeneration := newFleetAccountCopySource(accountIDs, nil)
	var observed int64
	allocations := func(source *fleetAccountCopySource, hasGeneration bool) float64 {
		return testing.AllocsPerRun(3, func() {
			source.index = 0
			for i := 0; source.Next(); i++ {
				values, err := source.Values()
				if err != nil {
					t.Fatal(err)
				}
				if hasGeneration {
					rowGeneration, ok := values[0].(string)
					if !ok || rowGeneration != generation {
						t.Fatal("generation account copy row is incorrect")
					}
					userID, ok := values[1].(int64)
					if !ok || userID != accountIDs[i] {
						t.Fatal("generation account copy row is incorrect")
					}
				} else {
					userID, ok := values[0].(int64)
					if !ok || userID != accountIDs[i] {
						t.Fatal("pending account copy row is incorrect")
					}
				}
				observed += int64(len(values))
			}
		})
	}
	withAllocations := allocations(withGeneration, true)
	withoutAllocations := allocations(withoutGeneration, false)
	if extra := withAllocations - withoutAllocations; extra > 0.1 {
		t.Fatalf("generation adds %.0f allocations for %d rows, want no per-row allocation", extra, len(accountIDs))
	}
	if observed == 0 {
		t.Fatal("copy source rows were not observed")
	}
}
