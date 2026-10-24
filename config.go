package main

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port int    `yaml:"port"`
		Bind string `yaml:"bind"`
		// AllowDirect lets requests that did not come through the hub act as admin.
		// Only for local testing; leave false in production.
		AllowDirect bool `yaml:"allow_direct"`
		// Where Wi-Fi passwords set through the app are kept (encrypted), and
		// the key that encrypts them. The key is generated on first run.
		SecretsFile   string `yaml:"secrets_file"`
		SecretKeyFile string `yaml:"secret_key_file"`
	} `yaml:"server"`
	AccessPoints []AccessPoint      `yaml:"access_points"`
	Profiles     map[string]Profile `yaml:"profiles"`
}

type AccessPoint struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	KeyFile  string `yaml:"key_file"`
	// HostKeyFingerprint pins the AP's SSH host key (SHA256:... as printed by ssh-keygen -lf).
	// Empty means any key is accepted and the fingerprint is logged so you can pin it.
	HostKeyFingerprint string `yaml:"host_key_fingerprint"`
	Profile            string `yaml:"profile"`
	// WLAN is the WLAN profile index that carries your SSID (cnPilot: "wireless wlan 1").
	WLAN int `yaml:"wlan"`
	// Radios maps a band ("2.4", "5") to the radio index on the AP.
	Radios         map[string]int `yaml:"radios"`
	TimeoutSeconds int            `yaml:"timeout_seconds"`
}

func (ap *AccessPoint) Timeout() time.Duration {
	return time.Duration(ap.TimeoutSeconds) * time.Second
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8084
	}
	if cfg.Server.Bind == "" {
		cfg.Server.Bind = "127.0.0.1"
	}
	if cfg.Server.SecretsFile == "" {
		cfg.Server.SecretsFile = "secrets.json"
	}
	if cfg.Server.SecretKeyFile == "" {
		cfg.Server.SecretKeyFile = "secret.key"
	}

	// Built-in profiles can be overridden by name from config.yaml.
	profiles := builtinProfiles()
	for name, p := range cfg.Profiles {
		profiles[name] = p
	}
	cfg.Profiles = profiles

	seen := map[string]bool{}
	for i := range cfg.AccessPoints {
		ap := &cfg.AccessPoints[i]
		if ap.ID == "" || ap.Host == "" || ap.Username == "" {
			return nil, fmt.Errorf("access point %d: id, host and username are required", i+1)
		}
		if seen[ap.ID] {
			return nil, fmt.Errorf("duplicate access point id %q", ap.ID)
		}
		seen[ap.ID] = true
		if ap.Name == "" {
			ap.Name = ap.ID
		}
		if ap.Port == 0 {
			ap.Port = 22
		}
		if ap.Profile == "" {
			ap.Profile = "cnpilot"
		}
		if _, ok := cfg.Profiles[ap.Profile]; !ok {
			return nil, fmt.Errorf("access point %q: unknown profile %q", ap.ID, ap.Profile)
		}
		if ap.WLAN == 0 {
			ap.WLAN = 1
		}
		if len(ap.Radios) == 0 {
			ap.Radios = map[string]int{"2.4": 1, "5": 2}
		}
		if ap.TimeoutSeconds == 0 {
			ap.TimeoutSeconds = 10
		}
	}
	return &cfg, nil
}
