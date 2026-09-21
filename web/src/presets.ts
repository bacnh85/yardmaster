/** Provider registry: known API-key providers, shown as cards even when unconfigured.
 *  A registry provider maps to one or more config providers (Zen needs one per wire). */
export interface RegistryEntry {
  name: string;      // config provider name to create, e.g. "opencode-go"
  wire: "openai" | "anthropic" | "responses";
  base_url: string;
  session?: string;
  family: string;    // chat | anthropic | responses — which catalog models this entry serves
  familyLabel: string;
}

export interface RegistryProvider {
  id: string;        // preset id stored on config providers
  title: string;     // display name, includes short code
  desc: string;
  code: string;      // short code: monogram + connection label prefix (e.g. "OCG")
  prefix: string;    // routing prefix; models exposed to agents as "prefix/model"
  plans?: boolean;   // catalog supports plan-tier filtering (cmdPlan)
  entries: RegistryEntry[];
}

export const REGISTRY: RegistryProvider[] = [
  {
    id: "opencode-go", title: "OpenCode Go (OCG)", code: "OCG", prefix: "ocg",
    desc: "OpenCode's model gateway — Claude, GPT/Grok, DeepSeek, GLM, Kimi, Qwen + free models",
    entries: [
      { name: "opencode-go", wire: "openai", base_url: "https://opencode.ai/zen/go/v1", session: "opencode", family: "chat", familyLabel: "DeepSeek · GLM · Kimi · MiniMax · free" },
      { name: "opencode-go-claude", wire: "anthropic", base_url: "https://opencode.ai/zen/go/v1", session: "opencode", family: "anthropic", familyLabel: "Claude · Qwen" },
      { name: "opencode-go-gpt", wire: "responses", base_url: "https://opencode.ai/zen/go/v1", session: "opencode", family: "responses", familyLabel: "GPT · Grok" },
    ],
  },
  {
    id: "deepseek", title: "DeepSeek (DS)", code: "DS", prefix: "ds",
    desc: "DeepSeek API — deepseek-flash / deepseek-v4-pro, 1M context, all three wires",
    entries: [
      { name: "deepseek", wire: "openai", base_url: "https://api.deepseek.com", family: "chat", familyLabel: "DeepSeek V4" },
      { name: "deepseek-claude", wire: "anthropic", base_url: "https://api.deepseek.com/anthropic", family: "anthropic", familyLabel: "DeepSeek (Claude wire)" },
      { name: "deepseek-gpt", wire: "responses", base_url: "https://api.deepseek.com", family: "responses", familyLabel: "DeepSeek (Responses wire)" },
    ],
  },
  {
    id: "zai", title: "Z.AI (GLM Coding Plan)", code: "ZAI", prefix: "zai",
    desc: "GLM models over the Anthropic wire with cache-control injection and dispatch spacing",
    entries: [
      { name: "zai", wire: "anthropic", base_url: "https://api.z.ai/api/anthropic", family: "anthropic", familyLabel: "GLM" },
    ],
  },
  {
    id: "cmdcode", title: "Command Code (CC)", code: "CC", prefix: "cmd", plans: true,
    desc: "Command Code Provider API — 50+ open models on GOAT, Claude/GPT/Gemini on Pro/Max",
    entries: [
      { name: "cmdcode", wire: "openai", base_url: "https://api.commandcode.ai/provider/v1", family: "chat", familyLabel: "Open models (GOAT+)" },
      { name: "cmdcode-claude", wire: "anthropic", base_url: "https://api.commandcode.ai/provider/v1", family: "anthropic", familyLabel: "Claude (Pro/Max)" },
    ],
  },
];

// Command Code plan tiers, by live catalog id (api.commandcode.ai/provider/v1/models,
// lowercase; verified 2026-09-20). Ids CommandCode adds later fall to "" —
// shown only under the "all" filter and never swept by plan expose-alls until
// filed into the right list here.
const CC_GOAT = [
  "z-ai/glm-5.3-flash", "z-ai/glm-5.3-flashx", "zai-org/glm-5.3", "zai-org/glm-5.2",
  "zai-org/glm-5.2-fast", "zai-org/glm-5.1", "zai-org/glm-5",
  "deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-flash-vision-exp",
  "deepseek/deepseek-v4-flash-fast", "deepseek/deepseek-v4.1-flash",
  "moonshotai/kimi-k3", "moonshotai/kimi-k2.7-code", "moonshotai/kimi-k2.7-code-highspeed",
  "moonshotai/kimi-k2.6", "moonshotai/kimi-k2.5",
  "minimaxai/minimax-m3", "minimaxai/minimax-m2.7", "minimaxai/minimax-m2.5",
  "xiaomi/mimo-v2.5", "xiaomi/mimo-v2.5-pro",
  "qwen/qwen3.8-omni-flash", "qwen/qwen3.8-max-0902", "qwen/qwen3.8-max", "qwen/qwen3.8-27b",
  "qwen/qwen3.8-flash", "qwen/qwen3.7-max", "qwen/qwen3.7-plus", "qwen/qwen3.7-flash",
  "qwen/qwen3.6-max-preview", "qwen/qwen3.6-plus",
  "meituan/longcat-2.0", "stepfun/step-3.7-flash", "stepfun/step-3.5-flash",
  "tencent/hy3-paid", "tencent/hy4-preview",
  "google/gemini-3.8-flash", "google/gemini-3.7-flash",
  "nvidia/nemotron-3-ultra-550b-a55b",
  "thinkingmachines/inkling", "thinkingmachines/inkling-small",
  "poolside/laguna-s-2.1-free", "inclusionai/ling-3.0-flash-sante:free",
  "meta/muse-spark-1.2", "meta/muse-spark-1.2-contributor", "meta/muse-spark-1.3", "meta/muse-spark-1.3-contributor",
  "xai/grok-4.5", "xai/grok-4.6", "gpt-5.6-sol", "gpt-5.6-luna",
];
const CC_PRO = [
  "claude-sonnet-5", "claude-sonnet-4-6", "claude-haiku-4-5-20251001",
  "gpt-5.6-terra", "gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex",
  "google/gemini-3.6-flash", "google/gemini-3.5-flash", "google/gemini-3.5-flash-lite", "google/gemini-3.1-flash-lite",
  "meta/muse-spark-1.1",
];
const CC_MAX = [
  "claude-fable-5", "claude-fable-5-1", "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "sakana/fugu-ultra",
];

export type CmdPlan = "goat" | "pro" | "max" | ""; // "" = new/unclassified

/** Plan tier for a CommandCode catalog id (case-insensitive). */
export function cmdPlan(id: string): CmdPlan {
  const k = id.toLowerCase();
  if (CC_MAX.includes(k)) return "max";
  if (CC_PRO.includes(k)) return "pro";
  if (CC_GOAT.includes(k)) return "goat";
  return "";
}

/** Find a config provider's registry entry: by preset id, else by exact name (legacy configs). */
export function registryFor(p: { preset: string; name: string }): RegistryProvider | undefined {
  if (p.preset) {
    const byPreset = REGISTRY.find((r) => r.id === p.preset);
    if (byPreset) return byPreset;
    // unknown preset id (removed/renamed) — fall through to name matching
  }
  return REGISTRY.find((r) => r.entries.some((e) => e.name === p.name));
}

/** The registry entry a config provider implements (wire family disambiguates groups). */
export function entryFor(r: RegistryProvider, p: { preset: string; name: string; wire: string }): RegistryEntry | undefined {
  const byName = r.entries.find((e) => e.name === p.name);
  if (byName) return byName;
  return r.entries.find((e) => e.wire === p.wire);
}

/** Which catalog family a provider wire serves (preselects filters). */
export const wireFamily = (wire: string): string =>
  wire === "anthropic" ? "anthropic" : wire === "responses" ? "responses" : "chat";

/** Form fields a registry entry fills (legacy single-entry helper, still used by custom form). */
export const presetToForm = (e: RegistryEntry) => ({
  name: e.name,
  wire: e.wire,
  base_url: e.base_url,
  session: e.session ?? "",
});
