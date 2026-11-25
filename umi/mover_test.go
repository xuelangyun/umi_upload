package umi

import "testing"

func TestMovedRecordFallbackRekeysAndInvalidatesScanSignature(t *testing.T) {
	rec := &DatasetRecord{
		ID: "old-id",
		Rel: "normal/PRT_XC_room100/20261125/session",
		Type: "normal",
		Source: "/data/umi_normal/PRT_XC_room100/20261125/session",
		Sig: "old-signature",
		Cached: true,
	}
	plan := &MovePlan{
		To: "calibration",
		Target: "/data/umi_calibration/PRT_XC_room100/20261125/session",
	}

	moved := movedRecordFallback(rec, plan)
	if moved == nil {
		t.Fatal("fallback record is nil")
	}
	if moved.ID != relID(moved.Rel) || moved.ID == rec.ID {
		t.Fatalf("fallback ID was not regenerated: old=%q new=%q rel=%q", rec.ID, moved.ID, moved.Rel)
	}
	if moved.Type != "calibration" || moved.Source != plan.Target || moved.Rel != "calibration/PRT_XC_room100/20261125/session" {
		t.Fatalf("fallback location was not updated: %#v", moved)
	}
	if moved.Sig != "" || moved.Cached {
		t.Fatalf("fallback record retained stale scan metadata: sig=%q cached=%v", moved.Sig, moved.Cached)
	}
	if moved.Misplaced != "" {
		t.Fatalf("fallback record retained a stale misplacement warning: %q", moved.Misplaced)
	}
	if rec.ID != "old-id" || rec.Type != "normal" || rec.Sig != "old-signature" {
		t.Fatalf("source record was mutated: %#v", rec)
	}
}
