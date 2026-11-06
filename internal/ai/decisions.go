package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// OpenAI's decision model takes the same questions as System 1 in another
// shape, through its Decisions API. Apps send System 1 whichever model is
// chosen, so a request for OpenAI's is translated on the way there and its
// answers on the way back:
//
//	System 1                            OpenAI Decisions
//	state (text, or JSON)               input (text, or a message with input_json)
//	questions {name: {...}}             questions [{name, ...}], in the same order
//	noul                                predicate; noul ← probability
//	choice, criteria {label: desc}      choice, choices [{value, description}]
//	score, criteria [level, ...]        score, levels [{label}]
//	box                                 box
//	images [url]                        input_image parts
//
// assets, TypeSafe's way of attaching images, has no OpenAI equivalent and
// is refused.

// errTranslate is a request that can't be put to OpenAI's decision model.
type errTranslate struct{ msg string }

func (e errTranslate) Error() string { return e.msg }

func refuse(format string, v ...any) error { return errTranslate{fmt.Sprintf(format, v...)} }

// toDecisions turns a System 1 request into an OpenAI Decisions one.
func toDecisions(fields map[string]json.RawMessage, model string) ([]byte, error) {
	if _, ok := fields["assets"]; ok {
		return nil, refuse("OpenAI's decision model doesn't take assets; send images as data URLs in \"images\"")
	}
	state, ok := fields["state"]
	if !ok {
		return nil, refuse("a System 1 request needs a state")
	}
	var images []string
	if raw, ok := fields["images"]; ok {
		if err := json.Unmarshal(raw, &images); err != nil {
			return nil, refuse("images has to be a list of URLs or data URLs")
		}
	}
	input, err := decisionsInput(state, images)
	if err != nil {
		return nil, err
	}

	names, qs, err := ordered(fields["questions"])
	if err != nil || len(names) == 0 {
		return nil, refuse("questions has to be an object of named questions, with at least one")
	}
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		var q struct {
			Type         string          `json:"type"`
			Instructions json.RawMessage `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		}
		if err := json.Unmarshal(qs[name], &q); err != nil {
			return nil, refuse("question %q isn't an object", name)
		}
		d := map[string]any{"name": name}
		instructions := q.Instructions
		switch q.Type {
		case "noul":
			d["type"] = "predicate"
			// System 1 lets a yes/no question be put by its criteria alone.
			if len(instructions) == 0 && len(q.Criteria) > 0 {
				var c struct{ True, False string }
				if json.Unmarshal(q.Criteria, &c) == nil {
					instructions = jsonBody(fmt.Sprintf("True if: %s False if: %s", c.True, c.False))
				}
			}
		case "choice":
			d["type"] = "choice"
			labels, descs, err := ordered(q.Criteria)
			if err != nil || len(labels) < 2 {
				return nil, refuse("choice question %q needs at least two labels in criteria for OpenAI's decision model", name)
			}
			choices := make([]map[string]any, 0, len(labels))
			for _, l := range labels {
				c := map[string]any{"value": l}
				var desc string
				if json.Unmarshal(descs[l], &desc) == nil && desc != "" {
					c["description"] = desc
				}
				choices = append(choices, c)
			}
			d["choices"] = choices
		case "score":
			d["type"] = "score"
			var levels []string
			if err := json.Unmarshal(q.Criteria, &levels); err != nil || len(levels) < 2 || len(levels) > 10 {
				return nil, refuse("score question %q needs 2 to 10 levels in criteria for OpenAI's decision model", name)
			}
			ls := make([]map[string]string, len(levels))
			for i, l := range levels {
				ls[i] = map[string]string{"label": l}
			}
			d["levels"] = ls
		case "box":
			d["type"] = "box"
		default:
			return nil, refuse("question %q has type %q; System 1's are noul, choice, score and box", name, q.Type)
		}
		if len(instructions) > 0 {
			d["instructions"] = instructions
		} else {
			d["instructions"] = "Answer about the input."
		}
		out = append(out, d)
	}
	return json.Marshal(map[string]any{"model": model, "input": input, "questions": out})
}

// decisionsInput is a state, and any images, as OpenAI's input.
func decisionsInput(state json.RawMessage, images []string) (any, error) {
	var text string
	isText := json.Unmarshal(state, &text) == nil
	if isText && len(images) == 0 {
		return text, nil
	}
	var content []map[string]any
	if isText {
		content = append(content, map[string]any{"type": "input_text", "text": text})
	} else {
		var data any
		if err := json.Unmarshal(state, &data); err != nil {
			return nil, refuse("state has to be text, an object or a list")
		}
		content = append(content, map[string]any{"type": "input_json", "data": data})
	}
	for _, u := range images {
		content = append(content, map[string]any{"type": "input_image", "image_url": u})
	}
	return []map[string]any{{"type": "message", "role": "user", "content": content}}, nil
}

// fromDecisions turns OpenAI's answers back into System 1's.
func fromDecisions(b []byte) ([]byte, error) {
	var r struct {
		Model   string `json:"model"`
		Answers []struct {
			Type          string          `json:"type"`
			Name          string          `json:"name"`
			Probability   *float64        `json:"probability"`
			Choice        json.RawMessage `json:"choice"`
			Score         *float64        `json:"score"`
			Confidence    *float64        `json:"confidence"`
			Box           json.RawMessage `json:"box"`
			Probabilities []struct {
				Value       json.RawMessage `json:"value"`
				Label       string          `json:"label"`
				Probability float64         `json:"probability"`
			} `json:"probabilities"`
		} `json:"answers"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("OpenAI's answer isn't what Outpost expects: %w", err)
	}
	if r.Answers == nil {
		return nil, errors.New("OpenAI's answer has no answers in it")
	}
	answers := map[string]any{}
	for _, a := range r.Answers {
		out := map[string]any{}
		switch a.Type {
		case "predicate":
			out["type"] = "noul"
			if a.Probability != nil {
				out["noul"] = *a.Probability
			}
		case "choice":
			out["type"] = "choice"
			out["choice"] = valueText(a.Choice)
			probs := map[string]float64{}
			for _, p := range a.Probabilities {
				probs[valueText(p.Value)] = p.Probability
			}
			out["probabilities"] = probs
		case "score":
			out["type"] = "score"
			if a.Score != nil {
				out["score"] = *a.Score
			}
			legend, probs := map[string]string{}, map[string]float64{}
			for i, p := range a.Probabilities {
				idx := valueText(p.Value)
				if idx == "" {
					idx = strconv.Itoa(i)
				}
				legend[idx], probs[idx] = p.Label, p.Probability
			}
			out["legend"], out["probabilities"] = legend, probs
		case "box":
			out["type"] = "box"
			out["box"] = a.Box
		default: // "refusal", and anything newer, as it came
			out["type"] = a.Type
		}
		if a.Confidence != nil {
			out["confidence"] = *a.Confidence
		}
		answers[a.Name] = out
	}
	resp := map[string]any{"model": r.Model, "answers": answers}
	if len(r.Usage) > 0 {
		resp["usage"] = r.Usage
	}
	return json.Marshal(resp)
}

// valueText is a choice's value as System 1 writes a label: as text, so a
// boolean value reads "true".
func valueText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(bytes.TrimSpace(raw))
}

// ordered reads a JSON object's keys in the order they're written, which a
// map loses: the order an app asks its questions in is the order OpenAI
// answers them, and the order of a choice's labels may mean something.
func ordered(raw json.RawMessage) ([]string, map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil, errors.New("missing")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil, errors.New("not an object")
	}
	var keys []string
	vals := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		k, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		if _, dup := vals[k]; !dup {
			keys = append(keys, k)
		}
		vals[k] = v
	}
	return keys, vals, nil
}
