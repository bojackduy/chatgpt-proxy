import type { Plugin } from "@opencode-ai/plugin"
import type { Plugin as V2Plugin } from "@opencode/plugin"

// Scope this protocol to the local ChatGPT bridge provider; unrelated providers
// and existing headers remain untouched. Primary agent switches stay "main".
const server: Plugin = async () => ({
  "chat.headers": async (input, output) => {
    if (input.model.providerID !== "chatgpt-web") return
    output.headers["X-OpenCode-Request-Lane"] =
      ["title", "summary", "compaction"].includes(input.agent) ? input.agent : "main"
  },
})

// ─── v2 (opencode v2 host) ─────────────────────────────────────────────────
// v2 has no chat.headers hook: per-request headers cannot be injected. Lane
// identity therefore travels as STATIC per-entry headers instead — one
// provider entry per auxiliary lane, all serving the same chatgpt-web model
// against the same adapter. Agents already on chatgpt-web are retargeted to
// their lane entry; every other agent (ollama, etc.) is left untouched.
// Shape parity with the manual `chatgpt-web` provider entry is intentional:
// same model id, same input modalities, same limits.
const ADAPTER_BASE_URL = "http://127.0.0.1:8788/v1"
const V2_PROVIDER_PACKAGE = "@opencode/ai/providers/openai-compatible"

const LANE_MAIN = "chatgpt-web"
const LANES = ["title", "summary", "compaction"] as const

function laneModel(): any {
  return {
    id: "chatgpt-web",
    modelID: "chatgpt-web",
    providerID: LANE_MAIN,
    name: "ChatGPT Web",
    capabilities: { tools: true, input: ["text", "image", "video"], output: ["text"] },
    variants: [],
    time: { released: 0 },
    cost: [],
    status: "active",
    enabled: true,
    limit: { context: 0, output: 32768 },
  }
}

function laneProviderEntry(lane: string): any {
  return {
    info: {
      id: lane === "main" ? LANE_MAIN : `${LANE_MAIN}-${lane}`,
      name: lane === "main" ? "ChatGPT Web (browser tool bridge)" : `ChatGPT Web (${lane} lane)`,
      activation: "enabled",
      package: V2_PROVIDER_PACKAGE,
      settings: {
        baseURL: ADAPTER_BASE_URL,
        timeout: false,
        ...(lane === "main" ? {} : { headers: { "X-OpenCode-Request-Lane": lane } }),
      },
    },
    models: [laneModel()],
  }
}

async function setup(context: V2Plugin.Context) {
  const registrations: Array<{ dispose(): Promise<void> }> = []
  registrations.push(
    await context.provider.transform((editor: any) => {
      // Fill-missing-only: the manual opencode.jsonc entry (or the
      // opencode-chatgpt plugin) owns `chatgpt-web` when present.
      for (const lane of ["main", ...LANES]) {
        const id = lane === "main" ? LANE_MAIN : `${LANE_MAIN}-${lane}`
        if (!editor.get(id)) editor.add(laneProviderEntry(lane))
      }
    }),
  )
  registrations.push(
    await context.agent.transform((editor: any) => {
      // Retarget ONLY agents explicitly on chatgpt-web. Anything else
      // (ollama title, other backends, unset models) is none of our business.
      // Idempotent: an already lane-mapped agent no longer matches the guard.
      for (const lane of LANES) {
        const agent = editor.get(lane)
        const model = agent?.model
        if (model && (model.providerID === LANE_MAIN || (model as any).id === LANE_MAIN)) {
          editor.update(lane, (a: any) => {
            a.model = { id: "chatgpt-web", providerID: `${LANE_MAIN}-${lane}` }
          })
        }
      }
    }),
  )
  return async () => {
    await Promise.allSettled(registrations.reverse().map((r) => r.dispose()))
  }
}

export default { id: "chatgpt-proxy.session-lanes", server, setup }
