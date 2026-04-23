package pool

import (
	"encoding/json"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

func TestRewriteMetadataAttempt_SetsAttemptUnconditional(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		original    bamlutils.Metadata
		newAttempt  int
		wantAttempt int
	}{
		{
			name:        "first attempt is no-op",
			original:    bamlutils.Metadata{Phase: bamlutils.MetadataPhasePlanned, Attempt: 0, Path: "buildrequest"},
			newAttempt:  0,
			wantAttempt: 0,
		},
		{
			name:        "retry stamps non-zero attempt",
			original:    bamlutils.Metadata{Phase: bamlutils.MetadataPhasePlanned, Attempt: 0, Path: "buildrequest"},
			newAttempt:  2,
			wantAttempt: 2,
		},
		{
			name:        "outcome event also rewritten",
			original:    bamlutils.Metadata{Phase: bamlutils.MetadataPhaseOutcome, Attempt: 0, Path: "buildrequest"},
			newAttempt:  3,
			wantAttempt: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(&tc.original)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			rewritten, err := rewriteMetadataAttempt(data, tc.newAttempt)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			var got bamlutils.Metadata
			if err := json.Unmarshal(rewritten, &got); err != nil {
				t.Fatalf("decode rewritten: %v", err)
			}
			if got.Attempt != tc.wantAttempt {
				t.Errorf("Attempt: got %d, want %d", got.Attempt, tc.wantAttempt)
			}
			// Other fields should be preserved.
			if got.Phase != tc.original.Phase {
				t.Errorf("Phase: got %q, want %q", got.Phase, tc.original.Phase)
			}
			if got.Path != tc.original.Path {
				t.Errorf("Path: got %q, want %q", got.Path, tc.original.Path)
			}
		})
	}
}

func TestMetadataPhase_RecognisesPhases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{name: "planned", data: []byte(`{"phase":"planned"}`), want: "planned"},
		{name: "outcome", data: []byte(`{"phase":"outcome"}`), want: "outcome"},
		{name: "missing", data: []byte(`{}`), want: ""},
		{name: "invalid json", data: []byte(`not json`), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := metadataPhase(tc.data); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
