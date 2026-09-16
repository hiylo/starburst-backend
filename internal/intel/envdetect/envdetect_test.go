package envdetect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeFiles writes a set of relative files under root for detector tests.
func writeFiles(t *testing.T, root string, files map[string]string) {
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

func TestDetect(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []Requirement
	}{
		{
			name: "pom java jdk mysql redis with property version",
			files: map[string]string{
				"pom.xml": `<project>
  <properties>
    <java.version>17</java.version>
    <mysql.version>8.0.33</mysql.version>
  </properties>
  <dependencies>
    <dependency>
      <groupId>com.mysql</groupId>
      <artifactId>mysql-connector-j</artifactId>
      <version>${mysql.version}</version>
    </dependency>
    <dependency>
      <groupId>org.springframework.boot</groupId>
      <artifactId>spring-boot-starter-data-redis</artifactId>
    </dependency>
  </dependencies>
</project>`,
			},
			want: []Requirement{
				{Service: "jdk", Category: "toolchain", Version: "17", Source: "pom.xml"},
				{Service: "mysql", Category: "middleware", Version: "8.0.33", Source: "pom.xml"},
				{Service: "redis", Category: "middleware", Source: "pom.xml"},
			},
		},
		{
			name: "pom version resolved from parent properties",
			files: map[string]string{
				"pom.xml": `<project>
  <properties>
    <mysql.version>8.0.31</mysql.version>
  </properties>
</project>`,
				"svc/pom.xml": `<project>
  <parent>
    <groupId>org.demo</groupId>
    <artifactId>parent</artifactId>
    <version>1.0</version>
    <relativePath>../pom.xml</relativePath>
  </parent>
  <dependencies>
    <dependency>
      <groupId>com.mysql</groupId>
      <artifactId>mysql-connector-j</artifactId>
      <version>${mysql.version}</version>
    </dependency>
  </dependencies>
</project>`,
			},
			want: []Requirement{
				{Service: "mysql", Category: "middleware", Version: "8.0.31", Source: "svc/pom.xml"},
			},
		},
		{
			name: "gradle android sdk and agp",
			files: map[string]string{
				"build.gradle.kts": `plugins {
    id("com.android.application") version "8.1.0"
}
android {
    compileSdk = 34
    minSdk = 24
}`,
			},
			want: []Requirement{
				{Service: "android-sdk", Category: "toolchain", Version: "34", Source: "build.gradle.kts"},
				{Service: "gradle", Category: "toolchain", Version: "8.1.0", Source: "build.gradle.kts"},
			},
		},
		{
			name: "go mod",
			files: map[string]string{
				"go.mod": "module example.com/foo\n\ngo 1.22\n",
			},
			want: []Requirement{
				{Service: "go", Category: "toolchain", Version: "1.22", Source: "go.mod"},
			},
		},
		{
			name: "package json engines node plus playwright",
			files: map[string]string{
				"package.json": `{
  "name": "web",
  "engines": { "node": ">=20.0.0" },
  "devDependencies": { "@playwright/test": "^1.40.0", "vitest": "^1.0.0" }
}`,
			},
			want: []Requirement{
				{Service: "node", Category: "toolchain", Version: "20.0.0", Source: "package.json"},
			},
		},
		{
			name: "package json jest only no engines",
			files: map[string]string{
				"package.json": `{
  "name": "service",
  "devDependencies": { "jest": "^29.0.0" }
}`,
			},
			want: []Requirement{
				{Service: "node", Category: "toolchain", Source: "package.json"},
			},
		},
		{
			name: "application yml middleware",
			files: map[string]string{
				"application.yml": `spring:
  datasource:
    url: jdbc:mysql://localhost:3306/test
  data:
    redis:
      host: localhost
  cloud:
    nacos:
      discovery:
        server-addr: localhost:8848
  rabbitmq:
    host: localhost
  elasticsearch:
    uris: http://localhost:9200
`,
			},
			want: []Requirement{
				{Service: "elasticsearch", Category: "middleware", Source: "application.yml"},
				{Service: "mysql", Category: "middleware", Source: "application.yml"},
				{Service: "nacos", Category: "middleware", Source: "application.yml"},
				{Service: "rabbitmq", Category: "middleware", Source: "application.yml"},
				{Service: "redis", Category: "middleware", Source: "application.yml"},
			},
		},
		{
			name: "application properties middleware",
			files: map[string]string{
				"application.properties": "spring.datasource.url=jdbc:mysql://localhost:3306/test\n" +
					"spring.redis.host=localhost\n" +
					"spring.cloud.nacos.discovery.server-addr=localhost:8848\n" +
					"spring.rabbitmq.host=localhost\n" +
					"spring.elasticsearch.uris=http://localhost:9200\n",
			},
			want: []Requirement{
				{Service: "elasticsearch", Category: "middleware", Source: "application.properties"},
				{Service: "mysql", Category: "middleware", Source: "application.properties"},
				{Service: "nacos", Category: "middleware", Source: "application.properties"},
				{Service: "rabbitmq", Category: "middleware", Source: "application.properties"},
				{Service: "redis", Category: "middleware", Source: "application.properties"},
			},
		},
		{
			name: "mixed repo dedupe and sorted",
			files: map[string]string{
				"pom.xml": `<project>
  <properties>
    <java.version>17</java.version>
    <mysql.version>8.0.33</mysql.version>
  </properties>
  <dependencies>
    <dependency>
      <groupId>com.mysql</groupId>
      <artifactId>mysql-connector-j</artifactId>
      <version>${mysql.version}</version>
    </dependency>
    <dependency>
      <groupId>org.springframework.boot</groupId>
      <artifactId>spring-boot-starter-data-redis</artifactId>
    </dependency>
  </dependencies>
</project>`,
				"build.gradle.kts": `plugins {
    id("com.android.application") version "8.1.0"
}
android {
    compileSdk = 34
}`,
				"go.mod":       "module example.com/foo\n\ngo 1.22\n",
				"package.json": `{ "engines": { "node": ">=20.0.0" } }`,
				"application.yml": `spring:
  datasource:
    url: jdbc:mysql://localhost:3306/test
  data:
    redis:
      host: localhost
  cloud:
    nacos:
      discovery:
        server-addr: localhost:8848
  rabbitmq:
    host: localhost
  elasticsearch:
    uris: http://localhost:9200
`,
			},
			want: []Requirement{
				{Service: "android-sdk", Category: "toolchain", Version: "34", Source: "build.gradle.kts"},
				{Service: "elasticsearch", Category: "middleware", Source: "application.yml"},
				{Service: "go", Category: "toolchain", Version: "1.22", Source: "go.mod"},
				{Service: "gradle", Category: "toolchain", Version: "8.1.0", Source: "build.gradle.kts"},
				{Service: "jdk", Category: "toolchain", Version: "17", Source: "pom.xml"},
				{Service: "mysql", Category: "middleware", Version: "8.0.33", Source: "pom.xml"},
				{Service: "nacos", Category: "middleware", Source: "application.yml"},
				{Service: "node", Category: "toolchain", Version: "20.0.0", Source: "package.json"},
				{Service: "rabbitmq", Category: "middleware", Source: "application.yml"},
				{Service: "redis", Category: "middleware", Source: "application.yml"},
			},
		},
		{
			name:  "empty repo",
			files: map[string]string{"README.md": "nothing here"},
			want:  []Requirement{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, tt.files)
			got, err := Detect(root, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Detect() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestDetectModuleScope(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"application.yml": "spring:\n  datasource:\n    url: jdbc:mysql://localhost:3306/test\n",
		"svc/pom.xml": `<project>
  <properties><java.version>21</java.version></properties>
  <dependencies>
    <dependency>
      <groupId>org.springframework.boot</groupId>
      <artifactId>spring-boot-starter-data-redis</artifactId>
    </dependency>
  </dependencies>
</project>`,
	})
	got, err := Detect(root, "svc")
	if err != nil {
		t.Fatal(err)
	}
	want := []Requirement{
		{Service: "jdk", Category: "toolchain", Version: "21", Source: "svc/pom.xml"},
		{Service: "redis", Category: "middleware", Source: "svc/pom.xml"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Detect(root, svc) = %#v, want %#v", got, want)
	}
}
