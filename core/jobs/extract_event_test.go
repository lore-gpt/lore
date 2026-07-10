package jobs_test

import (
	"context"
	"testing"

	"github.com/riverqueue/river"

	"github.com/lore-gpt/lore/core/jobs"
)

func TestExtractEventArgs_Kind(t *testing.T) {
	if got := (jobs.ExtractEventArgs{}).Kind(); got != "extract_event" {
		t.Errorf("Kind() = %q, want %q", got, "extract_event")
	}
}

func TestExtractEventArgs_MaxAttempts(t *testing.T) {
	if got := (jobs.ExtractEventArgs{}).InsertOpts().MaxAttempts; got != 3 {
		t.Errorf("InsertOpts().MaxAttempts = %d, want 3", got)
	}
}

func TestExtractEventWorker_Work(t *testing.T) {
	w := jobs.NewExtractEventWorker()
	job := &river.Job[jobs.ExtractEventArgs]{Args: jobs.ExtractEventArgs{EventID: "evt-123"}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
}
