package android

import (
	"os"
	"path/filepath"
	"testing"
)

// writeKt writes content into dir/rel, creating parent directories.
func writeKt(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanKotlinEntityWithTableAndPrimaryKey(t *testing.T) {
	dir := t.TempDir()
	writeKt(t, dir, "src/main/java/com/example/data/User.kt", `package com.example.data

@Entity
@Table(name = "users")
data class User(
    @PrimaryKey val id: Long,
    val name: String?,
    val age: Int
)
`)

	ents, eps, err := ScanKotlin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 0 {
		t.Errorf("endpoints = %d, want 0: %+v", len(eps), eps)
	}
	if len(ents) != 3 {
		t.Fatalf("entities = %d, want 3: %+v", len(ents), ents)
	}

	want := []struct {
		entity, table, col, typ string
		nullable, primary       bool
		line                    int
	}{
		{"User", "users", "id", "Long", false, true, 6},
		{"User", "users", "name", "String", true, false, 7},
		{"User", "users", "age", "Int", false, false, 8},
	}
	for i, w := range want {
		e := ents[i]
		if e.Entity != w.entity || e.TableName != w.table || e.ColumnName != w.col ||
			e.FieldType != w.typ || e.Nullable != w.nullable || e.IsPrimary != w.primary {
			t.Errorf("entity[%d] = {entity:%s table:%s col:%s type:%s nullable:%v primary:%v}, "+
				"want {entity:%s table:%s col:%s type:%s nullable:%v primary:%v}",
				i, e.Entity, e.TableName, e.ColumnName, e.FieldType, e.Nullable, e.IsPrimary,
				w.entity, w.table, w.col, w.typ, w.nullable, w.primary)
		}
		if e.SourceLine != w.line {
			t.Errorf("entity[%d] SourceLine = %d, want %d", i, e.SourceLine, w.line)
		}
		if e.SourceFile != "src/main/java/com/example/data/User.kt" {
			t.Errorf("entity[%d] SourceFile = %q, want %q", i, e.SourceFile, "src/main/java/com/example/data/User.kt")
		}
	}
}

func TestScanKotlinDataClassAndSkippedDirs(t *testing.T) {
	dir := t.TempDir()
	writeKt(t, dir, "src/Profile.kt", `package com.example.dto

data class Profile(
    val nickname: String?,
    val email: String
)
`)
	// build/ and .gradle/ are generated output and must be skipped.
	writeKt(t, dir, "build/Generated.kt", `package generated

@Entity
data class Ghost(val id: Long)
`)
	writeKt(t, dir, ".gradle/cache/Foo.kt", `package cached

data class Cached(val id: Long)
`)

	ents, eps, err := ScanKotlin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 0 {
		t.Errorf("endpoints = %d, want 0: %+v", len(eps), eps)
	}
	if len(ents) != 2 {
		t.Fatalf("entities = %d, want 2 (build/.gradle skipped): %+v", len(ents), ents)
	}

	p0 := ents[0]
	if p0.Entity != "Profile" || p0.TableName != "" {
		t.Errorf("entity[0] = %+v, want Profile with empty table name", p0)
	}
	if p0.ColumnName != "nickname" || p0.FieldType != "String" || !p0.Nullable {
		t.Errorf("entity[0] = %+v, want nickname/String/nullable", p0)
	}
	p1 := ents[1]
	if p1.Entity != "Profile" || p1.ColumnName != "email" || p1.FieldType != "String" || p1.Nullable {
		t.Errorf("entity[1] = %+v, want email/String/not-nullable", p1)
	}
}

func TestScanKotlinRetrofitEndpoints(t *testing.T) {
	dir := t.TempDir()
	writeKt(t, dir, "api/UserApi.kt", `package com.example.api

import retrofit2.http.GET
import retrofit2.http.POST

interface UserApi {
    @GET("api/users")
    suspend fun listUsers(): List<User>

    @GET("api/users")
    suspend fun samePath(): List<User>

    @POST("api/users")
    suspend fun create(body: User): User
}
`)

	ents, eps, err := ScanKotlin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("entities = %d, want 0: %+v", len(ents), ents)
	}
	if len(eps) != 2 {
		t.Fatalf("endpoints = %d, want 2 after (method, path) dedup: %+v", len(eps), eps)
	}

	got := eps[0]
	if got.Method != "GET" || got.Path != "api/users" {
		t.Errorf("endpoint[0] = %+v, want GET api/users", got)
	}
	if got.SourceLine != 7 {
		t.Errorf("endpoint[0] SourceLine = %d, want 7", got.SourceLine)
	}
	got = eps[1]
	if got.Method != "POST" || got.Path != "api/users" {
		t.Errorf("endpoint[1] = %+v, want POST api/users", got)
	}
	if got.SourceLine != 13 {
		t.Errorf("endpoint[1] SourceLine = %d, want 13", got.SourceLine)
	}
	for i, e := range eps {
		if e.SourceFile != "api/UserApi.kt" {
			t.Errorf("endpoint[%d] SourceFile = %q, want %q", i, e.SourceFile, "api/UserApi.kt")
		}
	}
}

func TestScanKotlinSameLineAnnotationAndBareSkipped(t *testing.T) {
	dir := t.TempDir()
	writeKt(t, dir, "api/PingApi.kt", `package com.example.api

interface PingApi {
    @GET("api/ping") suspend fun ping(): String
    @DELETE
    suspend fun noPath(): String
}
`)

	ents, eps, err := ScanKotlin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("entities = %d, want 0: %+v", len(ents), ents)
	}
	if len(eps) != 1 {
		t.Fatalf("endpoints = %d, want 1 (bare @DELETE skipped): %+v", len(eps), eps)
	}
	got := eps[0]
	if got.Method != "GET" || got.Path != "api/ping" {
		t.Errorf("endpoint = %+v, want GET api/ping", got)
	}
	if got.SourceLine != 4 {
		t.Errorf("endpoint SourceLine = %d, want 4", got.SourceLine)
	}
}
