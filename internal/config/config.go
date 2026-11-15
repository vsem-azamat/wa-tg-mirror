// Package config loads and validates the mirror configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the whole configuration file.
type Config struct {
	// OwnerID is the Telegram user ID of the only person the bots listen to.
	OwnerID int64 `yaml:"owner_id"`
	// Database is the SQLite file with the WhatsApp session and the message map.
	Database string `yaml:"database"`
	// MarkRead marks a WhatsApp chat as read when the owner writes to it from Telegram.
	MarkRead bool `yaml:"mark_read"`
	// BotName is the display name of a person's bot; {name} stands for people[].name.
	BotName string `yaml:"bot_name"`
	// ServiceBot writes to WhatsApp groups, speaks for senders without a bot and runs the overflow forum.
	ServiceBot Bot `yaml:"service_bot"`
	// People get their own bot each.
	People []Person `yaml:"people"`
	// Groups are WhatsApp groups mirrored into ordinary Telegram groups.
	Groups []Group `yaml:"groups"`
	// Overflow is the forum supergroup where every other chat gets a topic. Optional.
	Overflow *Overflow `yaml:"overflow"`
}

// Bot is a Telegram bot token. A value of the form ${NAME} is read from the environment.
type Bot struct {
	Token string `yaml:"token"`
}

// Person is one WhatsApp contact mirrored through their own bot.
type Person struct {
	Name string `yaml:"name"`
	// Phone is the international number, digits only, without "+".
	Phone string `yaml:"phone"`
	Bot   Bot    `yaml:"bot"`
}

// Group maps a WhatsApp group to a Telegram group.
type Group struct {
	Name     string `yaml:"name"`
	WAJID    string `yaml:"wa_jid"`
	TGChatID int64  `yaml:"tg_chat_id"`
}

// Overflow is the forum supergroup for every chat that is neither a person nor a group.
type Overflow struct {
	TGChatID int64 `yaml:"tg_chat_id"`
	// Ignore lists WhatsApp chat JIDs that are not mirrored at all.
	Ignore []string `yaml:"ignore"`
}

// Load reads, expands and validates the file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's config file
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw, os.LookupEnv)
}

// Parse decodes raw strictly, expands ${NAME} tokens through lookup and validates the result.
func Parse(raw []byte, lookup func(string) (string, bool)) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.ServiceBot.expand(lookup); err != nil {
		return nil, fmt.Errorf("service_bot: %w", err)
	}
	for i := range c.People {
		if err := c.People[i].Bot.expand(lookup); err != nil {
			return nil, fmt.Errorf("people[%d] %s: %w", i, c.People[i].Name, err)
		}
	}
	if c.BotName == "" {
		c.BotName = "{name}"
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (b *Bot) expand(lookup func(string) (string, bool)) error {
	name, ok := strings.CutPrefix(b.Token, "${")
	if !ok {
		return nil
	}
	name, ok = strings.CutSuffix(name, "}")
	if !ok || name == "" {
		return fmt.Errorf("token reference %q is not of the form ${NAME}", b.Token)
	}
	v, ok := lookup(name)
	if !ok || v == "" {
		return fmt.Errorf("environment variable %s is not set", name)
	}
	b.Token = v
	return nil
}

func (c *Config) validate() error {
	var errs []error
	if c.OwnerID <= 0 {
		errs = append(errs, errors.New("owner_id must be a Telegram user ID"))
	}
	if c.Database == "" {
		errs = append(errs, errors.New("database is required"))
	}
	if !strings.Contains(c.BotName, "{name}") {
		errs = append(errs, errors.New("bot_name must contain {name}"))
	}
	if c.ServiceBot.Token == "" {
		errs = append(errs, errors.New("service_bot.token is required"))
	}
	tokens := map[string]string{c.ServiceBot.Token: "service_bot"}
	phones := map[string]bool{}
	for i, p := range c.People {
		where := fmt.Sprintf("people[%d]", i)
		if p.Name == "" {
			errs = append(errs, fmt.Errorf("%s: name is required", where))
		}
		if !isDigits(p.Phone) {
			errs = append(errs, fmt.Errorf("%s: phone must be digits only, without +", where))
		}
		if phones[p.Phone] {
			errs = append(errs, fmt.Errorf("%s: phone is listed twice", where))
		}
		phones[p.Phone] = true
		if p.Bot.Token == "" {
			errs = append(errs, fmt.Errorf("%s: bot.token is required", where))
		} else if other, dup := tokens[p.Bot.Token]; dup && p.Bot.Token != "" {
			errs = append(errs, fmt.Errorf("%s: bot token is already used by %s", where, other))
		}
		tokens[p.Bot.Token] = where
	}
	chats := map[int64]string{}
	for i, g := range c.Groups {
		where := fmt.Sprintf("groups[%d]", i)
		if !strings.HasSuffix(g.WAJID, "@g.us") {
			errs = append(errs, fmt.Errorf("%s: wa_jid must be a group JID ending in @g.us", where))
		}
		if !isSupergroup(g.TGChatID) {
			errs = append(errs, fmt.Errorf("%s: tg_chat_id must be a supergroup ID (-100…): in basic groups each bot numbers messages on its own, so replies cannot be matched", where))
		}
		if other, dup := chats[g.TGChatID]; dup {
			errs = append(errs, fmt.Errorf("%s: tg_chat_id is already used by %s", where, other))
		}
		chats[g.TGChatID] = where
	}
	if o := c.Overflow; o != nil {
		if !isSupergroup(o.TGChatID) {
			errs = append(errs, errors.New("overflow.tg_chat_id must be a forum supergroup ID (-100…)"))
		}
		if other, dup := chats[o.TGChatID]; dup {
			errs = append(errs, fmt.Errorf("overflow.tg_chat_id is already used by %s", other))
		}
	}
	return errors.Join(errs...)
}

// supergroupBase is the offset of supergroup and channel IDs in the Bot API.
const supergroupBase = -1_000_000_000_000

func isSupergroup(id int64) bool { return id < supergroupBase }

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// BotDisplayName returns the bot name of a person, at most the 64 characters Telegram allows.
func (c *Config) BotDisplayName(person string) string {
	name := []rune(strings.ReplaceAll(c.BotName, "{name}", person))
	return string(name[:min(len(name), maxBotName)])
}

// maxBotName is Telegram's limit on a bot's display name.
const maxBotName = 64
