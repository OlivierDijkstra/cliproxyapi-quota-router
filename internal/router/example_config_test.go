package router

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExampleConfigParses(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Plugins struct {
			Configs map[string]yaml.Node `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	node, ok := doc.Plugins.Configs[PluginName]
	if !ok {
		t.Fatalf("config.example.yaml has no plugins.configs.%s section", PluginName)
	}
	section, err := yaml.Marshal(&node)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(section)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != DefaultConfig() {
		t.Fatalf("example config drifted from defaults:\n got %+v\nwant %+v", cfg, DefaultConfig())
	}
}
