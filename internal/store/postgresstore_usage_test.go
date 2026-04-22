package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func TestUsageDetailRecordsRoundTrip(t *testing.T) {
	ts1 := time.Date(2026, 3, 18, 9, 30, 0, 0, time.UTC)
	ts2 := time.Date(2026, 3, 18, 10, 15, 0, 0, time.UTC)
	snapshot := usage.StatisticsSnapshot{
		TotalRequests: 2,
		SuccessCount:  1,
		FailureCount:  1,
		TotalTokens:   42,
		APIs: map[string]usage.APISnapshot{
			"/v1/chat/completions": {
				TotalRequests: 2,
				TotalTokens:   42,
				Models: map[string]usage.ModelSnapshot{
					"gpt-5": {
						TotalRequests: 2,
						TotalTokens:   42,
						Details: []usage.RequestDetail{
							{
								Timestamp: ts1,
								Source:    "openai",
								AuthIndex: "0",
								Failed:    false,
								Tokens: usage.TokenStats{
									InputTokens:  10,
									OutputTokens: 5,
									TotalTokens:  15,
								},
							},
							{
								Timestamp: ts2,
								Source:    "openai",
								AuthIndex: "1",
								Failed:    true,
								Tokens: usage.TokenStats{
									InputTokens:     20,
									OutputTokens:    3,
									ReasoningTokens: 4,
									TotalTokens:     27,
								},
							},
						},
					},
				},
			},
		},
		RequestsByDay: map[string]int64{"2026-03-18": 2},
		RequestsByHour: map[string]int64{
			"09": 1,
			"10": 1,
		},
		TokensByDay: map[string]int64{"2026-03-18": 42},
		TokensByHour: map[string]int64{
			"09": 15,
			"10": 27,
		},
	}

	records := usageDetailRecordsFromSnapshot(snapshot)
	if got := len(records); got != 2 {
		t.Fatalf("usageDetailRecordsFromSnapshot() len = %d, want 2", got)
	}

	rebuilt := usageSnapshotFromDetailRecords(records)
	if rebuilt.TotalRequests != snapshot.TotalRequests {
		t.Fatalf("TotalRequests = %d, want %d", rebuilt.TotalRequests, snapshot.TotalRequests)
	}
	if rebuilt.SuccessCount != snapshot.SuccessCount {
		t.Fatalf("SuccessCount = %d, want %d", rebuilt.SuccessCount, snapshot.SuccessCount)
	}
	if rebuilt.FailureCount != snapshot.FailureCount {
		t.Fatalf("FailureCount = %d, want %d", rebuilt.FailureCount, snapshot.FailureCount)
	}
	if rebuilt.TotalTokens != snapshot.TotalTokens {
		t.Fatalf("TotalTokens = %d, want %d", rebuilt.TotalTokens, snapshot.TotalTokens)
	}

	rebuiltAPI := rebuilt.APIs["/v1/chat/completions"]
	if rebuiltAPI.TotalRequests != 2 {
		t.Fatalf("api TotalRequests = %d, want 2", rebuiltAPI.TotalRequests)
	}
	rebuiltModel := rebuiltAPI.Models["gpt-5"]
	if rebuiltModel.TotalTokens != 42 {
		t.Fatalf("model TotalTokens = %d, want 42", rebuiltModel.TotalTokens)
	}
	if len(rebuiltModel.Details) != 2 {
		t.Fatalf("details len = %d, want 2", len(rebuiltModel.Details))
	}
	if rebuilt.RequestsByHour["09"] != 1 || rebuilt.RequestsByHour["10"] != 1 {
		t.Fatalf("RequestsByHour = %#v, want 09=1 and 10=1", rebuilt.RequestsByHour)
	}
	if rebuilt.TokensByHour["09"] != 15 || rebuilt.TokensByHour["10"] != 27 {
		t.Fatalf("TokensByHour = %#v, want 09=15 and 10=27", rebuilt.TokensByHour)
	}
}

func TestLegacyUsageTableCandidatesDefault(t *testing.T) {
	got := legacyUsageTableCandidates("")
	want := []string{"usage_store", "useage_storage", "usage_storage"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacyUsageTableCandidates(\"\") = %#v, want %#v", got, want)
	}
}

func TestLegacyUsageTableCandidatesTypoFallback(t *testing.T) {
	got := legacyUsageTableCandidates("usage_storage")
	want := []string{"usage_storage", "usage_store", "useage_storage"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacyUsageTableCandidates(\"usage_storage\") = %#v, want %#v", got, want)
	}
}
