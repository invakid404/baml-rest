//go:build integration

package mockllm

import "testing"

func TestScenarioStore_GetRequestCountIfExists(t *testing.T) {
	store := NewScenarioStore()
	store.Register(&Scenario{ID: "scenario-a"})

	if _, _, ok := store.GetAndAdvance("scenario-a"); !ok {
		t.Fatal("expected registered scenario to exist")
	}

	count, ok := store.GetRequestCountIfExists("scenario-a")
	if !ok {
		t.Fatal("expected count lookup to succeed for existing scenario")
	}
	if count != 1 {
		t.Fatalf("expected request count 1, got %d", count)
	}

	store.Delete("scenario-a")
	if _, ok := store.GetRequestCountIfExists("scenario-a"); ok {
		t.Fatal("expected count lookup to fail after scenario deletion")
	}
}

func TestScenarioStore_ResetRequestCountIfExists(t *testing.T) {
	store := NewScenarioStore()
	store.Register(&Scenario{ID: "scenario-b"})

	if _, _, ok := store.GetAndAdvance("scenario-b"); !ok {
		t.Fatal("expected registered scenario to exist")
	}
	if !store.ResetRequestCountIfExists("scenario-b") {
		t.Fatal("expected reset to succeed for existing scenario")
	}
	if count := store.GetRequestCount("scenario-b"); count != 0 {
		t.Fatalf("expected reset count 0, got %d", count)
	}

	store.Delete("scenario-b")
	if store.ResetRequestCountIfExists("scenario-b") {
		t.Fatal("expected reset to fail for deleted scenario")
	}
	if _, exists := store.requestCounts["scenario-b"]; exists {
		t.Fatal("expected reset on missing scenario not to recreate request-count entry")
	}
}
