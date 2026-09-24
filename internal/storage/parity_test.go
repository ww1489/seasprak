package storage_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/agent"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/session/history"
)

func TestMemoryAndJSONLAssignSameEventSequence(t *testing.T) {
	mem, err := memory.Open("s1", history.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	root := t.TempDir()
	disk, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()

	steps := []history.Commit{
		commitWithEvents("c1", "e1"),
		{CommitID: "c2"},
		commitWithEvents("c3", "e2", "e3"),
	}
	var prev uint64
	var memSeq, diskSeq [][]uint64
	for _, step := range steps {
		step.ExpectedPreviousSeq = prev
		expected := history.ExpectedCommit{ExpectedPreviousSeq: prev}
		mr, err := mem.Append(context.Background(), "s1", expected, step)
		if err != nil {
			t.Fatal(err)
		}
		dr, err := disk.Append(context.Background(), "s1", expected, step)
		if err != nil {
			t.Fatal(err)
		}
		memSeq = append(memSeq, append([]uint64(nil), mr.DurableSeq...))
		diskSeq = append(diskSeq, append([]uint64(nil), dr.DurableSeq...))
		prev = mr.CommitSeq
	}
	if !reflect.DeepEqual(memSeq, diskSeq) {
		t.Fatalf("memory %v jsonl %v", memSeq, diskSeq)
	}
	want := [][]uint64{{1}, nil, {2, 3}}
	if !reflect.DeepEqual(memSeq, want) {
		t.Fatalf("sequence = %v, want %v", memSeq, want)
	}
	memLoaded, err := mem.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	diskLoaded, err := disk.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if memLoaded.DurableCursor != 3 || diskLoaded.DurableCursor != 3 {
		t.Fatalf("cursors memory=%d jsonl=%d", memLoaded.DurableCursor, diskLoaded.DurableCursor)
	}
}

func commitWithEvents(commitID string, eventIDs ...string) history.Commit {
	commit := history.Commit{CommitID: commitID}
	for _, id := range eventIDs {
		commit.Events = append(commit.Events, agent.Event{
			SchemaVersion: 1,
			Type:          "message.finalized",
			Scope:         agent.EventScope{SessionID: "s1"},
			EventID:       id,
			Payload:       []byte(`{}`),
		})
	}
	return commit
}
