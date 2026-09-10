package store

import (
	"encoding/json"
	"testing"
)

func TestNormalizePolicyParallelQuotaAndHourResetCount(t *testing.T) {
	weekly := int64(10_000_000)
	p, err := normalizePolicy(Policy{
		QuotaMode:     "limited",
		TokenLimit:    &weekly,
		IntervalKind:  "week",
		IntervalCount: 9,
		ParallelLimits: json.RawMessage(`[
			{"id":"client-value-is-ignored","tokenLimit":10000,"intervalKind":"hour","intervalCount":3}
		]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.IntervalCount != 1 {
		t.Fatalf("non-hour primary reset count should normalize to 1, got %d", p.IntervalCount)
	}
	var limits []QuotaLimit
	if err := json.Unmarshal(p.ParallelLimits, &limits); err != nil {
		t.Fatal(err)
	}
	if len(limits) != 1 || limits[0].ID != "parallel-1" || limits[0].TokenLimit != 10_000 || limits[0].IntervalKind != "hour" || limits[0].IntervalCount != 3 {
		t.Fatalf("unexpected normalized limits %#v", limits)
	}
}

func TestNormalizePolicyRejectsTooManyParallelLimits(t *testing.T) {
	limit := int64(100)
	rows := make([]QuotaLimit, 9)
	for i := range rows {
		rows[i] = QuotaLimit{TokenLimit: 10, IntervalKind: "hour", IntervalCount: 1}
	}
	raw, _ := json.Marshal(rows)
	_, err := normalizePolicy(Policy{QuotaMode: "limited", TokenLimit: &limit, IntervalKind: "week", ParallelLimits: raw})
	if err == nil {
		t.Fatal("expected parallel limit count validation error")
	}
}

func TestNormalizePolicyKeepsResourceLimitsWhenTokensUnlimited(t *testing.T) {
	p, err := normalizePolicy(Policy{
		QuotaMode:    "unlimited",
		IntervalKind: "month",
		ResourceLimits: json.RawMessage(`{
			"maxUploadsPerHour":12,
			"maxStoredFiles":50,
			"maxStoredBytes":104857600,
			"maxAttachmentsPerMessage":8,
			"maxFileBytes":10485760,
			"maxImageBytes":4194304
		}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var resources ResourceLimits
	if err := json.Unmarshal(p.ResourceLimits, &resources); err != nil {
		t.Fatal(err)
	}
	if resources.MaxUploadsPerHour != 12 || resources.MaxAttachmentsPerMessage != 8 || resources.MaxFileBytes != 10<<20 || resources.MaxImageBytes != 4<<20 {
		t.Fatalf("resource limits must survive unlimited token mode: %#v", resources)
	}
}

func TestNormalizePolicyRejectsUnsafeResourceLimits(t *testing.T) {
	for _, raw := range []string{
		`{"maxFileBytes":26214401}`,
		`{"maxImageBytes":4194305}`,
		`{"maxAttachmentsPerMessage":101}`,
		`{"maxStoredFiles":-1}`,
	} {
		if _, err := normalizePolicy(Policy{QuotaMode: "unlimited", IntervalKind: "month", ResourceLimits: json.RawMessage(raw)}); err == nil {
			t.Fatalf("expected resource validation error for %s", raw)
		}
	}
}
