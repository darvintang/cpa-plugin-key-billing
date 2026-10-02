// Created by Darvin.
package plugin

import (
	"encoding/json"
	"os"
	"testing"
)

// Keep the store entry aligned with the identity used by the host and release files.
func TestRegistryMatchesPlugin(t *testing.T) {
	raw, err := os.ReadFile("../../registry.json")
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		SchemaVersion int `json:"schema_version"`
		Plugins       []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Author      string `json:"author"`
			Version     string `json:"version"`
			Repository  string `json:"repository"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		t.Fatal(err)
	}
	if registry.SchemaVersion != 1 || len(registry.Plugins) != 1 {
		t.Fatalf("expected schema 1 with one plugin: %+v", registry)
	}
	entry := registry.Plugins[0]
	if entry.ID != PluginID || entry.Version != Version || entry.Repository != GitHubRepository {
		t.Fatalf("registry identity differs from plugin: %+v", entry)
	}
	if entry.Name == "" || entry.Description == "" || entry.Author == "" {
		t.Fatalf("missing required registry metadata: %+v", entry)
	}
}
