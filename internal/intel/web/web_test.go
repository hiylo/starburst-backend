package web

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReduceExpression(t *testing.T) {
	cases := []struct{ in, want string }{
		{"detail.userId", "detail.userId"},
		{"detail.nickname || '-'", "detail.nickname"},
		{"detail.realName.idCardMasked || '-'", "detail.realName.idCardMasked"},
		{"detail.status === 0 ? 'success' : 'danger'", "detail.status"},
		{"formatDate(row.createdAt)", "row.createdAt"},
		{"activeTab", "activeTab"},
		{"genderLabel", "genderLabel"},
		{"r", "r"},
		{"related.posts", "related.posts"},
		{"this.list", "this.list"},
		{"a && b", "a"},
	}
	for _, c := range cases {
		if got := reduceExpression(c.in); got != c.want {
			t.Errorf("reduceExpression(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestVForCollection(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cap in capabilities", "capabilities"},
		{"(item, index) in pendingItems", "pendingItems"},
		{"c of categories", "categories"},
		{"menus", "menus"},
	}
	for _, c := range cases {
		if got := vForCollection(c.in); got != c.want {
			t.Errorf("vForCollection(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExtractBindings(t *testing.T) {
	src := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("views/users/UserDetail.vue", `<template>
  <div>
    <span>{{ detail.userId }}</span>
    <span>{{ detail.nickname || '-' }}</span>
    <input v-model="activeTab" />
    <img :src="detail.avatarUrl" />
    <p v-text="detail.bio"></p>
  </div>
</template>
<script>export default { name: 'UserDetail' }</script>`)
	write("views/users/List.vue", `<template>
  <ul>
    <li v-for="user in users">{{ user.name }}</li>
  </ul>
</template>`)

	bindings, err := ExtractBindings(src)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, b := range bindings {
		got[b.FieldPath] = b.Slot
	}
	want := map[string]string{
		"detail.userId":    "interpolation",
		"detail.nickname":  "interpolation",
		"activeTab":        "v-model",
		"detail.avatarUrl": "v-bind:src",
		"detail.bio":       "v-text",
		"users":            "v-for",
		"user.name":        "interpolation",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d bindings, want %d: %+v", len(got), len(want), bindings)
	}
	for fp, slot := range want {
		if got[fp] != slot {
			t.Errorf("binding %q slot = %q, want %q", fp, got[fp], slot)
		}
	}
}
