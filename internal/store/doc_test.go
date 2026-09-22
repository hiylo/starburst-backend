package store

import (
	"context"
	"errors"
	"testing"
)

func TestDocDocumentCRUD(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	d := &DocDocument{Name: "报价单", DocType: "xlsx", Prompt: "做一张报价表", Skeleton: `{"type":"xlsx"}`}
	if err := st.CreateDocDocument(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d.ID == 0 {
		t.Fatal("id not assigned")
	}

	got, err := st.GetDocDocument(ctx, d.ID)
	if err != nil || got.DocType != "xlsx" || got.Prompt != "做一张报价表" {
		t.Fatalf("get = %+v, %v", got, err)
	}

	d.Status = "ready"
	d.SizeBytes = 4096
	if err := st.UpdateDocDocumentResult(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetDocDocument(ctx, d.ID)
	if err != nil || got.Status != "ready" || got.SizeBytes != 4096 {
		t.Fatalf("updated = %+v, %v", got, err)
	}

	docs, err := st.ListDocDocuments(ctx, 10)
	if err != nil || len(docs) != 1 {
		t.Fatalf("list = %d, %v", len(docs), err)
	}

	if err := st.DeleteDocDocument(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDocDocument(ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted doc should 404, got %v", err)
	}
}
