package intel

import (
	"strings"
	"testing"

	"github.com/hiylo/starburst-backend/internal/store"
)

func TestDefaultCommands(t *testing.T) {
	cases := map[string][]string{
		"java":    {"mvn test", "mvn package", "mvn clean install", "mvn verify", "mvn -o test"},
		"android": {"./gradlew test", "./gradlew testDebugUnitTest", "./gradlew assembleDebug", "./gradlew lint"},
		"go":      {"go test ./...", "go build ./...", "go vet ./...", "go test -race ./..."},
		"web":     {"npm test", "npm run build", "npm run lint", "npm run type-check"},
		"node":    {"npm test", "npm run build", "npm run lint", "npm run type-check"},
		"ios":     {"xcodebuild test", "xcodebuild build"},
		"bff":     {"npm test", "npm run build", "npm run lint"},
		"unknown": {},
	}
	for kind, want := range cases {
		got := DefaultCommands(kind)
		if len(got) != len(want) {
			t.Errorf("DefaultCommands(%q) = %v, want %v", kind, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("DefaultCommands(%q)[%d] = %q, want %q", kind, i, got[i], want[i])
			}
		}
	}
}

func TestProjectCommandsJSONDedup(t *testing.T) {
	mods := []*store.IntelModule{
		{KindType: "java"},
		{KindType: "node"},
		{KindType: "web"},
	}
	got := ProjectCommandsJSON(mods)
	for _, want := range []string{"mvn test", "npm test", "npm run build"} {
		if !strings.Contains(got, want) {
			t.Errorf("ProjectCommandsJSON missing %q: %s", want, got)
		}
	}
	// npm test / npm run build appear once despite web+node overlap.
	if strings.Count(got, "npm test") != 1 {
		t.Errorf("npm test duplicated: %s", got)
	}
}
