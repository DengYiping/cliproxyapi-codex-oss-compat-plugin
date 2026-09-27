package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type rolloutEvent struct {
	Type    string `json:"type"`
	Payload struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Name    string `json:"name"`
		Author  string `json:"author"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"payload"`
}

func main() {
	model := flag.String("model", "glm-5.3-flash", "model served by the local proxy")
	effort := flag.String("effort", "medium", "Codex reasoning effort (medium or ultra)")
	timeout := flag.Duration("timeout", 180*time.Second, "deadline for the full Codex run")
	flag.Parse()

	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		inconclusive("generate run identifier: %v", err)
	}
	runID := hex.EncodeToString(nonce)
	expected := "42:" + runID
	fmt.Printf("run=%s model=%s effort=%s expected=%s\n", runID, *model, *effort, expected)

	cwd, err := os.Getwd()
	if err != nil {
		inconclusive("get working directory: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	prompt := fmt.Sprintf("Delegate exactly one task via collaboration.spawn_agent: compute 17 + 25 and reply with exactly %s. The child must give that exact reply; wait for it and then answer with the child's exact reply. Do not use shell tools or edit files.", expected)
	cmd := exec.CommandContext(ctx, "codex", "exec", "--json", "--skip-git-repo-check", "-s", "read-only", "--enable", "multi_agent_v2", "-m", *model, "-c", "model_reasoning_effort="+*effort, "-C", cwd, prompt)
	// Codex can leave child MCP processes holding stdout open after the deadline.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil && ctx.Err() == nil {
		inconclusive("codex exec failed: %v (stderr tail: %q)", err, tail(stderr.String(), 350))
	}

	threadID := ""
	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "thread.started" {
			threadID = event.ThreadID
		}
	}
	if err := scanner.Err(); err != nil || threadID == "" {
		inconclusive("cannot identify Codex thread (scan=%v, stdout tail=%q, stderr tail=%q)", err, tail(stdout.String(), 350), tail(stderr.String(), 250))
	}
	fmt.Printf("thread=%s\n", threadID)

	rolloutPath, err := findRollout(threadID)
	if err != nil {
		inconclusive("locate recorded thread: %v", err)
	}
	spawnCalls, childReply, parentReply, err := inspectRollout(rolloutPath)
	if err != nil {
		inconclusive("inspect recorded thread: %v", err)
	}
	fmt.Printf("spawn_calls=%d child_reply=%q parent_reply=%q\n", spawnCalls, tail(childReply, 160), tail(parentReply, 160))
	switch {
	case spawnCalls == 0:
		inconclusive("no collaboration.spawn_agent call; the risky boundary was not exercised")
	case !strings.Contains(childReply, expected):
		red("child did not receive/return the delegated task marker")
	case !strings.Contains(parentReply, expected):
		red("parent did not report the child's result")
	case ctx.Err() != nil:
		inconclusive("deadline expired after the expected replies; Codex did not finish")
	default:
		fmt.Println("GREEN: child and parent both returned the delegated result")
	}
}

func findRollout(threadID string) (string, error) {
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".codex")
	}
	var found string
	err := filepath.WalkDir(filepath.Join(root, "sessions"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), threadID+".jsonl") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("no rollout for %s", threadID)
	}
	return found, nil
}

func inspectRollout(path string) (int, string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", "", err
	}
	defer f.Close()
	spawnCalls, childReply, parentReply := 0, "", ""
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 32<<20)
	for scanner.Scan() {
		var event rolloutEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.Type != "response_item" {
			continue
		}
		p := event.Payload
		if p.Type == "function_call" && p.Name == "spawn_agent" {
			spawnCalls++
		}
		for _, content := range p.Content {
			if p.Type == "agent_message" && p.Author != "/root" && strings.Contains(content.Text, "Message Type: FINAL_ANSWER") {
				childReply = content.Text
			}
			if p.Type == "message" && p.Role == "assistant" {
				parentReply = content.Text
			}
		}
	}
	return spawnCalls, childReply, parentReply, scanner.Err()
}

func tail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[len(s)-limit:]
}

func red(format string, args ...any) {
	fmt.Printf("RED: "+format+"\n", args...)
	os.Exit(1)
}

func inconclusive(format string, args ...any) {
	fmt.Printf("INCONCLUSIVE: "+format+"\n", args...)
	os.Exit(2)
}
