// Managed by tuna. Recreated by `tuna setup opencode`.
import { spawn } from "node:child_process";

const binary = __TUNA_BINARY__;
const home = __TUNA_HOME__;

function capture(payload) {
  return new Promise((resolve) => {
    const child = spawn(binary, ["--home", home, "capture", "opencode"], {
      stdio: ["pipe", "ignore", "ignore"],
      timeout: 1500,
    });
    child.on("error", resolve);
    child.on("close", resolve);
    child.stdin.on("error", () => {});
    child.stdin.end(JSON.stringify(payload));
  });
}

export default {
  id: "tuna",
  async setup(ctx) {
    if (process.env.TUNA_INTERNAL === "1") return;
    await ctx.mcp.transform((editor) => {
      editor.set("tuna", { type: "local", command: [binary, "--home", home, "mcp"] });
    });
    await ctx.session.hook("prompt", async (event) => {
      if (event.prompt.text.startsWith("TUNA_INTERNAL_ANALYSIS_V1")) return;
      try {
        const [session, messages] = await Promise.all([
          ctx.session.get({ sessionID: event.sessionID }).catch(() => undefined),
          ctx.session.context({ sessionID: event.sessionID }).catch(() => []),
        ]);
        const previous = messages.findLast((m) => m.type === "assistant");
        const model = previous?.model ?? session?.model;
        await capture({
          key: event.messageID,
          session_id: event.sessionID,
          prompt: event.prompt.text,
          model: model ? `${model.providerID}/${model.id}` : "unknown",
          cwd: session?.location.directory ?? ctx.location.directory,
          context: previous?.content.filter((p) => p.type === "text").map((p) => p.text).join("\n").slice(-12000),
          timestamp: new Date().toISOString(),
        });
      } catch {
        // Capture is best-effort and must never interrupt prompt admission.
      }
    });
  },
};
