// Package spec is the spec-validation harness framework (Phase-01.5 Task
// 1.5.3.2). It extracts `Spec checkpoint:` lines from the phase plan docs,
// assigns deterministic checkpoint IDs and shards, runs registered
// implementations, and emits a per-checkpoint JSON report.
package spec

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Checkpoint is one extracted `Spec checkpoint:` line.
type Checkpoint struct {
	// ID is the stable checkpoint identifier `P<phase>-T<task>-C<index>`
	// (e.g. "P01-T1.3.6-C2"). Index is the checkpoint's ordinal within its
	// task's SDD checklist in document order. IDs are deterministic across
	// runs; doc edits that reorder checkpoints inside a task are defects
	// (see AGENTS.md change protocol) and surface as corpus drift.
	ID      string `json:"checkpoint_id"`
	Phase   string `json:"phase"`   // "01", "01.5", "19.5" — from the Phase-*.md filename
	Task    string `json:"task"`    // e.g. "1.3.6" — from the `### Task N:` header
	Index   int    `json:"index"`   // 1-based ordinal within the task
	Text    string `json:"text"`    // checkpoint text (spec prose; suffix trimmed)
	File    string `json:"file"`    // phase doc basename
	Line    int    `json:"line"`    // 1-based line in File
	Checked bool   `json:"checked"` // doc checkbox state ([x] = task claims implemented)
	Shard   int    `json:"shard"`   // deterministic shard via ShardFor(ID)
}

// RawCount is the number of lines in the docs containing the literal
// string "Spec checkpoint:" — including non-checkbox prose mentions. The
// canonical count quoted in AGENTS.md / Phase-01.5 (543) is a raw grep
// count and includes exactly one such prose occurrence (the harness task's
// own implementation text in Phase-01.5 line ~62). Strict checkbox
// extraction yields 542 real checkpoints; both figures are recorded in the
// corpus header for auditability.
type Corpus struct {
	ExtractedCount int          `json:"extracted_count"` // strict checkpoint lines
	RawGrepCount   int          `json:"raw_grep_count"`  // literal "Spec checkpoint:" occurrences
	TaskCount      int          `json:"task_count"`      // `### Task` headers parsed
	Docs           string       `json:"docs"`            // docs dir scanned
	Shards         int          `json:"shards"`
	Checkpoints    []Checkpoint `json:"checkpoints"`
}

var (
	// Matches "- [ ] Spec checkpoint: <text>" / "* [x] Spec checkpoint: <text>".
	checkpointRe = regexp.MustCompile(`^\s*[-*] \[([ xX])\]\s*Spec checkpoint:\s*(.+?)\s*$`)
	// Matches "### Task 1.3.6: Title".
	taskRe = regexp.MustCompile(`^#{2,4}\s*Task\s+([0-9]+(?:\.[0-9]+)*)\s*:`)
	// Phase-01-Project-Foundation.md → "01"; Phase-19.5-…md → "19.5".
	phaseFileRe = regexp.MustCompile(`^Phase-([0-9]+(?:\.[0-9]+)?)`)
)

// NumShards is the canonical shard count (spec-validation runs across 4
// parallel shards per Phase-01.5 Task 1.5.3.1).
const NumShards = 4

// ShardFor deterministically maps a checkpoint ID to a shard in
// [0, NumShards) via FNV-1a 32-bit. Identical inputs always map to the same
// shard on every toolchain (no map iteration, no seeded hash).
func ShardFor(id string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % NumShards)
}

// PhaseFromFile derives the phase tag ("01", "01.5") from a doc basename.
func PhaseFromFile(base string) (string, error) {
	m := phaseFileRe.FindStringSubmatch(base)
	if m == nil {
		return "", fmt.Errorf("not a phase doc: %s", base)
	}
	return m[1], nil
}

// checkpointID builds the stable ID. Task numbers already contain the phase
// (e.g. task 1.5.3.2 inside Phase-01.5) so the ID carries both verbatim.
func checkpointID(phase, task string, index int) string {
	return fmt.Sprintf("P%s-T%s-C%d", phase, task, index)
}

// PhaseDocs returns sorted phase doc paths under docsDir.
func PhaseDocs(docsDir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(docsDir, "Phase-*.md"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range matches {
		if _, err := PhaseFromFile(filepath.Base(m)); err == nil {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Extract scans every Phase-*.md under docsDir and returns the corpus:
// one Checkpoint per `Spec checkpoint:` SDD-checklist line, in document
// order, each bound to the nearest preceding `### Task N:` header.
// Checkpoints found outside a task section are flagged with Task "-"
// (extraction is strict about the SDD structure so drift is visible).
func Extract(docsDir string) (*Corpus, error) {
	docs, err := PhaseDocs(docsDir)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("no Phase-*.md docs under %s", docsDir)
	}

	corpus := &Corpus{Docs: filepath.ToSlash(docsDir), Shards: NumShards}
	seen := map[string]int{}

	for _, doc := range docs {
		base := filepath.Base(doc)
		phase, err := PhaseFromFile(base)
		if err != nil {
			continue
		}
		fh, err := os.Open(doc)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)

		curTask := ""
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			if strings.Contains(line, "Spec checkpoint:") {
				corpus.RawGrepCount++
			}
			if m := taskRe.FindStringSubmatch(line); m != nil {
				curTask = m[1]
				corpus.TaskCount++
				continue
			}
			m := checkpointRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			task := curTask
			if task == "" {
				task = "-"
			}
			key := phase + "|" + task
			seen[key]++
			idx := seen[key]
			// Strip the uniform boilerplate suffix so reports carry the
			// checkpoint's spec text, not the SDD boilerplate.
			text := strings.TrimSuffix(m[2], " — defined first, validated against spec")
			corpus.Checkpoints = append(corpus.Checkpoints, Checkpoint{
				ID:      checkpointID(phase, task, idx),
				Phase:   phase,
				Task:    task,
				Index:   idx,
				Text:    text,
				File:    base,
				Line:    lineNo,
				Checked: m[1] == "x" || m[1] == "X",
				Shard:   ShardFor(checkpointID(phase, task, idx)),
			})
		}
		if err := sc.Err(); err != nil {
			fh.Close()
			return nil, err
		}
		fh.Close()
	}
	corpus.ExtractedCount = len(corpus.Checkpoints)
	return corpus, nil
}

// ShardCounts returns the number of checkpoints assigned to each shard.
func (c *Corpus) ShardCounts() [NumShards]int {
	var counts [NumShards]int
	for _, cp := range c.Checkpoints {
		counts[cp.Shard]++
	}
	return counts
}

// ParseShardCSV is a tiny helper for tests.
func ParseShardCSV(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
