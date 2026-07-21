package flat

import "testing"

func TestColumnNames(t *testing.T) {
	want := []string{
		"default", "Metadata", "Account", "Storage",
		"StateNodes", "StateTopNodes", "StorageNodes", "FallbackNodes",
	}
	if len(ColumnNames) != len(want) {
		t.Fatalf("len(ColumnNames)=%d want %d", len(ColumnNames), len(want))
	}
	for i, n := range want {
		if ColumnNames[i] != n {
			t.Errorf("ColumnNames[%d]=%q want %q", i, ColumnNames[i], n)
		}
	}
}

func TestColumnOrdinals(t *testing.T) {
	// The Column constants must stay aligned with ColumnNames' index order —
	// a writer indexes handles[col] where the handle slice is opened in
	// ColumnNames order (TestColumnNames pins the names themselves).
	if ColDefault != 0 || ColMetadata != 1 || ColAccount != 2 || ColStorage != 3 ||
		ColStateNodes != 4 || ColStateTopNodes != 5 || ColStorageNodes != 6 || ColFallbackNodes != 7 {
		t.Fatalf("Column ordinals drifted from ColumnNames ordering")
	}
}
