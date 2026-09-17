package intel

import (
	"encoding/json"

	"github.com/hiylo/starburst-backend/internal/store"
)

// DefaultCommands returns the default build/test/package command templates for
// a detected kind type. These are the candidate commands the user whitelists on
// the project detail page. The set covers the common per-tool build, test and
// package surface; entries containing a "test" intent are picked up at run time
// by whitelistedTestCommand (argv-parsed, never shell), everything else stays
// an explicit whitelist candidate for one-click execution.
func DefaultCommands(kindType string) []string {
	switch kindType {
	case "java":
		return []string{
			"mvn test",
			"mvn package",
			"mvn clean install",
			"mvn verify",
			"mvn -o test",
		}
	case "android":
		return []string{
			"./gradlew test",
			"./gradlew testDebugUnitTest",
			"./gradlew assembleDebug",
			"./gradlew lint",
		}
	case "go":
		return []string{
			"go test ./...",
			"go build ./...",
			"go vet ./...",
			"go test -race ./...",
		}
	case "web", "node":
		return []string{
			"npm test",
			"npm run build",
			"npm run lint",
			"npm run type-check",
		}
	case "ios":
		return []string{
			"xcodebuild test",
			"xcodebuild build",
		}
	case "bff":
		return []string{
			"npm test",
			"npm run build",
			"npm run lint",
		}
	default:
		return nil
	}
}

// ProjectCommandsJSON aggregates the default command whitelist across a
// project's detected modules into a deduplicated JSON array, preserving module
// order.
func ProjectCommandsJSON(mods []*store.IntelModule) string {
	seen := make(map[string]bool)
	cmds := make([]string, 0)
	for _, m := range mods {
		for _, c := range DefaultCommands(m.KindType) {
			if seen[c] {
				continue
			}
			seen[c] = true
			cmds = append(cmds, c)
		}
	}
	b, _ := json.Marshal(cmds)
	return string(b)
}
