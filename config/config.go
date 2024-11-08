package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DownloadBaseDir string  `json:"download_base_dir" yaml:"download_base_dir"`
	TargetPeerID    string  `json:"target_peer_id"    yaml:"target_peer_id"`
	FromIDs         []int64 `json:"from_ids"          yaml:"from_ids"`
}

func (cfg *Config) validate() error {
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

	if err := cfg.validate(); nil != err {
		return nil, fmt.Errorf("validation failed: %v", err)
	}

	return &cfg, nil
}
