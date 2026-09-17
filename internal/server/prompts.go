package server

import "embed"

//go:embed prompts/*.prompt
var promptFS embed.FS

// loadPrompt returns a versionable prompt template from prompts/ by name. A
// missing file falls back to the supplied default so runtime never breaks on a
// packaging slip (prompt files are part of the binary).
func loadPrompt(name, fallback string) string {
	data, err := promptFS.ReadFile("prompts/" + name + ".prompt")
	if err != nil {
		return fallback
	}
	return string(data)
}
