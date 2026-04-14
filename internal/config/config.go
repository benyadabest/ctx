package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Capture    CaptureConfig    `toml:"capture"`
	Detection  DetectionConfig  `toml:"detection"`
	Ranking    RankingConfig    `toml:"ranking"`
	Compile    CompileConfig    `toml:"compile"`
	Serve      ServeConfig      `toml:"serve"`
	API        APIConfig        `toml:"api"`
	Models     ModelsConfig     `toml:"models"`
}

type CaptureConfig struct {
	CursorDir  string `toml:"cursor_dir"`
	ClaudeDir  string `toml:"claude_dir"`
	ContextDir string `toml:"context_dir"`
}

type DetectionConfig struct {
	BatchSize int `toml:"batch_size"`
	MinFreq   int `toml:"min_freq"`
}

type RankingConfig struct {
	Similarity          float64 `toml:"similarity"`
	Frequency           float64 `toml:"frequency"`
	Recency             float64 `toml:"recency"`
	RecencyHalflifeDays int     `toml:"recency_halflife_days"`
}

type CompileConfig struct {
	TopKSkills    int    `toml:"top_k_skills"`
	TopKPatterns  int    `toml:"top_k_patterns"`
	TopKLearnings int    `toml:"top_k_learnings"`
	OutputDir     string `toml:"output_dir"`
}

type ServeConfig struct {
	Port int `toml:"port"`
}

type APIConfig struct {
	AnthropicAPIKey string `toml:"anthropic_api_key"`
	OllamaBaseURL   string `toml:"ollama_base_url"`
}

type ModelsConfig struct {
	Summarize  string `toml:"summarize"`
	Detect     string `toml:"detect"`
	Compile    string `toml:"compile"`
	Synthesize string `toml:"synthesize"`
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func defaults() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		Capture: CaptureConfig{
			CursorDir:  filepath.Join(home, ".cursor", "projects"),
			ClaudeDir:  filepath.Join(home, ".claude", "projects"),
			ContextDir: filepath.Join(home, ".context"),
		},
		Detection: DetectionConfig{
			BatchSize: 20,
			MinFreq:   3,
		},
		Ranking: RankingConfig{
			Similarity:          0.60,
			Frequency:           0.25,
			Recency:             0.15,
			RecencyHalflifeDays: 30,
		},
		Compile: CompileConfig{
			TopKSkills:    3,
			TopKPatterns:  2,
			TopKLearnings: 3,
			OutputDir:     filepath.Join(home, ".context", "compiled"),
		},
		Serve: ServeConfig{
			Port: 7337,
		},
		API: APIConfig{
			OllamaBaseURL: "http://localhost:11434",
		},
		Models: ModelsConfig{
			Summarize:  "ollama/mistral",
			Detect:     "claude-sonnet-4-6",
			Compile:    "claude-sonnet-4-6",
			Synthesize: "claude-sonnet-4-6",
		},
	}
}

func Load() (*Config, error) {
	cfg := defaults()

	home, _ := os.UserHomeDir()
	tomlPath := filepath.Join(home, ".context", "ctx.toml")

	if _, err := os.Stat(tomlPath); err == nil {
		if _, err := toml.DecodeFile(tomlPath, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", tomlPath, err)
		}
	}

	// Expand ~ in all paths
	cfg.Capture.CursorDir = expandPath(cfg.Capture.CursorDir)
	cfg.Capture.ClaudeDir = expandPath(cfg.Capture.ClaudeDir)
	cfg.Capture.ContextDir = expandPath(cfg.Capture.ContextDir)
	cfg.Compile.OutputDir = expandPath(cfg.Compile.OutputDir)

	// API key from env takes precedence
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		cfg.API.AnthropicAPIKey = key
	}

	return cfg, nil
}

func (c *Config) DBPath() string {
	return filepath.Join(c.Capture.ContextDir, "index.db")
}

func (c *Config) ContextDir() string {
	return c.Capture.ContextDir
}
