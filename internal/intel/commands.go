package intel

import (
	"encoding/json"

	"github.com/hiylo/starburst-backend/internal/store"
)

// DefaultCommands returns the default build/test command templates for a
// detected kind type. These are the candidate commands the user whitelists on
// the project detail page (a placeholder set for M1; the full build/test/
// package surface lands with the execution milestone).
func DefaultCommands(kindType string) []string {
	switch kindType {
	case "java":
		return []string{"mvn test", "mvn package", "mvn clean install"}
	case "android":
		return []string{"./gradlew test", "./gradlew assembleDebug"}
	case "go":
		return []string{"go test ./...", "go build ./...", "go vet ./..."}
	case "web", "node":
		return []string{"npm test", "npm run build"}
	case "ios":
		return []string{"xcodebuild test"}
	case "bff":
		return []string{"npm test"}
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
