package tuna

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenCodeCaptureExcludesDelegatedSessions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is needed to exercise the OpenCode plugin")
	}
	p, _, _ := fixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TUNA_TEST_CAPTURE_FILE", p.file("captured.jsonl"))
	t.Setenv("TUNA_INTERNAL", "")
	if err := installHarness(p, "opencode", harnessDir(p, "opencode"), exe); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(p.Home, "check-plugin.mjs")
	if err := os.WriteFile(script, []byte(`
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
const source = await readFile(process.argv[2], "utf8");
const { default: plugin } = await import("data:text/javascript;base64," + Buffer.from(source).toString("base64"));
let handler;
let session = { id: "root", location: { directory: "/project" } };
let contextReads = 0;
await plugin.setup({
  location: { directory: "/project" },
  mcp: { transform: async (fn) => fn({ set() {} }) },
  session: {
    hook: async (name, fn) => { assert.equal(name, "prompt"); handler = fn; },
    get: async () => { if (!session) throw new Error("session unavailable"); return session; },
    context: async () => { contextReads++; return []; },
  },
});
const event = (id) => ({ sessionID: id, messageID: "message-" + id, prompt: { text: "Please keep changes focused." } });
await handler(event("root"));
session = { id: "child", parentID: "root", location: { directory: "/project" } };
await handler(event("child"));
session = undefined;
await handler(event("unavailable"));
const captured = (await readFile(process.env.TUNA_TEST_CAPTURE_FILE, "utf8")).trim().split("\n").map(JSON.parse);
assert.equal(captured.length, 1, "delegated or unknown-origin instructions were captured");
assert.equal(captured[0].session_id, "root");
assert.equal(captured[0].prompt, "Please keep changes focused.");
assert.equal(contextReads, 1, "delegated sessions should not load transcript context");
`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, script, filepath.Join(harnessDir(p, "opencode"), "plugins", "tuna", "index.js"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plugin capture: %v\n%s", err, out)
	}
}
