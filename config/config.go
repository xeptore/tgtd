package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

type Config struct {
	LogLevel        string  `yaml:"log_level"`
	DownloadBaseDir string  `yaml:"download_base_dir"`
	TargetPeerID    string  `yaml:"target_peer_id"`
	CredsDir        string  `yaml:"credentials_dir"`
	FromIDs         []int64 `yaml:"from_ids"`
	Signature       string  `yaml:"signature"`
}

func (cfg *Config) setDefaults() {
	if cfg.LogLevel == "" {
		cfg.LogLevel = zerolog.LevelInfoValue
	}

	if cfg.CredsDir == "" {
		cfg.CredsDir = ".creds"
	}
}

func (cfg *Config) validate() error {
	if _, err := zerolog.ParseLevel(cfg.LogLevel); nil != err {
		return fmt.Errorf("invalid log level: %v", err)
	}

	if cfg.DownloadBaseDir == "" {
		return errors.New("download base dir is empty")
	}

	if cfg.TargetPeerID == "" {
		return errors.New("target peer ID is empty")
	}

	return nil
}

func FromFile(filePath string) (*Config, error) {
	data, err := os.ReadFile(filePath)
	if nil != err {
		return nil, fmt.Errorf("failed to read config file %q: %v", filePath, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); nil != err {
		return nil, fmt.Errorf("failed to unmarshal config file %q: %v", filePath, err)
	}
	cfg.setDefaults()

	if err := cfg.validate(); nil != err {
		return nil, fmt.Errorf("validation failed: %v", err)
	}

	return &cfg, nil
}

func FromString(data string) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal([]byte(data), &cfg); nil != err {
		return nil, fmt.Errorf("failed to unmarshal config: %v", err)
	}
	cfg.setDefaults()

	if err := cfg.validate(); nil != err {
		return nil, fmt.Errorf("validation failed: %v", err)
	}

	return &cfg, nil
}
