package ios

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFieldPaths(t *testing.T) {
	cases := []struct{ in, want string }{
		{"post.content", "post.content"},
		{"post.coverURL", "post.coverURL"},
		{"viewModel.post.coverURL", "post.coverURL"},
		{"viewModel.activity?.cover", "activity.cover"},
		{"comment.nickname ?? \"匿名用户\"", "comment.nickname"},
		{"friend.isOnline ? \"在线\" : \"离线\"", "friend.isOnline"},
		{"Formatters.formatDate(item.browsedAt)", "item.browsedAt"},
		{"Formatters.formatPrice(record.amount)", "record.amount"},
		{"item.price == \"0.00\" ? \"免费\" : \"x\"", "item.price"},
		{"author", "author"},
		{"\"Echo\"", ""},
	}
	for _, c := range cases {
		got := fieldPaths(c.in)
		var first string
		if len(got) > 0 {
			first = got[0]
		}
		if first != c.want {
			t.Errorf("fieldPaths(%q) = %q, want %q", c.in, first, c.want)
		}
	}
}

func TestStripViewRoot(t *testing.T) {
	cases := []struct{ in, want string }{
		{"viewModel.post.coverURL", "post.coverURL"},
		{"self.name", "name"},
		{"post.coverURL", "post.coverURL"},
	}
	for _, c := range cases {
		if got := stripViewRoot(c.in); got != c.want {
			t.Errorf("stripViewRoot(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInterpolate(t *testing.T) {
	var got []string
	interpolate(`Text("¥\(Formatters.formatPrice(record.amount))"`, func(expr, slot string) {
		for _, fp := range fieldPaths(expr) {
			got = append(got, fp)
		}
	})
	if len(got) != 1 || got[0] != "record.amount" {
		t.Fatalf("interpolate extracted %v, want [record.amount]", got)
	}
}

func TestExtractBindings(t *testing.T) {
	src := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("EchoApp/Views/Home/PostCardView.swift", `import SwiftUI
struct PostCardView: View {
    let post: Post
    var body: some View {
        VStack {
            Text(post.content)
            EchoAsyncImage(url: post.coverURL)
            if post.type == 2 {
                Text("视频")
            }
        }
    }
}`)

	bindings, err := ExtractBindings(src)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, b := range bindings {
		got[b.FieldPath] = b.Slot
	}
	if got["post.content"] != "text" {
		t.Errorf("post.content slot = %q, want text", got["post.content"])
	}
	if got["post.coverURL"] != "image" {
		t.Errorf("post.coverURL slot = %q, want image", got["post.coverURL"])
	}
	if _, ok := got["post.type"]; ok {
		t.Errorf("post.type should not be extracted (condition, not display)")
	}
	if len(bindings) == 0 {
		t.Fatal("no bindings extracted")
	}
}
