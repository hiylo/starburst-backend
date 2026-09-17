package intel

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTree writes a set of relative files under root for scanner tests.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetectTypeJava(t *testing.T) {
	files := []string{"pom.xml", "src/main/java/x.java"}
	if got := DetectType(files); got != "java" {
		t.Fatalf("DetectType = %q, want java", got)
	}
}

func TestDetectTypeGo(t *testing.T) {
	files := []string{"go.mod", "main.go"}
	if got := DetectType(files); got != "go" {
		t.Fatalf("DetectType = %q, want go", got)
	}
}

func TestDetectModulesMixedRepo(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"pom.xml":                          `<project></project>`,
		"services/order/pom.xml":           `<project></project>`,
		"clients/android/build.gradle.kts": `plugins { id("com.android.application") }`,
		"app/ios/MyApp.xcodeproj/project.pbxproj": ``,
	})
	mods, err := DetectModules(root)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, m := range mods {
		types[m.RelPath] = m.KindType
	}
	if types["."] != "java" {
		t.Errorf("root module type = %q, want java", types["."])
	}
	if types["services/order"] != "java" {
		t.Errorf("services/order type = %q, want java", types["services/order"])
	}
	if types["clients/android"] != "android" {
		t.Errorf("clients/android type = %q, want android", types["clients/android"])
	}
	if types["app/ios"] != "ios" {
		t.Errorf("app/ios type = %q, want ios", types["app/ios"])
	}
}

func TestScanModuleJavaContracts(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"pom.xml": `<project></project>`,
		"src/main/java/demo/UserEntity.java": `package demo;
import javax.persistence.*;
@Entity
@Table(name = "sys_user")
public class UserEntity {
    @Id
    @Column(name = "id", nullable = false)
    private Long id;
    @Column(name = "nickname")
    private String nickname;
}`,
		"src/main/java/demo/UserController.java": `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/users")
public class UserController {
    @GetMapping("/{id}")
    public UserEntity getUser(@PathVariable Long id) {
        return null;
    }
    @PostMapping
    public UserEntity create(@RequestBody UserEntity user) {
        return user;
    }
}`,
	})
	sum, err := ScanModule(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Entities) != 2 {
		t.Fatalf("entities = %d, want 2 (%+v)", len(sum.Entities), sum.Entities)
	}
	var gotTable, gotCol string
	var gotNullable *bool
	for _, e := range sum.Entities {
		if e.Entity == "UserEntity" || e.TableName == "sys_user" {
			gotTable = e.TableName
		}
		if e.ColumnName == "id" {
			gotNullable = &e.Nullable
		}
		if e.ColumnName == "nickname" {
			gotCol = e.ColumnName
		}
	}
	if gotTable != "sys_user" {
		t.Errorf("table = %q, want sys_user", gotTable)
	}
	if gotNullable == nil || *gotNullable {
		t.Errorf("id nullable = %v, want false (primary key)", gotNullable)
	}
	if gotCol != "nickname" {
		t.Errorf("column = %q, want nickname", gotCol)
	}
	if len(sum.Endpoints) < 2 {
		t.Fatalf("endpoints = %d, want >=2", len(sum.Endpoints))
	}
	paths := map[string]string{}
	for _, ep := range sum.Endpoints {
		paths[ep.Method+" "+ep.Path] = ep.SourceFile
	}
	if _, ok := paths["GET /api/users/{id}"]; !ok {
		t.Errorf("missing GET /api/users/{id}, got %v", paths)
	}
	if _, ok := paths["POST /api/users"]; !ok {
		t.Errorf("missing POST /api/users, got %v", paths)
	}
	// Provenance must reference the controller file.
	for _, ep := range sum.Endpoints {
		if ep.SourceFile == "" || ep.SourceLine == 0 {
			t.Errorf("endpoint %s missing provenance (file=%q line=%d)", ep.Path, ep.SourceFile, ep.SourceLine)
		}
	}
}

func TestScanModuleNonJava(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.mod": "module x", "main.go": "package main"})
	sum, err := ScanModule(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Entities) != 0 || len(sum.Endpoints) != 0 {
		t.Fatalf("non-java module should yield no contracts, got %+v", sum)
	}
}

func TestScanModuleInterfaceController(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"pom.xml": `<project></project>`,
		"src/main/java/demo/FriendProvider.java": `package demo;
import org.springframework.cloud.openfeign.FeignClient;
import org.springframework.web.bind.annotation.*;
@FeignClient(name = "friend")
public interface FriendProvider {
    @GetMapping("/friends")
    Object list();
    @DeleteMapping("/friends/{id}")
    Object delete(@PathVariable Long id);
}`,
		"src/main/java/demo/FriendController.java": `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
public class FriendController implements FriendProvider {
    public Object list() { return null; }
    public Object delete(Long id) { return null; }
}`,
		"src/main/java/demo/ShopController.java": `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
public class ShopController implements ShopProvider {
    @GetMapping("/shop")
    public Object direct() { return null; }
}`,
		"src/main/java/demo/ShopProvider.java": `package demo;
import org.springframework.cloud.openfeign.FeignClient;
import org.springframework.web.bind.annotation.*;
@FeignClient(name = "shop")
public interface ShopProvider {
    @GetMapping("/shop")
    Object get();
    @GetMapping("/shop/all")
    Object all();
}`,
	})
	sum, err := ScanModule(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, ep := range sum.Endpoints {
		key := ep.Method + " " + ep.Path
		if got[key] {
			t.Fatalf("duplicate endpoint %s", key)
		}
		got[key] = true
	}
	for _, want := range []string{"GET /friends", "DELETE /friends/{id}", "GET /shop", "GET /shop/all"} {
		if !got[want] {
			t.Errorf("missing interface endpoint %s, got %v", want, got)
		}
	}
}

func TestToSnake(t *testing.T) {
	cases := map[string]string{
		"id":           "id",
		"nickname":     "nickname",
		"realName":     "real_name",
		"userProfileId": "user_profile_id",
	}
	for in, want := range cases {
		if got := toSnake(in); got != want {
			t.Errorf("toSnake(%q) = %q, want %q", in, got, want)
		}
	}
}