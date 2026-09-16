package android

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractLayoutDataBindingExpressions(t *testing.T) {
	layout := `<LinearLayout xmlns:android="http://schemas.android.com/apk/res/android"
    xmlns:app="http://schemas.android.com/apk/res-auto">
    <TextView
        android:id="@+id/name"
        android:text="@{user.name}" />
    <TextView
        android:id="@+id/banner"
        android:text="@{viewModel.banner.title}" />
    <TextView
        android:id="@+id/label"
        android:text="@string/app_name" />
</LinearLayout>`

	got := ExtractLayout("activity_main", []byte(layout))
	if len(got) != 2 {
		t.Fatalf("bindings = %d, want 2: %+v", len(got), got)
	}

	want := []Binding{
		{Page: "activity_main", FieldPath: "user.name", Widget: "TextView"},
		{Page: "activity_main", FieldPath: "banner.title", Widget: "TextView"},
	}
	for i, w := range want {
		if got[i].Page != w.Page || got[i].FieldPath != w.FieldPath || got[i].Widget != w.Widget {
			t.Errorf("binding[%d] = %+v, want %+v", i, got[i], w)
		}
		if got[i].Source == "" {
			t.Errorf("binding[%d] missing source provenance", i)
		}
	}
}

func TestExtractLayoutRecyclerViewItemBinding(t *testing.T) {
	layout := `<layout xmlns:android="http://schemas.android.com/apk/res/android"
    xmlns:app="http://schemas.android.com/apk/res-auto">
    <androidx.constraintlayout.widget.ConstraintLayout>
        <ImageView
            android:id="@+id/image"
            app:imageUrl="@{item.image}" />
        <TextView
            android:id="@+id/title"
            android:text="@{item.title}" />
    </androidx.constraintlayout.widget.ConstraintLayout>
</layout>`

	got := ExtractLayout("item_banner", []byte(layout))
	if len(got) != 2 {
		t.Fatalf("bindings = %d, want 2: %+v", len(got), got)
	}

	if got[0].Page != "item_banner" || got[0].FieldPath != "item.image" || got[0].Widget != "ImageView" {
		t.Errorf("binding[0] = %+v, want item_banner/item.image/ImageView", got[0])
	}
	if got[1].Page != "item_banner" || got[1].FieldPath != "item.title" || got[1].Widget != "TextView" {
		t.Errorf("binding[1] = %+v, want item_banner/item.title/TextView", got[1])
	}
}

func TestFieldPathIndexAndMethodCall(t *testing.T) {
	cases := map[string]string{
		"user.name":              "user.name",
		"viewModel.banner.title": "banner.title",
		"bean.user.nickname":     "user.nickname",
		"bannerList[0].title":    "bannerList.title",
		"item.title":             "item.title",
		"user.getName()":         "user",
		"viewModel.count[0]":     "count",
	}
	for in, want := range cases {
		if got := fieldPath(in); got != want {
			t.Errorf("fieldPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractBindingsDirectory(t *testing.T) {
	resDir := t.TempDir()
	layoutDir := filepath.Join(resDir, "layout")
	if err := os.MkdirAll(layoutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"activity_main.xml": `<LinearLayout xmlns:android="http://schemas.android.com/apk/res/android">
    <TextView android:text="@{user.name}" />
</LinearLayout>`,
		"item_banner.xml": `<layout xmlns:android="http://schemas.android.com/apk/res/android">
    <TextView android:text="@{item.title}" />
</layout>`,
		"activity_detail.xml": `<LinearLayout xmlns:android="http://schemas.android.com/apk/res/android">
    <TextView android:text="@{viewModel.detail.summary}" />
</LinearLayout>`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(layoutDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ExtractBindings(resDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("bindings = %d, want 3: %+v", len(got), got)
	}

	want := []Binding{
		{Page: "activity_detail", FieldPath: "detail.summary", Widget: "TextView"},
		{Page: "activity_main", FieldPath: "user.name", Widget: "TextView"},
		{Page: "item_banner", FieldPath: "item.title", Widget: "TextView"},
	}
	for i, w := range want {
		if got[i].Page != w.Page || got[i].FieldPath != w.FieldPath || got[i].Widget != w.Widget {
			t.Errorf("binding[%d] = %+v, want %+v", i, got[i], w)
		}
	}
	if !strings.HasPrefix(got[1].Source, "layout/activity_main.xml:") {
		t.Errorf("source = %q, want layout/activity_main.xml: prefix", got[1].Source)
	}
}
