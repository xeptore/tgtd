package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DownloadBaseDir           string `json:"download_base_dir"             yaml:"download_base_dir"`
	TargetPeerID              string `json:"target_peer_id"                yaml:"target_peer_id"`
	OriginalTidalDLConfigPath string `json:"original_tidal_dl_config_path" yaml:"original_tidal_dl_config_path"`
	TidalDLPath               string `json:"tidal_dl_path"                 yaml:"tidal_dl_path"`
}

func (cfg *Config) validate() error {
	if cfg.DownloadBaseDir == "" {
		return errors.New("config: download base dir is empty")
	}
	if _, err := os.Stat(cfg.DownloadBaseDir); nil != err {
		if errors.Is(err, os.ErrNotExist) {
		}
		return fmt.Errorf("config: download base dir %q does not exist: %v", cfg.DownloadBaseDir, err)
	}

	if cfg.OriginalTidalDLConfigPath == "" {
		return errors.New("config: original tidal-dl config path is empty")
	}
	if s, err := os.Stat(cfg.OriginalTidalDLConfigPath); nil != err {
		// TODO: improve error handling
		return fmt.Errorf("config: original tidal-dl config path dir %q does not exist: %v", cfg.OriginalTidalDLConfigPath, err)
	} else if !s.Mode().IsRegular() {
		return fmt.Errorf("config: original tidal-dl config path %q is not a regular file", cfg.OriginalTidalDLConfigPath)
	}

	if cfg.TidalDLPath != "" {
		if s, err := os.Stat(cfg.TidalDLPath); nil != err {
			// TODO: improve error handling
			return fmt.Errorf("config: tidal-dl binary path %q does not exist: %v", cfg.TidalDLPath, err)
		} else if !s.Mode().IsRegular() {
			return fmt.Errorf("config: tidal-dl binary path %q is not a regular file", cfg.TidalDLPath)
		}
	} else {
		cfg.TidalDLPath = "tidal-dl-ng"
	}

	if cfg.TargetPeerID == "" {
		return errors.New("config: target peer ID is empty")
	}

	return nil
}

func FromYML(filePath string) (*Config, error) {
	data, err := os.ReadFile(filePath)
	if nil != err {
		return nil, fmt.Errorf("config: failed to read config file %q: %v", filePath, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); nil != err {
		return nil, fmt.Errorf("config: failed to unmarshal config file %q: %v", filePath, err)
	}

	if err := cfg.validate(); nil != err {
		return nil, fmt.Errorf("config: validation failed: %v", err)
	}

	return &cfg, nil
}

func FromJSON(filePath string) (*Config, error) {
	data, err := os.ReadFile(filePath)
	if nil != err {
		return nil, fmt.Errorf("config: failed to read config file %q: %v", filePath, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); nil != err {
		return nil, fmt.Errorf("config: failed to unmarshal config file %q: %v", filePath, err)
	}

	if err := cfg.validate(); nil != err {
		return nil, fmt.Errorf("config: validation failed: %v", err)
	}

	return &cfg, nil
}

func FromYMLString(data string) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal([]byte(data), &cfg); nil != err {
		return nil, fmt.Errorf("config: failed to unmarshal config: %v", err)
	}

	if err := cfg.validate(); nil != err {
		return nil, fmt.Errorf("config: validation failed: %v", err)
	}

	return &cfg, nil
}
