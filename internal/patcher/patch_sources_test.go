package patcher

import "testing"

func TestLaterPatchesDoNotCopyFilesCreatedByEarlierPatches(t *testing.T) {
	_, sources, outputs, _, _, _, err := loadToolchainPatches("go1.27")
	if err != nil {
		t.Fatal(err)
	}
	const created = "cmd/internal/objabi/garble.go"
	if !outputs[created] {
		t.Fatalf("patch series no longer creates %s", created)
	}
	if sources[created] {
		t.Fatalf("later edits require copying generated %s from stock GOROOT", created)
	}
}
