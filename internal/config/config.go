// Package config parses, validates, and reloads the gateway's config file.
package config

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Owner    string            `yaml:"owner"`
	People   map[string]string `yaml:"people"`
	Agents   map[string]*Agent `yaml:"agents"`
	MCPToken string            `yaml:"mcp_token"`
}

type Agent struct {
	Token   string            `yaml:"token"`
	Members map[string]string `yaml:"members"`
	Groups  []string          `yaml:"groups"`
	History int               `yaml:"history"`

	mention *regexp.Regexp
}

var (
	phoneRE = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	nameRE  = regexp.MustCompile(`^[a-z0-9_]+$`)
)

// Watch loads the config and reloads it whenever the file changes.
// A config that fails to load leaves the previous one in place.
func Watch(path string) (func() *Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var cur atomic.Pointer[Config]
	cur.Store(c)
	go func() {
		for range time.Tick(2 * time.Second) {
			next, err := os.ReadFile(path)
			if err != nil || bytes.Equal(next, b) {
				continue
			}
			b = next
			c, err := Parse(b)
			if err != nil {
				log.Printf("config: %v; keeping the previous config", err)
				continue
			}
			cur.Store(c)
			log.Print("config: reloaded")
		}
	}()
	return cur.Load, nil
}

func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if _, ok := c.People[c.Owner]; !ok {
		return nil, fmt.Errorf("owner %q is not in people", c.Owner)
	}
	phones := map[string]bool{}
	for name, phone := range c.People {
		if !phoneRE.MatchString(phone) || phones[phone] {
			return nil, fmt.Errorf("people.%s: %q is not a unique E.164 phone number", name, phone)
		}
		phones[phone] = true
	}
	if c.MCPToken == "" {
		return nil, fmt.Errorf("mcp_token is empty")
	}
	tokens := map[string]bool{c.MCPToken: true}
	for name, a := range c.Agents {
		if a == nil || !nameRE.MatchString(name) {
			return nil, fmt.Errorf("agents.%s: needs a lowercase name of letters, digits, or _, and a body", name)
		}
		if a.Token == "" || tokens[a.Token] {
			return nil, fmt.Errorf("agents.%s.token is empty or used twice", name)
		}
		tokens[a.Token] = true
		for person, role := range a.Members {
			if _, ok := c.People[person]; !ok || (role != "read" && role != "write") {
				return nil, fmt.Errorf("agents.%s.members.%s: needs a person from people and a role of read or write", name, person)
			}
		}
		if a.History < 0 || a.History > 20 {
			return nil, fmt.Errorf("agents.%s.history: %d is not between 0 and 20", name, a.History)
		}
		a.mention = regexp.MustCompile(`(?i)(?:^|[^\w@])@` + name + `\b`)
	}
	return &c, nil
}

func (c *Config) Person(phone string) string {
	for name, p := range c.People {
		if p == phone {
			return name
		}
	}
	return ""
}

func (c *Config) Mentioned(text string) []string {
	var names []string
	for name, a := range c.Agents {
		if a.mention.MatchString(text) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// MemberDM reports whether chatGUID is a direct chat with one of a's members.
// Direct chat GUIDs look like "iMessage;-;+15551234567" or "any;-;+15551234567".
func (c *Config) MemberDM(a *Agent, chatGUID string) bool {
	parts := strings.SplitN(chatGUID, ";", 3)
	if len(parts) != 3 || parts[1] != "-" {
		return false
	}
	_, ok := a.Members[c.Person(parts[2])]
	return ok
}
