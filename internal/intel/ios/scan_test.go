package ios

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSwift writes content into dir/rel, creating parent directories.
func writeSwift(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanSwiftEntities(t *testing.T) {
	dir := t.TempDir()
	writeSwift(t, dir, "Models/User.swift", `import Foundation

struct User: Codable, Identifiable {
    @Attribute(.primaryKey)
    var id: String
    let name: String?
    let email: String
}

struct Post: Decodable {
    let postID: Int
    let title: String?
}
`)

	ents, eps, err := ScanSwift(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 0 {
		t.Errorf("endpoints = %d, want 0: %+v", len(eps), eps)
	}
	if len(ents) != 5 {
		t.Fatalf("entities = %d, want 5: %+v", len(ents), ents)
	}

	want := []struct {
		entity, col, typ  string
		nullable, primary bool
		line              int
	}{
		{"User", "id", "String", false, true, 5},
		{"User", "name", "String", true, false, 6},
		{"User", "email", "String", false, false, 7},
		{"Post", "postID", "Int", false, false, 11},
		{"Post", "title", "String", true, false, 12},
	}
	for i, w := range want {
		e := ents[i]
		if e.Entity != w.entity || e.TableName != "" || e.ColumnName != w.col ||
			e.FieldType != w.typ || e.Nullable != w.nullable || e.IsPrimary != w.primary {
			t.Errorf("entity[%d] = {entity:%s table:%s col:%s type:%s nullable:%v primary:%v}, "+
				"want {entity:%s col:%s type:%s nullable:%v primary:%v}",
				i, e.Entity, e.TableName, e.ColumnName, e.FieldType, e.Nullable, e.IsPrimary,
				w.entity, w.col, w.typ, w.nullable, w.primary)
		}
		if e.SourceLine != w.line {
			t.Errorf("entity[%d] SourceLine = %d, want %d", i, e.SourceLine, w.line)
		}
		if e.SourceFile != "Models/User.swift" {
			t.Errorf("entity[%d] SourceFile = %q, want %q", i, e.SourceFile, "Models/User.swift")
		}
	}
}

func TestScanSwiftEntitiesIDIsPrimary(t *testing.T) {
	dir := t.TempDir()
	writeSwift(t, dir, "Models/Item.swift", `import Foundation

struct Item: Codable {
    let id: UUID
    let sku: String
}
`)

	ents, _, err := ScanSwift(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("entities = %d, want 2: %+v", len(ents), ents)
	}
	if !ents[0].IsPrimary {
		t.Errorf("entity[0] = %+v, want id marked primary", ents[0])
	}
	if ents[1].IsPrimary {
		t.Errorf("entity[1] = %+v, want sku not primary", ents[1])
	}
}

func TestScanSwiftEndpoints(t *testing.T) {
	dir := t.TempDir()
	writeSwift(t, dir, "Networking/APIClient.swift", `import Foundation

class APIClient {
    func fetchUsers() {
        let url = URL(string: "https://api.example.com/v1/users")!
        let task = URLSession.shared.dataTask(with: url)
        task.resume()
    }

    func createUser() {
        var request = URLRequest(url: URL(string: "https://api.example.com/v1/users")!)
        request.httpMethod = "POST"
        URLSession.shared.dataTask(with: request).resume()
    }

    func fetchPost() {
        let url = URL(string: "https://api.example.com/v1/posts/42")!
        URLSession.shared.dataTask(with: url).resume()
    }

    func duplicateFetch() {
        let url = URL(string: "https://api.example.com/v1/users")!
        URLSession.shared.dataTask(with: url).resume()
    }
}
`)

	ents, eps, err := ScanSwift(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("entities = %d, want 0: %+v", len(ents), ents)
	}
	if len(eps) != 3 {
		t.Fatalf("endpoints = %d, want 3 after (method, path) dedup: %+v", len(eps), eps)
	}

	want := []struct {
		method, path string
		line         int
	}{
		{"GET", "/v1/users", 5},
		{"POST", "/v1/users", 11},
		{"GET", "/v1/posts/42", 17},
	}
	for i, w := range want {
		ep := eps[i]
		if ep.Method != w.method || ep.Path != w.path {
			t.Errorf("endpoint[%d] = %s %s, want %s %s", i, ep.Method, ep.Path, w.method, w.path)
		}
		if ep.SourceLine != w.line {
			t.Errorf("endpoint[%d] SourceLine = %d, want %d", i, ep.SourceLine, w.line)
		}
		if ep.SourceFile != "Networking/APIClient.swift" {
			t.Errorf("endpoint[%d] SourceFile = %q, want %q", i, ep.SourceFile, "Networking/APIClient.swift")
		}
	}
}

func TestScanSwiftSkippedDirs(t *testing.T) {
	dir := t.TempDir()
	writeSwift(t, dir, "Models/Profile.swift", `struct Profile: Codable {
    let name: String?
}
`)
	// Generated/vendored output must be skipped.
	writeSwift(t, dir, "Pods/APIClient/Pods.swift", `struct PodGhost: Codable {
    let id: Int
}
`)
	writeSwift(t, dir, "DerivedData/Sources/DD.swift", `struct DDGhost: Codable {
    let id: Int
}
`)
	writeSwift(t, dir, "build/Generated.swift", `struct BuildGhost: Codable {
    let id: Int
}
`)
	writeSwift(t, dir, ".build/checkouts/SPMGhost.swift", `struct SPMGhost: Codable {
    let id: Int
}
`)
	writeSwift(t, dir, "node_modules/pk/NodeGhost.swift", `struct NodeGhost: Codable {
    let id: Int
}
`)
	writeSwift(t, dir, ".git/hooks/GitGhost.swift", `struct GitGhost: Codable {
    let id: Int
}
`)

	ents, eps, err := ScanSwift(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 0 {
		t.Errorf("endpoints = %d, want 0: %+v", len(eps), eps)
	}
	if len(ents) != 1 {
		t.Fatalf("entities = %d, want 1 (generated/vendored dirs skipped): %+v", len(ents), ents)
	}
	if ents[0].Entity != "Profile" || ents[0].ColumnName != "name" || !ents[0].Nullable {
		t.Errorf("entity[0] = %+v, want Profile/name/nullable", ents[0])
	}
	if ents[0].SourceFile != "Models/Profile.swift" {
		t.Errorf("entity[0] SourceFile = %q, want %q", ents[0].SourceFile, "Models/Profile.swift")
	}
}
