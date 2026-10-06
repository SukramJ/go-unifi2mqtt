// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// The configs paired with the bytes they read.
//
// Every other surface test looks at one side: the discovery configs, or
// the status items. Nothing put the two together and asked what Home
// Assistant would make of them, which is how 2.0.0 shipped absent
// clients whose only availability topic no longer existed. This file is
// a small model of the part of Home Assistant's MQTT integration that
// reads a config — availability, value templates, the per-platform state
// payloads and the attributes template — evaluated against a retained
// broker store. It is checked against Home Assistant core 2026.10:
//
//   - availability (homeassistant/components/mqtt/entity.py): every
//     topic starts "not available" (`_availability_prepare_subscribe_topics`
//     seeds `_available` with False), a message is run through the
//     entry's template and compared with payload_available /
//     payload_not_available — a result matching neither changes nothing
//     (`_availability_message_received`) — and availability_mode "all"
//     is `all(self._available.values())` (`available`);
//   - payload_available/payload_not_available default to "online" /
//     "offline" (mqtt/const.py, DEFAULT_PAYLOAD_AVAILABLE);
//   - device_tracker compares the rendered value with payload_home /
//     payload_not_home, defaulting to "home" / "not_home"
//     (mqtt/device_tracker.py, `_tracker_message_received`);
//   - switch maps state_on/state_off, falling back to payload_on/
//     payload_off ("ON"/"OFF") (mqtt/switch.py, `_is_on_map`).
//
// The templates are evaluated by jinjaRender below, a strict subset of
// Jinja: it fails on any construct it does not know rather than guess.

// --- a strict Jinja subset ------------------------------------------

// jinjaUndefined is the value of a missing attribute. Rendering it, or
// reading through it, is an error here: Home Assistant would log a
// template warning and render something nobody intended.
type jinjaUndefined struct{ what string }

// jinjaRender renders one `{{ … }}` template against a payload: `value`
// is the payload as a string and `value_json` its JSON decoding, which
// is undefined when the payload is not JSON.
//
// Supported: string and number literals, `value`, `value_json.a.b`,
// the filters `lower`, `int(default)` and `tojson`, the comparisons
// `== != >= <= > <`, and `x if cond else y`. Anything else is an error.
func jinjaRender(tmpl string, payload []byte) (string, error) {
	tmpl = strings.TrimSpace(tmpl)
	body, ok := strings.CutPrefix(tmpl, "{{")
	if !ok {
		return "", fmt.Errorf("template %q: not a single {{ … }} expression", tmpl)
	}
	body, ok = strings.CutSuffix(body, "}}")
	if !ok || strings.Contains(body, "{{") || strings.Contains(body, "{%") {
		return "", fmt.Errorf("template %q: not a single {{ … }} expression", tmpl)
	}

	toks, err := jinjaTokens(body)
	if err != nil {
		return "", fmt.Errorf("template %q: %w", tmpl, err)
	}
	vars := map[string]any{"value": string(payload)}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var vj any
	if dec.Decode(&vj) == nil && !dec.More() {
		vars["value_json"] = vj
	} else {
		vars["value_json"] = jinjaUndefined{"value_json (payload is not JSON)"}
	}

	p := &jinjaParser{toks: toks, vars: vars}
	v, err := p.expr()
	if err != nil {
		return "", fmt.Errorf("template %q: %w", tmpl, err)
	}
	if p.pos != len(p.toks) {
		return "", fmt.Errorf("template %q: unexpected %q", tmpl, p.toks[p.pos])
	}
	s, err := jinjaString(v)
	if err != nil {
		return "", fmt.Errorf("template %q on %q: %w", tmpl, payload, err)
	}
	return s, nil
}

func jinjaTokens(s string) ([]string, error) {
	var out []string
	for i := 0; i < len(s); {
		r := rune(s[i])
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '\'' || r == '"':
			end := strings.IndexByte(s[i+1:], s[i])
			if end < 0 {
				return nil, errors.New("unterminated string")
			}
			out = append(out, s[i:i+end+2])
			i += end + 2
		case strings.ContainsRune("=!<>", r):
			if i+1 < len(s) && s[i+1] == '=' {
				out = append(out, s[i:i+2])
				i += 2
			} else if r == '<' || r == '>' {
				out = append(out, s[i:i+1])
				i++
			} else {
				return nil, fmt.Errorf("unsupported operator at %q", s[i:])
			}
		case strings.ContainsRune("|().,", r):
			out = append(out, s[i:i+1])
			i++
		case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-':
			j := i + 1
			for j < len(s) && (s[j] == '_' || unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j]))) {
				j++
			}
			out = append(out, s[i:j])
			i = j
		default:
			return nil, fmt.Errorf("unsupported character %q", r)
		}
	}
	return out, nil
}

type jinjaParser struct {
	toks []string
	pos  int
	vars map[string]any
}

func (p *jinjaParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *jinjaParser) next() string {
	t := p.peek()
	p.pos++
	return t
}

// expr is `cmp [if cmp else expr]`. Both branches are parsed, but only
// the chosen one is evaluated, as in Jinja.
func (p *jinjaParser) expr() (any, error) {
	start := p.pos
	if err := p.skipCmp(); err != nil {
		return nil, err
	}
	if p.peek() != "if" {
		p.pos = start
		return p.cmp()
	}
	thenStart, thenEnd := start, p.pos
	p.next() // if
	cond, err := p.cmp()
	if err != nil {
		return nil, err
	}
	if p.next() != "else" {
		return nil, errors.New("conditional without else")
	}
	elseStart := p.pos
	if _, err := p.dryExpr(); err != nil {
		return nil, err
	}
	end := p.pos
	truth, err := jinjaTruth(cond)
	if err != nil {
		return nil, err
	}
	if truth {
		p.pos = thenStart
		v, err := p.cmp()
		if err != nil {
			return nil, err
		}
		if p.pos != thenEnd {
			return nil, errors.New("malformed conditional")
		}
		p.pos = end
		return v, nil
	}
	p.pos = elseStart
	v, err := p.expr()
	p.pos = end
	return v, err
}

// skipCmp and dryExpr advance over an expression without evaluating
// it, so an untaken branch cannot fail on an undefined value.
func (p *jinjaParser) skipCmp() error {
	saved := p.vars
	p.vars = nil
	defer func() { p.vars = saved }()
	_, err := p.cmp()
	return err
}

func (p *jinjaParser) dryExpr() (any, error) {
	saved := p.vars
	p.vars = nil
	defer func() { p.vars = saved }()
	return p.expr()
}

func (p *jinjaParser) cmp() (any, error) {
	left, err := p.pipe()
	if err != nil {
		return nil, err
	}
	switch op := p.peek(); op {
	case "==", "!=", ">=", "<=", ">", "<":
		p.next()
		right, err := p.pipe()
		if err != nil {
			return nil, err
		}
		if p.vars == nil {
			return nil, nil
		}
		return jinjaCompare(op, left, right)
	default:
		return left, nil
	}
}

func (p *jinjaParser) pipe() (any, error) {
	v, err := p.primary()
	if err != nil {
		return nil, err
	}
	for p.peek() == "|" {
		p.next()
		name := p.next()
		var args []any
		if p.peek() == "(" {
			p.next()
			for p.peek() != ")" {
				a, err := p.primary()
				if err != nil {
					return nil, err
				}
				args = append(args, a)
				if p.peek() == "," {
					p.next()
				}
			}
			p.next()
		}
		if p.vars == nil {
			continue
		}
		if v, err = jinjaFilter(name, v, args); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func (p *jinjaParser) primary() (any, error) {
	t := p.next()
	switch {
	case t == "":
		return nil, errors.New("unexpected end of expression")
	case t[0] == '\'' || t[0] == '"':
		return t[1 : len(t)-1], nil
	case t[0] == '-' || unicode.IsDigit(rune(t[0])):
		return json.Number(t), nil
	case t == "none" || t == "None":
		return nil, nil
	case t == "value" || t == "value_json":
		var v any
		if p.vars != nil {
			v = p.vars[t]
		}
		for p.peek() == "." {
			p.next()
			field := p.next()
			if p.vars == nil {
				continue
			}
			m, ok := v.(map[string]any)
			if !ok {
				if u, isU := v.(jinjaUndefined); isU {
					return nil, fmt.Errorf("%s.%s: %s is undefined", t, field, u.what)
				}
				return nil, fmt.Errorf("%s.%s: not an object", t, field)
			}
			if v, ok = m[field]; !ok {
				return nil, fmt.Errorf("%s.%s is undefined", t, field)
			}
		}
		if u, ok := v.(jinjaUndefined); ok && p.vars != nil {
			return nil, fmt.Errorf("%s is undefined", u.what)
		}
		return v, nil
	default:
		return nil, fmt.Errorf("unsupported name %q", t)
	}
}

func jinjaFilter(name string, v any, args []any) (any, error) {
	switch name {
	case "lower":
		s, err := jinjaString(v)
		return strings.ToLower(s), err
	case "int":
		if len(args) > 1 {
			return nil, errors.New("int: too many arguments")
		}
		var def any = json.Number("0")
		if len(args) == 1 {
			def = args[0]
		}
		s, err := jinjaString(v)
		if err != nil {
			return def, nil //nolint:nilerr // Jinja's int filter falls back to the default
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return json.Number(strconv.FormatInt(n, 10)), nil
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return json.Number(strconv.FormatInt(int64(math.Trunc(f)), 10)), nil
		}
		return def, nil
	case "tojson":
		b, err := json.Marshal(v)
		return string(b), err
	default:
		return nil, fmt.Errorf("unsupported filter %q", name)
	}
}

// jinjaString is Python's str() of a decoded JSON value.
func jinjaString(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "None", nil
	case string:
		return x, nil
	case json.Number:
		return x.String(), nil
	case bool:
		if x {
			return "True", nil
		}
		return "False", nil
	case jinjaUndefined:
		return "", fmt.Errorf("%s is undefined", x.what)
	default:
		b, err := json.Marshal(x)
		return string(b), err
	}
}

func jinjaTruth(v any) (bool, error) {
	switch x := v.(type) {
	case nil:
		return false, nil
	case bool:
		return x, nil
	case string:
		return x != "", nil
	case json.Number:
		f, err := x.Float64()
		return f != 0, err
	default:
		return false, fmt.Errorf("unsupported truth value %T", v)
	}
}

func jinjaCompare(op string, a, b any) (any, error) {
	an, aNum := a.(json.Number)
	bn, bNum := b.(json.Number)
	if aNum && bNum {
		af, err1 := an.Float64()
		bf, err2 := bn.Float64()
		if err1 != nil || err2 != nil {
			return nil, errors.New("bad number")
		}
		switch op {
		case "==":
			return af == bf, nil
		case "!=":
			return af != bf, nil
		case ">=":
			return af >= bf, nil
		case "<=":
			return af <= bf, nil
		case ">":
			return af > bf, nil
		default:
			return af < bf, nil
		}
	}
	switch op {
	case "==":
		return jinjaEqual(a, b), nil
	case "!=":
		return !jinjaEqual(a, b), nil
	default:
		return nil, fmt.Errorf("cannot order %T and %T", a, b)
	}
}

func jinjaEqual(a, b any) bool {
	as, aOK := a.(string)
	bs, bOK := b.(string)
	if aOK && bOK {
		return as == bs
	}
	return a == nil && b == nil
}

func TestJinjaSubset(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		tmpl, payload, want string
	}{
		{"{{ value_json.val }}", `{"val":"home","ts":1,"lc":1}`, "home"},
		{"{{ value_json.val }}", `{"val":-54}`, "-54"},
		{"{{ value_json.val }}", `{"val":12.5}`, "12.5"},
		{"{{ value_json.val | lower }}", `{"val":true}`, "true"},
		{"{{ value_json.val | lower }}", `{"val":false}`, "false"},
		{"{{ value_json.val | tojson }}", `{"val":{"a":1}}`, `{"a":1}`},
		{"{{ 'ON' if value_json.val == 'UP' else 'OFF' }}", `{"val":"UP"}`, "ON"},
		{"{{ 'ON' if value_json.val == 'UP' else 'OFF' }}", `{"val":"DOWN"}`, "OFF"},
		{"{{ 'online' if value | int(0) >= 2 else 'offline' }}", "2", "online"},
		{"{{ 'online' if value | int(0) >= 2 else 'offline' }}", "1", "offline"},
		{"{{ 'online' if value | int(0) >= 2 else 'offline' }}", "online", "offline"},
		{"{{ 'online' if value == 'home' else 'offline' }}", "home", "online"},
	} {
		got, err := jinjaRender(tc.tmpl, []byte(tc.payload))
		if err != nil || got != tc.want {
			t.Errorf("render(%q, %s) = %q, %v; want %q", tc.tmpl, tc.payload, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ tmpl, payload string }{
		{"{{ value_json.val }}", "home"},              // not JSON
		{"{{ value_json.missing }}", `{"val":1}`},     // undefined field
		{"{{ value_json.val | round }}", `{"val":1}`}, // unknown filter
		{"{% if x %}y{% endif %}", "1"},               // statement
		{"{{ states('sensor.x') }}", "1"},             // unknown name
	} {
		if got, err := jinjaRender(tc.tmpl, []byte(tc.payload)); err == nil {
			t.Errorf("render(%q, %s) = %q, want an error", tc.tmpl, tc.payload, got)
		}
	}
}

// --- what Home Assistant makes of a config ---------------------------

// haConfig is the part of a discovery config Home Assistant's MQTT
// integration reads to decide availability, state and attributes.
type haConfig struct {
	UniqueID         string  `json:"unique_id"`
	StateTopic       string  `json:"state_topic"`
	CommandTopic     string  `json:"command_topic"`
	ValueTemplate    string  `json:"value_template"`
	AttributesTopic  string  `json:"json_attributes_topic"`
	AttributesTmpl   string  `json:"json_attributes_template"`
	AvailabilityMode string  `json:"availability_mode"`
	PayloadOn        *string `json:"payload_on"`
	PayloadOff       *string `json:"payload_off"`
	StateOn          *string `json:"state_on"`
	StateOff         *string `json:"state_off"`
	PayloadHome      *string `json:"payload_home"`
	PayloadNotHome   *string `json:"payload_not_home"`
	Unit             string  `json:"unit_of_measurement"`
	StateClass       string  `json:"state_class"`
	Availability     []struct {
		Topic               string  `json:"topic"`
		ValueTemplate       string  `json:"value_template"`
		PayloadAvailable    *string `json:"payload_available"`
		PayloadNotAvailable *string `json:"payload_not_available"`
	} `json:"availability"`
}

// haEntity is what Home Assistant would show for one config.
type haEntity struct {
	platform  string
	available bool
	// state is the entity state: "home"/"not_home", "on"/"off", a
	// sensor's rendered value, "unknown" when its topic holds nothing;
	// empty for a button.
	state string
	// problems are the ways the config and the store disagree: a
	// template that fails, a payload that maps onto nothing.
	problems []string
}

func or(p *string, def string) string {
	if p != nil {
		return *p
	}
	return def
}

// haEvaluate evaluates the config retained at topic against store.
func haEvaluate(topic string, raw []byte, store map[string][]byte) haEntity {
	levels := strings.Split(topic, "/")
	e := haEntity{platform: levels[len(levels)-4]}
	var cfg haConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		e.problems = append(e.problems, "config is not JSON: "+err.Error())
		return e
	}
	bad := func(format string, args ...any) { e.problems = append(e.problems, fmt.Sprintf(format, args...)) }
	render := func(tmpl string, payload []byte) (string, bool) {
		if tmpl == "" {
			return string(payload), true
		}
		out, err := jinjaRender(tmpl, payload)
		if err != nil {
			bad("%v", err)
			return "", false
		}
		return out, true
	}

	// Availability.
	avail := make([]bool, len(cfg.Availability))
	for i, a := range cfg.Availability {
		payload, ok := store[a.Topic]
		if !ok {
			bad("availability topic %s holds nothing: the entity can never become available", a.Topic)
			continue
		}
		got, ok := render(a.ValueTemplate, payload)
		if !ok {
			continue
		}
		switch got {
		case or(a.PayloadAvailable, "online"):
			avail[i] = true
		case or(a.PayloadNotAvailable, "offline"):
		default:
			bad("availability %s renders %q, matching neither payload", a.Topic, got)
		}
	}
	e.available = true
	if len(avail) > 0 {
		switch cfg.AvailabilityMode {
		case "", "latest", "all":
			// Every config of this bridge says "all"; "latest" with
			// one entry reads the same.
			e.available = !slices.Contains(avail, false)
		case "any":
			e.available = slices.Contains(avail, true)
		}
	}

	// State.
	if e.platform == "button" {
		return e
	}
	payload, ok := store[cfg.StateTopic]
	if !ok {
		e.state = "unknown"
	} else if got, ok := render(cfg.ValueTemplate, payload); ok {
		switch e.platform {
		case "device_tracker":
			switch got {
			case or(cfg.PayloadHome, "home"):
				e.state = "home"
			case or(cfg.PayloadNotHome, "not_home"):
				e.state = "not_home"
			default:
				bad("tracker %s renders %q, which is neither home nor not_home", cfg.StateTopic, got)
			}
		case "switch":
			switch got {
			case or(cfg.StateOn, or(cfg.PayloadOn, "ON")):
				e.state = "on"
			case or(cfg.StateOff, or(cfg.PayloadOff, "OFF")):
				e.state = "off"
			default:
				bad("switch %s renders %q, which is neither state_on nor state_off", cfg.StateTopic, got)
			}
		case "binary_sensor":
			switch got {
			case or(cfg.PayloadOn, "ON"):
				e.state = "on"
			case or(cfg.PayloadOff, "OFF"):
				e.state = "off"
			default:
				bad("binary sensor %s renders %q, which is neither payload_on nor payload_off", cfg.StateTopic, got)
			}
		default:
			e.state = got
			if cfg.Unit != "" || cfg.StateClass != "" {
				if _, err := strconv.ParseFloat(got, 64); err != nil {
					bad("numeric sensor %s renders %q", cfg.StateTopic, got)
				}
			}
		}
	}

	// Attributes.
	if cfg.AttributesTopic != "" {
		if payload, ok := store[cfg.AttributesTopic]; ok {
			if got, ok := render(cfg.AttributesTmpl, payload); ok {
				var obj map[string]any
				if json.Unmarshal([]byte(got), &obj) != nil {
					bad("attributes %s render %q, not a JSON object", cfg.AttributesTopic, got)
				}
			}
		} else if e.available {
			bad("attributes topic %s holds nothing", cfg.AttributesTopic)
		}
	}
	if e.available && e.state == "unknown" {
		bad("available, but state topic %s holds nothing", cfg.StateTopic)
	}
	return e
}

// retainedStore folds a publish history into what a broker retains.
func retainedStore(msgs []published) map[string][]byte {
	store := map[string][]byte{}
	for _, m := range msgs {
		if !m.retain {
			continue
		}
		if m.payload == "" {
			delete(store, m.topic)
			continue
		}
		store[m.topic] = []byte(m.payload)
	}
	return store
}

// The contract over every surface scenario: each entity the production
// code announces, evaluated against the bytes the production publisher
// put on the topics it names, is available where it should be and
// renders a state Home Assistant accepts.
func TestEveryEntityReadsWhatThePublisherWrites(t *testing.T) {
	t.Parallel()

	for _, sc := range surfaceScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			_, broker := driveSurface(t, sc)
			broker.mu.Lock()
			store := retainedStore(broker.msgs)
			broker.mu.Unlock()

			var configs int
			for _, topic := range sortedKeys(store) {
				if !strings.HasPrefix(topic, "homeassistant/") || !strings.HasSuffix(topic, "/config") {
					continue
				}
				configs++
				e := haEvaluate(topic, store[topic], store)
				for _, p := range e.problems {
					t.Errorf("%s: %s", topic, p)
				}
				// The fixture's offline AP is the one object whose
				// entities may be unavailable here.
				if !e.available && !strings.Contains(topic, surfOldMAC.String()) {
					t.Errorf("%s: unavailable", topic)
				}
			}
			if configs == 0 {
				t.Fatal("no discovery config was published")
			}
		})
	}
}
