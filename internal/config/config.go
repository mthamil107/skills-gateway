// Package config loads the gateway's YAML configuration.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mthamil107/skills-gateway/internal/auth"
)

// Config is the server configuration file.
type Config struct {
	Listen   string `yaml:"listen"`
	Database string `yaml:"database"`
	Policy   string `yaml:"policy"`
	// MaxUploadBytes bounds a compressed bundle upload.
	MaxUploadBytes int64 `yaml:"max_upload_bytes"`
	Auth           Auth  `yaml:"auth"`
}

// Auth selects an authentication mode.
type Auth struct {
	Mode string          `yaml:"mode"` // oidc | static | none
	OIDC auth.OIDCConfig `yaml:"oidc"`
	// Static maps bearer tokens to principals. Values may reference an
	// environment variable as "${NAME}" so tokens stay out of the file.
	Static []StaticToken `yaml:"static"`
	// None is the principal used when mode is none.
	None auth.Principal `yaml:"none"`
}

// StaticToken is one entry of the static token table.
type StaticToken struct {
	Token     string   `yaml:"token"`
	Subject   string   `yaml:"subject"`
	Teams     []string `yaml:"teams"`
	Roles     []string `yaml:"roles"`
	AgentType string   `yaml:"agent_type"`
}

// envRef matches ${NAME} references. A bare $NAME is left alone so values
// containing a dollar sign are not mangled.
var envRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// Load reads a config file, expanding ${VAR} references from the environment.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var missing []string
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // comments may mention ${VAR} without defining it
		}
		lines[i] = envRef.ReplaceAllStringFunc(line, func(m string) string {
			k := m[2 : len(m)-1]
			v, ok := os.LookupEnv(k)
			if !ok {
				missing = append(missing, k)
			}
			return v
		})
	}
	expanded := strings.Join(lines, "\n")
	if len(missing) > 0 {
		return nil, fmt.Errorf("config references unset environment variables: %s", strings.Join(missing, ", "))
	}
	c := &Config{}
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.defaults()
	return c, c.validate()
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.Database == "" {
		c.Database = "skills-gateway.db"
	}
	if c.MaxUploadBytes == 0 {
		c.MaxUploadBytes = 10 << 20
	}
}

func (c *Config) validate() error {
	if c.Policy == "" {
		return fmt.Errorf("config: policy file is required (access is default-deny)")
	}
	switch c.Auth.Mode {
	case "oidc", "none":
	case "static":
		if len(c.Auth.Static) == 0 {
			return fmt.Errorf("config: auth.static has no tokens")
		}
		for i, t := range c.Auth.Static {
			if len(t.Token) < 16 {
				return fmt.Errorf("config: auth.static[%d] token must be at least 16 characters", i)
			}
			if t.Subject == "" {
				return fmt.Errorf("config: auth.static[%d] subject is required", i)
			}
		}
	default:
		return fmt.Errorf("config: auth.mode must be oidc, static or none")
	}
	return nil
}

// StaticTable converts the static token list to an authenticator table.
func (a Auth) StaticTable() map[string]auth.Principal {
	m := make(map[string]auth.Principal, len(a.Static))
	for _, t := range a.Static {
		m[t.Token] = auth.Principal{Subject: t.Subject, Teams: t.Teams, Roles: t.Roles, AgentType: t.AgentType}
	}
	return m
}
