package locomo

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Turn is one message in a conversation.
type Turn struct {
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
}

// Question is a recall question grounded in a specific conversation.
type Question struct {
	ID       string `json:"id"`
	Category string `json:"category"` // e.g. single_hop, multi_hop, temporal
	Text     string `json:"text"`
	Answer   string `json:"answer"`
	ConvID   string `json:"conv_id"`
}

// Conversation is one long dialogue with associated recall questions.
type Conversation struct {
	ID        string     `json:"id"`
	Turns     []Turn     `json:"turns"`
	Questions []Question `json:"questions"`
}

// Dataset is the full collection loaded from NDJSON.
type Dataset struct {
	Conversations []Conversation
}

// LoadNDJSON reads one Conversation JSON object per line from r.
func LoadNDJSON(r io.Reader) (Dataset, error) {
	var d Dataset
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4<<20), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		var c Conversation
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return Dataset{}, fmt.Errorf("locomo: parse conversation: %w", err)
		}
		d.Conversations = append(d.Conversations, c)
	}
	return d, sc.Err()
}

// LoadFile opens path and calls LoadNDJSON.
func LoadFile(path string) (Dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return Dataset{}, err
	}
	defer func() { _ = f.Close() }()
	return LoadNDJSON(f)
}

// Questions returns all questions across all conversations.
func (d Dataset) Questions() []Question {
	var out []Question
	for _, c := range d.Conversations {
		out = append(out, c.Questions...)
	}
	return out
}

// TotalTurns returns the sum of turns across all conversations.
func (d Dataset) TotalTurns() int {
	n := 0
	for _, c := range d.Conversations {
		n += len(c.Turns)
	}
	return n
}
