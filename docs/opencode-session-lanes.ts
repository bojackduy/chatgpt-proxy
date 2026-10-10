import type { Plugin } from "@opencode-ai/plugin"

// Scope this protocol to the local ChatGPT bridge provider; unrelated providers
// and existing headers remain untouched. Primary agent switches stay "main".
const server: Plugin = async () => ({
  "chat.headers": async (input, output) => {
    if (input.model.providerID !== "chatgpt-web") return
    output.headers["X-OpenCode-Request-Lane"] =
      ["title", "summary", "compaction"].includes(input.agent) ? input.agent : "main"
  },
})

export default { id: "chatgpt-proxy.session-lanes", server }
