package store

import (
	"context"
	"testing"
)

func TestIntelChatLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	chat := &IntelChat{ProjectID: 3, Title: "用户表有哪些字段"}
	if err := st.CreateIntelChat(ctx, chat); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if chat.ID == 0 {
		t.Fatal("chat id not populated")
	}

	if err := st.AddIntelChatMessage(ctx, &IntelChatMessage{ChatID: chat.ID, Role: "user", Content: "问题1"}); err != nil {
		t.Fatalf("add user msg: %v", err)
	}
	if err := st.AddIntelChatMessage(ctx, &IntelChatMessage{ChatID: chat.ID, Role: "assistant", Content: "回答1", SourcesJSON: `[{"title":"user 表"}]`}); err != nil {
		t.Fatalf("add assistant msg: %v", err)
	}

	got, err := st.GetIntelChat(ctx, chat.ID)
	if err != nil {
		t.Fatalf("get chat: %v", err)
	}
	if got.ProjectID != 3 || got.Title != "用户表有哪些字段" {
		t.Fatalf("unexpected chat %+v", got)
	}

	msgs, err := st.ListIntelChatMessages(ctx, chat.ID, 0)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("messages len=%d err=%v", len(msgs), err)
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("unexpected roles %+v", msgs)
	}
	if msgs[1].SourcesJSON == "" {
		t.Fatal("assistant sources not persisted")
	}

	// Limit returns only the most recent message.
	recent, err := st.ListIntelChatMessages(ctx, chat.ID, 1)
	if err != nil || len(recent) != 1 || recent[0].Role != "assistant" {
		t.Fatalf("limit: %+v err=%v", recent, err)
	}

	chats, err := st.ListIntelChats(ctx, 3)
	if err != nil || len(chats) != 1 {
		t.Fatalf("list chats: %+v err=%v", chats, err)
	}

	if err := st.DeleteIntelChat(ctx, chat.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetIntelChat(ctx, chat.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	remaining, _ := st.ListIntelChatMessages(ctx, chat.ID, 0)
	if len(remaining) != 0 {
		t.Fatalf("messages not cascade-deleted: %d", len(remaining))
	}
}
