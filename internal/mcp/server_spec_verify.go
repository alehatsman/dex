package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SpecVerifyInput is the input to the spec_verify tool.
type SpecVerifyInput struct {
	SpecPath    string `json:"spec_path" jsonschema:"path to the spec file, relative to project root (e.g. 'specs/watch.md') or absolute"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	NoJudge     bool   `json:"no_judge,omitempty" jsonschema:"skip the LLM judgment pass and return raw code citations only (useful when no chat model is configured or for fast CI checks)"`
}

// SpecItemResult is the verification result for one checklist item.
type SpecItemResult struct {
	// Item is the verbatim checklist or behavior clause text.
	Item string `json:"item"`
	// Checked is true when the spec marks this item [x] (done), false for [ ] (pending).
	Checked bool `json:"checked"`
	// Status is the verification outcome.
	//   "pass"        — code evidence confirms the clause
	//   "fail"        — code evidence contradicts or is missing the clause
	//   "unknown"     — evidence present but insufficient to judge
	//   "no-evidence" — no matching code found in the index
	//   "pending"     — item is unchecked ([ ]) and was not verified
	Status string `json:"status"`
	// Reason is a one-sentence explanation from the LLM (absent when no chat or no-judge).
	Reason string `json:"reason,omitempty"`
	// Cites are "path:line" references to the supporting code chunks.
	Cites []string `json:"cites,omitempty"`
}

// SpecVerifyOutput is the result returned by spec_verify.
type SpecVerifyOutput struct {
	// Status is the overall operation outcome.
	//   "ok"                          — verification ran (some items may still be fail/unknown)
	//   "no-index"                    — no index on disk; run `dex index` first
	//   "no-spec"                     — spec file not found at the given path
	//   "embedding-service-unreachable" — embedder offline; cite extraction skipped
	//   "error"                       — unexpected error
	Status string `json:"status"`
	Hint   string `json:"hint,omitempty"`
	// SpecID is the id: field from the spec front matter.
	SpecID string `json:"spec_id,omitempty"`
	// DriftedSince is true when commits have landed on covered paths
	// since the spec's last_verified commit.
	DriftedSince bool `json:"drifted_since,omitempty"`
	// DriftCommits are the one-line commit summaries since last_verified.
	DriftCommits []string `json:"drift_commits,omitempty"`
	// Results holds one entry per checklist item.
	Results      []SpecItemResult `json:"results,omitempty"`
	PassCount    int              `json:"pass_count"`
	FailCount    int              `json:"fail_count"`
	UnknownCount int              `json:"unknown_count"`
	PendingCount int              `json:"pending_count"`
}

func (s *Server) SpecVerify(ctx context.Context, in SpecVerifyInput) (SpecVerifyOutput, error) {
	_, out, err := s.specVerify(ctx, nil, in)
	return out, err
}

func (s *Server) specVerify(ctx context.Context, _ *sdk.CallToolRequest, in SpecVerifyInput) (*sdk.CallToolResult, SpecVerifyOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, SpecVerifyOutput{Status: "error", Hint: hint}, nil
	}

	specPath := in.SpecPath
	if !filepath.IsAbs(specPath) {
		specPath = filepath.Join(p.Root, specPath)
	}
	raw, err := os.ReadFile(specPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, SpecVerifyOutput{Status: "no-spec", Hint: "spec file not found: " + specPath}, nil
		}
		return nil, SpecVerifyOutput{Status: "error", Hint: fmt.Sprintf("read spec: %v", err)}, nil
	}

	spec := parseSpec(raw)
	out := SpecVerifyOutput{SpecID: spec.id}

	if spec.lastVerified != "" && len(spec.covers) > 0 {
		commits, drifted := detectDrift(p.Root, spec.lastVerified, spec.covers)
		out.DriftedSince = drifted
		out.DriftCommits = commits
	}

	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		out.Status = "no-index"
		out.Hint = fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)
		return nil, out, nil
	}

	if len(spec.items) == 0 {
		out.Status = "no-items"
		out.Hint = "no checklist items found in spec — nothing to verify (expected `- [ ]`/`- [x]` lines)"
		return nil, out, nil
	}

	if s.EmbedClient == nil {
		// Lean profile (DEX_EMBED_ENGINE=none): spec verification retrieves
		// matching code chunks by embedding each clause — no embedder, no
		// verification.
		out.Status = "lean-no-embedder"
		out.Hint = "lean profile (DEX_EMBED_ENGINE=none): spec_check needs the embedding service to match clauses to code."
		return nil, out, nil
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, SpecVerifyOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	for _, item := range spec.items {
		res := SpecItemResult{
			Item:    item.text,
			Checked: item.checked,
		}

		// Skip LLM judgment for unchecked items — they're known pending.
		if !item.checked {
			res.Status = "pending"
			out.Results = append(out.Results, res)
			out.PendingCount++
			continue
		}

		vecs, embedErr := s.EmbedClient.Embed(ctx, []string{item.text})
		if embedErr != nil {
			if errors.Is(embedErr, embed.ErrUnreachable) {
				out.Status = "embedding-service-unreachable"
				out.Hint = "embedding service offline — code citations unavailable"
				return nil, out, nil
			}
			res.Status = "unknown"
			res.Reason = fmt.Sprintf("embed error: %v", embedErr)
			out.Results = append(out.Results, res)
			out.UnknownCount++
			continue
		}

		hits, searchErr := st.Search(ctx, vecs[0], item.text, 5)
		if searchErr != nil {
			res.Status = "unknown"
			res.Reason = fmt.Sprintf("search error: %v", searchErr)
			out.Results = append(out.Results, res)
			out.UnknownCount++
			continue
		}

		for _, h := range hits {
			res.Cites = append(res.Cites, h.Path+":"+strconv.Itoa(h.StartLine))
		}
		if len(hits) == 0 {
			res.Status = "no-evidence"
			out.Results = append(out.Results, res)
			out.UnknownCount++
			continue
		}

		if !in.NoJudge && s.ChatClient != nil {
			j := s.judgeSpecItem(ctx, item.text, hits)
			res.Status = j.status
			res.Reason = j.reason
		} else {
			res.Status = "unknown"
		}

		switch res.Status {
		case "pass":
			out.PassCount++
		case "fail":
			out.FailCount++
		default:
			out.UnknownCount++
		}
		out.Results = append(out.Results, res)
	}

	out.Status = "ok"
	return nil, out, nil
}

type specJudgment struct {
	status string // "pass" | "fail" | "unknown"
	reason string
}

const specJudgeSystem = `You are a spec auditor. Given a spec clause and code evidence, determine whether the code implements the clause.

Respond with valid JSON only — no markdown, no explanation outside the JSON:
{"status":"pass","reason":"..."} or {"status":"fail","reason":"..."} or {"status":"unknown","reason":"..."}

Rules:
- "pass": the code clearly implements what the clause describes
- "fail": the code is missing the clause or clearly contradicts it
- "unknown": insufficient evidence to judge confidently
- reason: one concise sentence, cite a file:line if helpful`

func (s *Server) judgeSpecItem(ctx context.Context, clause string, hits []store.Hit) specJudgment {
	var sb strings.Builder
	sb.WriteString("SPEC CLAUSE: ")
	sb.WriteString(clause)
	sb.WriteString("\n\nCODE EVIDENCE:\n")
	for i, h := range hits {
		if i >= 4 {
			break
		}
		fmt.Fprintf(&sb, "\n--- %s:%d ---\n%s\n", h.Path, h.StartLine, specTruncate(h.Content, 500))
	}

	resp, err := s.ChatClient.Generate(ctx, []chat.Message{
		{Role: "system", Content: specJudgeSystem},
		{Role: "user", Content: sb.String()},
	}, chat.Options{MaxTokens: 150})
	if err != nil {
		return specJudgment{status: "unknown", reason: "chat error: " + err.Error()}
	}

	content := strings.TrimSpace(resp.Content)
	var j struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(content), &j); err != nil {
		// Try to extract JSON substring if the model added surrounding text.
		if start := strings.Index(content, "{"); start >= 0 {
			if end := strings.LastIndex(content, "}"); end > start {
				_ = json.Unmarshal([]byte(content[start:end+1]), &j)
			}
		}
	}
	if j.Status == "" {
		j.Status = "unknown"
	}
	return specJudgment{status: j.Status, reason: j.Reason}
}

// parsedSpec holds the extracted metadata and items from a spec file.
type parsedSpec struct {
	id           string
	lastVerified string
	covers       []string
	items        []parsedSpecItem
}

type parsedSpecItem struct {
	text    string
	checked bool
}

// parseSpec extracts front matter fields and checklist items from a spec
// markdown file. Falls back to Behavior clauses when no ## Checklist exists.
func parseSpec(data []byte) parsedSpec {
	var spec parsedSpec
	lines := strings.Split(string(data), "\n")

	// Locate YAML front matter between the opening and closing ---.
	frontEnd := -1
	if len(lines) > 0 && lines[0] == "---" {
		for i := 1; i < len(lines); i++ {
			if lines[i] == "---" {
				frontEnd = i
				break
			}
		}
	}
	if frontEnd > 0 {
		inCovers := false
		for i := 1; i < frontEnd; i++ {
			line := lines[i]
			switch {
			case strings.HasPrefix(line, "id:"):
				spec.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			case strings.HasPrefix(line, "last_verified:"):
				spec.lastVerified = strings.TrimSpace(strings.TrimPrefix(line, "last_verified:"))
			case strings.HasPrefix(line, "covers:"):
				inCovers = true
			case inCovers && strings.HasPrefix(line, "  - "):
				path := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "  - ")), `"`)
				spec.covers = append(spec.covers, path)
			default:
				if inCovers && !strings.HasPrefix(line, " ") {
					inCovers = false
				}
			}
		}
	}

	start := frontEnd + 1
	if frontEnd < 0 {
		start = 0
	}
	body := lines[start:]

	// Try ## Checklist section first.
	spec.items = parseChecklistItems(body)

	// Fall back to Behavior clauses when no checklist exists.
	if len(spec.items) == 0 {
		spec.items = parseBehaviorClauses(body)
	}

	return spec
}

func parseChecklistItems(lines []string) []parsedSpecItem {
	var items []parsedSpecItem
	inChecklist := false
	for _, line := range lines {
		if strings.HasPrefix(line, "## Checklist") {
			inChecklist = true
			continue
		}
		if inChecklist {
			if strings.HasPrefix(line, "## ") {
				break
			}
			if strings.HasPrefix(line, "- [x] ") || strings.HasPrefix(line, "- [ ] ") {
				checked := strings.HasPrefix(line, "- [x] ")
				text := strings.TrimSpace(line[6:])
				items = append(items, parsedSpecItem{text: text, checked: checked})
			} else if len(items) > 0 && (strings.HasPrefix(line, "      ") || strings.HasPrefix(line, "\t")) {
				// Continuation line for a multi-line checklist item.
				items[len(items)-1].text += " " + strings.TrimSpace(line)
			}
		}
	}
	return items
}

func parseBehaviorClauses(lines []string) []parsedSpecItem {
	var items []parsedSpecItem
	inBehavior := false
	for _, line := range lines {
		if strings.HasPrefix(line, "## Behavior") {
			inBehavior = true
			continue
		}
		if inBehavior {
			if strings.HasPrefix(line, "## ") {
				break
			}
			trimmed := strings.TrimPrefix(line, "- ")
			for _, kw := range []string{"WHEN ", "WHERE ", "IF ", "WHILE ", "AFTER "} {
				if strings.HasPrefix(trimmed, kw) {
					items = append(items, parsedSpecItem{text: trimmed, checked: false})
					break
				}
			}
		}
	}
	return items
}

// detectDrift runs git log to find commits since lastVerified that touch
// any of the covered paths. Returns the commit list and whether any exist.
func detectDrift(repoRoot, lastVerified string, covers []string) ([]string, bool) {
	args := []string{"log", "--oneline", lastVerified + "..HEAD", "--"}
	for _, cover := range covers {
		// Strip trailing glob suffixes (/** and /*) for git log path matching.
		clean := strings.TrimSuffix(cover, "/**")
		clean = strings.TrimSuffix(clean, "/*")
		args = append(args, clean)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var commits []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			commits = append(commits, line)
		}
	}
	return commits, len(commits) > 0
}

func specTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
