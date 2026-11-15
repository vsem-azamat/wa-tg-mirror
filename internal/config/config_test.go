package config

import (
	"strings"
	"testing"
)

const valid = `
owner_id: 42
database: /data/x.db
service_bot: {token: "${SVC}"}
people:
  - {name: A, phone: "111", bot: {token: "tok-a"}}
groups:
  - {name: G, wa_jid: "1-2@g.us", tg_chat_id: -1001234567890}
overflow: {tg_chat_id: -1009876543210, ignore: ["0@s.whatsapp.net"]}
`

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestParseExpandsTokens(t *testing.T) {
	c, err := Parse([]byte(valid), env(map[string]string{"SVC": "tok-svc"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.BotDisplayName("Mum") != "Mum" {
		t.Fatalf("default bot name %q", c.BotDisplayName("Mum"))
	}
	if c.ServiceBot.Token != "tok-svc" || c.People[0].Bot.Token != "tok-a" {
		t.Fatalf("tokens %q %q", c.ServiceBot.Token, c.People[0].Bot.Token)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"bot name":        {"database:", "bot_name: WA\ndatabase:", "{name}"},
		"missing env":     {`"${SVC}"`, `"${NOPE}"`, "NOPE is not set"},
		"unknown field":   {"database:", "databse:", "databse"},
		"plus in phone":   {`"111"`, `"+111"`, "digits only"},
		"bad group jid":   {"1-2@g.us", "1-2@s.whatsapp.net", "@g.us"},
		"basic group":     {"tg_chat_id: -1001234567890", "tg_chat_id: -5408", "supergroup ID"},
		"shared token":    {`"tok-a"`, `"${SVC}"`, "already used"},
		"overflow reused": {"tg_chat_id: -1009876543210", "tg_chat_id: -1001234567890", "already used by groups[0]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(valid, tc.from, tc.to, 1)
			_, err := Parse([]byte(raw), env(map[string]string{"SVC": "tok-svc"}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
