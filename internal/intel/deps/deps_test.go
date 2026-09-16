package deps

import (
	"testing"
)

func TestParsePom(t *testing.T) {
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<project>
  <parent>
    <groupId>com.example</groupId>
    <artifactId>parent</artifactId>
    <version>2.0.0</version>
  </parent>
  <properties>
    <guava.version>31.1-jre</guava.version>
    <lombok.version>1.18.30</lombok.version>
  </properties>
  <dependencyManagement>
    <dependencies>
      <dependency>
        <groupId>com.example</groupId>
        <artifactId>managed-lib</artifactId>
        <version>3.2.1</version>
      </dependency>
      <dependency>
        <groupId>com.example</groupId>
        <artifactId>managed-prop</artifactId>
        <version>${guava.version}</version>
      </dependency>
    </dependencies>
  </dependencyManagement>
  <dependencies>
    <dependency>
      <groupId>com.google.guava</groupId>
      <artifactId>guava</artifactId>
      <version>${guava.version}</version>
      <scope>compile</scope>
    </dependency>
    <dependency>
      <groupId>org.projectlombok</groupId>
      <artifactId>lombok</artifactId>
      <version>${lombok.version}</version>
      <scope>provided</scope>
    </dependency>
    <dependency>
      <groupId>com.example</groupId>
      <artifactId>managed-lib</artifactId>
    </dependency>
    <dependency>
      <groupId>com.example</groupId>
      <artifactId>unknown-prop</artifactId>
      <version>${missing.version}</version>
    </dependency>
  </dependencies>
</project>`)

	want := []Dependency{
		{Ecosystem: "maven", Group: "com.example", Name: "managed-lib", Version: "3.2.1"},
		{Ecosystem: "maven", Group: "com.example", Name: "unknown-prop", Version: "${missing.version}"},
		{Ecosystem: "maven", Group: "com.google.guava", Name: "guava", Version: "31.1-jre", Scope: "compile"},
		{Ecosystem: "maven", Group: "org.projectlombok", Name: "lombok", Version: "1.18.30", Scope: "provided"},
	}

	got, err := ParsePom(data)
	if err != nil {
		t.Fatalf("ParsePom returned error: %v", err)
	}
	assertDeps(t, got, want)
}

func TestParsePomParentVersion(t *testing.T) {
	data := []byte(`<project>
  <parent>
    <groupId>com.example</groupId>
    <artifactId>parent</artifactId>
    <version>9.9.9</version>
  </parent>
  <dependencies>
    <dependency>
      <groupId>com.example</groupId>
      <artifactId>self</artifactId>
      <version>${project.version}</version>
    </dependency>
    <dependency>
      <groupId>com.example</groupId>
      <artifactId>inherited</artifactId>
      <version>${parent.version}</version>
    </dependency>
  </dependencies>
</project>`)

	want := []Dependency{
		{Ecosystem: "maven", Group: "com.example", Name: "inherited", Version: "9.9.9"},
		{Ecosystem: "maven", Group: "com.example", Name: "self", Version: "9.9.9"},
	}

	got, err := ParsePom(data)
	if err != nil {
		t.Fatalf("ParsePom returned error: %v", err)
	}
	assertDeps(t, got, want)
}

func TestParseGoMod(t *testing.T) {
	data := []byte(`module example.com/hello

go 1.21

require (
	github.com/gin-gonic/gin v1.9.1
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/sync
)

require github.com/spf13/cobra v1.8.0
`)

	want := []Dependency{
		{Ecosystem: "go", Name: "github.com/gin-gonic/gin", Version: "v1.9.1"},
		{Ecosystem: "go", Name: "github.com/google/uuid", Version: "v1.6.0"},
		{Ecosystem: "go", Name: "github.com/spf13/cobra", Version: "v1.8.0"},
		{Ecosystem: "go", Name: "golang.org/x/sync"},
	}

	got, err := ParseGoMod(data)
	if err != nil {
		t.Fatalf("ParseGoMod returned error: %v", err)
	}
	assertDeps(t, got, want)
}

func TestParsePackageJSON(t *testing.T) {
	data := []byte(`{
  "name": "app",
  "dependencies": {
    "express": "^4.18.2",
    "lodash": "4.17.21"
  },
  "devDependencies": {
    "jest": "^29.7.0",
    "typescript": "5.4.5"
  }
}`)

	want := []Dependency{
		{Ecosystem: "npm", Name: "express", Version: "^4.18.2", Scope: "runtime"},
		{Ecosystem: "npm", Name: "jest", Version: "^29.7.0", Scope: "test"},
		{Ecosystem: "npm", Name: "lodash", Version: "4.17.21", Scope: "runtime"},
		{Ecosystem: "npm", Name: "typescript", Version: "5.4.5", Scope: "test"},
	}

	got, err := ParsePackageJSON(data)
	if err != nil {
		t.Fatalf("ParsePackageJSON returned error: %v", err)
	}
	assertDeps(t, got, want)
}

func TestParseGradle(t *testing.T) {
	data := []byte(`dependencies {
    implementation 'com.google.guava:guava:31.1-jre'
    compileOnly 'org.projectlombok:lombok:1.18.30'
    runtimeOnly("com.h2database:h2:2.2.224")
    testImplementation 'junit:junit:4.13.2'
    api "org.slf4j:slf4j-api:2.0.9"
}`)

	want := []Dependency{
		{Ecosystem: "gradle", Group: "com.google.guava", Name: "guava", Version: "31.1-jre", Scope: "compile"},
		{Ecosystem: "gradle", Group: "com.h2database", Name: "h2", Version: "2.2.224", Scope: "runtime"},
		{Ecosystem: "gradle", Group: "junit", Name: "junit", Version: "4.13.2", Scope: "test"},
		{Ecosystem: "gradle", Group: "org.projectlombok", Name: "lombok", Version: "1.18.30", Scope: "provided"},
		{Ecosystem: "gradle", Group: "org.slf4j", Name: "slf4j-api", Version: "2.0.9", Scope: "compile"},
	}

	got, err := ParseGradle(data)
	if err != nil {
		t.Fatalf("ParseGradle returned error: %v", err)
	}
	assertDeps(t, got, want)
}

func assertDeps(t *testing.T, got, want []Dependency) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d deps, want %d:\n got: %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dep[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
