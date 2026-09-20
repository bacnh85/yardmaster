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
    desc: "DeepSeek API — deepseek-chat / reasoner on the OpenAI wire",
    entries: [
      { name: "deepseek", wire: "openai", base_url: "https://api.deepseek.com", family: "chat", familyLabel: "DeepSeek V4" },
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
    id: "cmdcode", title: "Command Code (CC)", code: "CC", prefix: "cmd",
    desc: "Command Code provider API",
    entries: [
      { name: "cmdcode", wire: "openai", base_url: "https://api.commandcode.ai/provider/v1", family: "chat", familyLabel: "Command Code models" },
    ],
  },
];

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
