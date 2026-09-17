package store

import (
	"context"
	"testing"
)

// TestReplaceIntelFeaturesKeepsStableIDs verifies the feature-point replace is
// idempotent by natural key (source+anchor+name): re-running the same set keeps
// every row's id and summary, manual rows (passed back with their id) update in
// place, a renamed row is matched by id and keeps its id, and a removed row is
// deleted. This keeps intel_overrides/issue/chat references valid across
// re-analyses (人工优先、重扫不覆盖).
func TestReplaceIntelFeaturesKeepsStableIDs(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	feats := []*IntelFeature{
		{Name: "用户列表", EndsJSON: `[{"method":"GET","path":"/users"}]`, SortOrder: 0, Source: "auto", Anchor: "GET /users", Status: "active"},
		{Name: "订单详情", EndsJSON: `[{"method":"GET","path":"/orders/{id}"}]`, SortOrder: 1, Source: "auto", Anchor: "GET /orders/{id}", Status: "active"},
	}
	if err := st.ReplaceIntelFeatures(ctx, 11, feats); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	first, err := st.ListIntelFeatures(ctx, 11)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("want 2 features after first replace, got %d", len(first))
	}
	ids := make(map[string]int64, len(first))
	for _, f := range first {
		if f.ID == 0 {
			t.Fatalf("feature %q has zero id", f.Name)
		}
		ids[f.Name] = f.ID
	}

	// Re-run the same set: ids and summaries must be stable.
	if err := st.ReplaceIntelFeatures(ctx, 11, feats); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	second, err := st.ListIntelFeatures(ctx, 11)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("want 2 features after second replace, got %d", len(second))
	}
	for _, f := range second {
		if f.ID != ids[f.Name] {
			t.Errorf("feature %q id churned: %d -> %d", f.Name, ids[f.Name], f.ID)
		}
	}

	// A manually created feature (id set) must update in place, not churn.
	manual := &IntelFeature{ProjectID: 11, Name: "人工功能点", Summary: "保留的摘要", EndsJSON: `[]`, SortOrder: 9, Source: "manual", Anchor: "", Status: "active"}
	if err := st.CreateIntelFeature(ctx, manual); err != nil {
		t.Fatalf("create manual feature: %v", err)
	}
	manual.Name = "人工功能点（改名）"
	// 只传回 manual 行：auto 行不在集合中应被删除。
	if err := st.ReplaceIntelFeatures(ctx, 11, []*IntelFeature{manual}); err != nil {
		t.Fatalf("replace with manual only: %v", err)
	}
	third, err := st.ListIntelFeatures(ctx, 11)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(third) != 1 {
		t.Fatalf("want exactly the manual feature after replace, got %d", len(third))
	}
	if third[0].ID != manual.ID {
		t.Errorf("manual feature id churned: %d -> %d", manual.ID, third[0].ID)
	}
	if third[0].Name != "人工功能点（改名）" {
		t.Errorf("manual rename not applied: %q", third[0].Name)
	}
	if third[0].Summary != "保留的摘要" {
		t.Errorf("manual summary not preserved: %q", third[0].Summary)
	}
}
