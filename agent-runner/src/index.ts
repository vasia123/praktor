import { query } from "@anthropic-ai/claude-agent-sdk";
import { NatsBridge } from "./nats-bridge.js";
import { applyExtensions } from "./extensions.js";
import { readFileSync, readdirSync, mkdirSync, writeFileSync, rmSync, symlinkSync, existsSync, lstatSync, readlinkSync, unlinkSync } from "fs";
import { join } from "path";
import { execSync } from "child_process";
import { DatabaseSync } from "node:sqlite";

// Patch console to prepend timestamps matching gateway format (YYYY/MM/DD HH:MM:SS)
const origLog = console.log;
const origWarn = console.warn;
const origError = console.error;
function ts(): string {
  const d = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}/${pad(d.getMonth() + 1)}/${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
console.log = (...args: unknown[]) => origLog(ts(), ...args);
console.warn = (...args: unknown[]) => origWarn(ts(), ...args);
console.error = (...args: unknown[]) => origError(ts(), ...args);

const NATS_URL = process.env.NATS_URL || "nats://localhost:4222";
const AGENT_ID = process.env.AGENT_ID || process.env.GROUP_ID || "default";
const USER_ID = process.env.USER_ID || "";
const CLAUDE_MODEL = process.env.CLAUDE_MODEL || undefined;
const ALLOWED_TOOLS_ENV = process.env.ALLOWED_TOOLS || "";
const SWARM_CHAT_TOPIC = process.env.SWARM_CHAT_TOPIC || "";
const SWARM_ROLE = process.env.SWARM_ROLE || "";

let bridge: NatsBridge;
// Per-chat:agent session isolation
const sessionsByKey = new Map<string, string>(); // "chatID:agentName" → session_id
const processingChats = new Set<string>(); // chat_ids currently being processed
const pendingByChat = new Map<string, Array<Record<string, unknown>>>(); // per-chat message queues
const abortedChats = new Set<string>(); // chat_ids that have been aborted
const abortedTasks = new Set<string>(); // msg_ids of aborted scheduled tasks
const currentQueryIters = new Map<string, AsyncIterator<unknown>>(); // per-chat query iterators
let extensionMcpServers: Record<string, { type: string; command?: string; args?: string[]; url?: string; env?: Record<string, string>; headers?: Record<string, string> }> = {};

// Per-chat active project
const activeProjects = new Map<string, string>(); // chatID → projectName

// Agent config cache (fetched from host via IPC)
interface AgentConfig {
  model?: string;
  system_prompt?: string;
  allowed_tools?: string[];
}
const agentConfigCache = new Map<string, AgentConfig>();

// Parallel task execution
const MAX_PARALLEL_TASKS = parseInt(process.env.MAX_PARALLEL_TASKS || "3", 10);
let activeTaskCount = 0;
const activeQueries = new Map<string, AsyncIterator<unknown>>();
const pendingTasks: Array<Record<string, unknown>> = [];

// Swarm collaborative chat buffer
interface ChatMessage {
  from: string;
  content: string;
  timestamp: number;
}
const chatHistory: ChatMessage[] = [];

export function parseAllowedTools(env: string): string[] | undefined {
  if (!env) return undefined;
  const tools = env.split(",").map((t) => t.trim()).filter(Boolean);
  return tools.length > 0 ? tools : undefined;
}

function sessionKey(chatID: string, agentName: string): string {
  return `${chatID}:${agentName}`;
}

const SESSION_MAP_PATH = "/workspace/agent/.session-map.json";

function loadSessionMap(): void {
  try {
    const data = readFileSync(SESSION_MAP_PATH, "utf-8");
    const loaded = JSON.parse(data) as Record<string, string>;
    for (const [key, sessionId] of Object.entries(loaded)) {
      sessionsByKey.set(key, sessionId);
    }
    console.log(`[agent] loaded ${sessionsByKey.size} session(s) from disk`);
  } catch {
    // No existing session map or parse error — start fresh
  }
}

function saveSessionMap(): void {
  try {
    writeFileSync(SESSION_MAP_PATH, JSON.stringify(Object.fromEntries(sessionsByKey)));
  } catch (err) {
    console.warn("[agent] failed to save session map:", err);
  }
}

function installGlobalInstructions(): void {
  // Write global instructions to ~/.claude/CLAUDE.md (user-level).
  // Claude Code automatically loads both user-level and project-level CLAUDE.md,
  // so we only need to write the global one here. The per-agent CLAUDE.md in
  // /workspace/agent/ is loaded automatically as the project-level file.
  try {
    const global = readFileSync("/workspace/global/CLAUDE.md", "utf-8");
    const userClaudeDir = "/home/praktor/.claude";
    mkdirSync(userClaudeDir, { recursive: true });
    writeFileSync(`${userClaudeDir}/CLAUDE.md`, global);
    console.log(`[agent] installed global instructions to ${userClaudeDir}/CLAUDE.md`);
  } catch (err) {
    console.warn("[agent] could not install global instructions:", err);
  }
}

const agentMdTemplate = `# Agent Identity

## Name
(Agent display name)

## Vibe
(Personality, communication style)

## Expertise
(Areas of specialization)
`;

function ensureAgentMd(): void {
  const path = "/workspace/agent/AGENT.md";
  if (!existsSync(path)) {
    try {
      writeFileSync(path, agentMdTemplate);
      console.log("[agent] created AGENT.md template");
    } catch (err) {
      console.warn("[agent] could not create AGENT.md:", err);
    }
  }
}

function ensureWorkspace(): void {
  const wsRoot = "/workspace/agent";
  const claudeMd = join(wsRoot, "CLAUDE.md");
  if (!existsSync(claudeMd)) {
    mkdirSync(join(wsRoot, "projects"), { recursive: true });
    mkdirSync(join(wsRoot, "uploads"), { recursive: true });
    writeFileSync(claudeMd, "# Workspace\n\n## Projects\n\nNo projects yet.\n");
    console.log("[agent] initialized workspace structure");
  }
}

function setupAgentBrowser(): void {
  const skillSource = "/usr/local/lib/node_modules/agent-browser/skills/agent-browser";
  const configSource = "/usr/local/share/agent-browser/config.json";
  if (!existsSync(skillSource)) return; // agent-browser not installed

  try {
    const skillsDir = "/home/praktor/.claude/skills";
    mkdirSync(skillsDir, { recursive: true });

    // Remove stale playwright-cli symlink from previous image versions
    const staleLink = join(skillsDir, "playwright-cli");
    try {
      if (lstatSync(staleLink).isSymbolicLink() && readlinkSync(staleLink) === "/opt/playwright-cli/skill") {
        unlinkSync(staleLink);
        console.log("[agent] removed stale playwright-cli skill symlink");
      }
    } catch { /* doesn't exist */ }

    // Force-update skill symlink
    const skillLink = join(skillsDir, "agent-browser");
    try { unlinkSync(skillLink); } catch { /* doesn't exist */ }
    symlinkSync(skillSource, skillLink);

    // Force-update config symlink
    const configDir = "/home/praktor/.agent-browser";
    mkdirSync(configDir, { recursive: true });
    const configLink = join(configDir, "config.json");
    try { unlinkSync(configLink); } catch { /* doesn't exist */ }
    symlinkSync(configSource, configLink);

    console.log("[agent] agent-browser configured");
  } catch (err) {
    console.warn("[agent] could not configure agent-browser:", err);
  }
}

function setupAgentMail(): void {
  const skillSource = "/opt/agentmail-skill";
  if (!process.env.AGENTMAIL_API_KEY || !existsSync(join(skillSource, "SKILL.md"))) return;

  try {
    const skillsDir = "/home/praktor/.claude/skills";
    mkdirSync(skillsDir, { recursive: true });

    const skillLink = join(skillsDir, "agentmail-cli");
    try { unlinkSync(skillLink); } catch { /* doesn't exist */ }
    symlinkSync(skillSource, skillLink);

    console.log("[agent] agentmail-cli skill configured");
  } catch (err) {
    console.warn("[agent] could not configure agentmail-cli:", err);
  }
}

async function getAgentConfig(agentName: string, userId: string): Promise<AgentConfig> {
  const cacheKey = `${userId}:${agentName}`;
  const cached = agentConfigCache.get(cacheKey);
  if (cached) return cached;

  try {
    const { sendIPC } = await import("./ipc.js");
    const resp = await sendIPC("get_agent_config", { user_id: userId, agent_name: agentName });
    if (resp.ok) {
      const config: AgentConfig = {
        model: (resp as any).model || undefined,
        system_prompt: (resp as any).system_prompt || undefined,
        allowed_tools: (resp as any).allowed_tools || undefined,
      };
      agentConfigCache.set(cacheKey, config);
      return config;
    }
    console.warn(`[agent] get_agent_config failed: ${resp.error}`);
  } catch (err) {
    console.warn("[agent] get_agent_config IPC error:", err);
  }
  return {};
}

function loadSystemPrompt(includeIdentity = true, meta?: Record<string, string | undefined>, agentSystemPrompt?: string): string {
  const parts: string[] = [];

  // Agent-specific system prompt from DB (if provided)
  if (agentSystemPrompt) {
    parts.push(agentSystemPrompt);
  }

  // User profile (loaded before global instructions so agents know the user)
  try {
    const user = readFileSync("/workspace/global/USER.md", "utf-8");
    parts.push(user);
  } catch {
    // User profile not available
  }

  // Agent identity (excluded for routing queries to avoid personality bleed)
  if (includeIdentity) {
    try {
      const agent = readFileSync("/workspace/agent/AGENT.md", "utf-8");
      parts.push(
        "The following is your agent identity. " +
        "This is stored at /workspace/agent/AGENT.md and you can update it " +
        "anytime using the Edit or Write tool (e.g. to set your name, vibe, or expertise).\n\n" +
        agent
      );
    } catch {
      // Agent identity not available
    }
  }

  // Include global instructions in system prompt as well (belt and suspenders)
  try {
    const global = readFileSync("/workspace/global/CLAUDE.md", "utf-8");
    parts.push(global);
  } catch {
    // Global instructions not available
  }

  // Project context: load project CLAUDE.md if in a project
  if (meta?.active_project) {
    try {
      const projectMd = readFileSync(`/workspace/agent/projects/${meta.active_project}/CLAUDE.md`, "utf-8");
      parts.push(`PROJECT: ${meta.active_project}\n\n${projectMd}`);
    } catch {
      // Project CLAUDE.md not available
    }
  }

  // Projects list
  try {
    const projectsDir = "/workspace/agent/projects";
    if (existsSync(projectsDir)) {
      const entries = readdirSync(projectsDir, { withFileTypes: true });
      const projects = entries.filter(e => e.isDirectory()).map(e => e.name);
      if (projects.length > 0) {
        parts.push(`PROJECTS\nAvailable projects: ${projects.join(", ")}\nUse project_switch MCP tool to change the active project.`);
      }
    }
  } catch {
    // Projects dir not accessible
  }

  // Nix package manager: detect nix-daemon and inform agent
  try {
    execSync("pgrep -l nix-daemon", { timeout: 5000 });
    parts.push(
      "NIX PACKAGE MANAGER — You have the nix package manager available.\n" +
      "- When a task requires a tool or language not present in the container, use nix to install it.\n" +
      "- Use the `nix_search` MCP tool to find packages, and `nix_add` to install them.\n" +
      "- Use `nix_list_installed` to see what's already installed.\n" +
      "- Example: if asked to run a Python script and python is missing, install it with nix_add(package: \"python3\") first.\n" +
      "- Always check if a command exists before installing (e.g. `which python3`)."
    );
  } catch {
    // nix-daemon not running, skip
  }

  // Messaging: explain how agent responses reach the user
  parts.push(
    "MESSAGING — Your text responses are automatically delivered to the user via Telegram.\n" +
    "- To send a message, simply reply with text — no special tool is needed.\n" +
    "- The file_send tool is ONLY for sending binary files (images, PDFs, etc.), NOT for text messages. NEVER create .txt files to deliver text content.\n" +
    "- When executing scheduled tasks, your text reply IS the notification the user receives.\n" +
    "- Keep scheduled task replies short and direct — the user sees them as Telegram messages."
  );

  // Telegram formatting: instruct agent to use Telegram-compatible Markdown
  parts.push(
    "TELEGRAM FORMATTING — Your messages are rendered in Telegram, which only supports MarkdownV1.\n" +
    "- Bold: *text* (single asterisks, NOT **double**)\n" +
    "- Italic: _text_\n" +
    "- Inline code: `code`\n" +
    "- Code blocks: ```code```\n" +
    "- Links: [text](url)\n" +
    "- DO NOT use: # headers, - bullet lists, --- horizontal rules, ![]()" +
    " image embeds — these render as raw text in Telegram.\n" +
    "- Instead of headers, use *bold text* on its own line.\n" +
    "- Instead of bullet lists with - or *, use • (bullet character) or numbered lists."
  );

  // Security: prevent agents from revealing secret values
  parts.push(
    "SECURITY — MANDATORY RULES:\n" +
    "- NEVER reveal, print, or include the values of environment variables that contain secrets, tokens, API keys, passwords, or credentials.\n" +
    "- NEVER read or output the contents of secret files (e.g. service account JSON files, SSH keys, certificates).\n" +
    "- NEVER include secrets, tokens, API keys, passwords, or credentials in emails. The same redaction rules apply to email as to Telegram.\n" +
    "- If the user asks for a secret value, respond with [REDACTED] in place of the value and explain that secrets cannot be disclosed.\n" +
    "- You may confirm that a secret or env var EXISTS, but must NEVER show its value — always use [REDACTED] as placeholder."
  );

  // Telegram context: inform agent about Telegram environment and formatting
  if (meta?.chat_id) {
    let tg = "TELEGRAM\n";
    tg += "You are communicating with users via Telegram.\n\n";
    tg += "Response behavior:\n";
    tg += "- Your normal text response is automatically sent as a Telegram message. This is the primary way to reply.\n";
    tg += "- Do NOT use telegram_send_message for your main reply — it would result in a duplicate message.\n";
    tg += "- Use praktor-telegram MCP tools ONLY for special actions: polls, stickers, reactions, pinning, editing, replying to specific messages, forwarding, etc.\n\n";

    tg += "Formatting — CRITICAL:\n";
    tg += "Your text output goes directly to Telegram. You MUST use Telegram MarkdownV1 formatting, NOT standard Markdown.\n\n";
    tg += "Telegram MarkdownV1 rules:\n";
    tg += "- Bold: *bold text* (single asterisks, NOT **double**)\n";
    tg += "- Italic: _italic text_ (underscores)\n";
    tg += "- Inline code: `code` (backticks)\n";
    tg += "- Code block: ```code block``` (triple backticks, optional language)\n";
    tg += "- Links: [text](url)\n";
    tg += "- Messages are auto-chunked at 4096 chars.\n\n";
    tg += "NOT supported in Telegram (DO NOT USE):\n";
    tg += "- Headers (# ## ###) — use *bold text* on a separate line instead\n";
    tg += "- Bullet lists with - or * at line start — use • (bullet character) or plain numbered lists\n";
    tg += "- Markdown tables — describe data as text or use code blocks for alignment\n";
    tg += "- Horizontal rules (---)\n";
    tg += "- Image embeds ![alt](url) — use telegram_send_photo_url tool instead\n\n";

    // User info
    const userParts = [meta.first_name, meta.last_name].filter(Boolean);
    if (meta.username) userParts.push(`(@${meta.username})`);
    if (userParts.length) tg += `Current user: ${userParts.join(" ")}\n`;
    // Chat info
    if (meta.chat_type) {
      tg += `Chat type: ${meta.chat_type}`;
      if (meta.chat_title) tg += ` "${meta.chat_title}"`;
      tg += "\n";
    }
    tg += `Chat ID: ${meta.chat_id}\n`;
    if (meta.msg_id) tg += `Last message ID: ${meta.msg_id}\n`;
    parts.push(tg);
  }

  // Memory: list existing keys so the agent knows what's stored
  try {
    const MEMORY_DB_PATH = "/workspace/agent/memory.db";
    let memorySection =
      "MEMORY — You have persistent memory via MCP tools (memory_store, memory_recall, memory_forget, memory_delete, memory_list).\n" +
      "- To remember: call memory_store with a short key and content\n" +
      "- To recall: call memory_recall with a keyword to search\n" +
      "- To forget: call memory_forget with a search query";

    if (existsSync(MEMORY_DB_PATH)) {
      const db = new DatabaseSync(MEMORY_DB_PATH);
      // access_count may not exist yet on older databases
      let rows: Array<{ key: string; tags: string; access_count?: number }>;
      try {
        rows = db.prepare(
          "SELECT key, tags, access_count FROM memories ORDER BY updated_at DESC"
        ).all() as typeof rows;
      } catch {
        rows = db.prepare(
          "SELECT key, tags FROM memories ORDER BY updated_at DESC"
        ).all() as typeof rows;
      }
      db.close();

      if (rows.length > 0) {
        memorySection += `\n\nYou currently have ${rows.length} stored memories:\n`;
        memorySection += rows
          .map((r) => {
            let line = `- ${r.key}`;
            if (r.tags) line += ` [${r.tags}]`;
            if (r.access_count) line += ` (${r.access_count}x)`;
            return line;
          })
          .join("\n");
        memorySection += "\n\nCall memory_recall with a relevant keyword to retrieve full content before answering.";
        memorySection += " memory_recall uses hybrid search combining keyword matching with semantic similarity — use natural language queries for best results.";
      }
    }
    parts.push(memorySection);
  } catch (err) {
    console.warn("[agent] could not load memory keys:", err);
  }

  // agent-browser: inform agent it's pre-installed with system chromium
  if (existsSync("/usr/local/lib/node_modules/agent-browser")) {
    parts.push(
      "AGENT-BROWSER — Pre-installed and configured. Do NOT install browsers via npm, npx, nix, or any other method.\n" +
      "- `agent-browser` is already in PATH and ready to use.\n" +
      "- It is configured to use the system Chromium at `/usr/bin/chromium-browser`.\n" +
      "- Run `agent-browser open <url>` to start a browser session, then `agent-browser snapshot -i` to see the page.\n" +
      "- The browser persists across messages. Reuse the existing session.\n" +
      "- When executing a scheduled task, ALWAYS run `agent-browser close` when done to free resources."
    );
  }

  // AgentMail: inbox-locked restrictions when configured
  if (process.env.AGENTMAIL_API_KEY && process.env.AGENTMAIL_INBOX_ID) {
    parts.push(
      "AGENTMAIL — MANDATORY RULES:\n" +
      `- Your inbox ID is: ${process.env.AGENTMAIL_INBOX_ID}. You MUST use ONLY this inbox ID for ALL agentmail operations.\n` +
      "- NEVER use, access, list, or reference any other inbox ID, even if the user asks.\n" +
      "- NEVER create new inboxes.\n" +
      "- NEVER use pods, webhooks, or domains commands. These are admin-only operations.\n" +
      "- NEVER include secrets, tokens, API keys, passwords, or credentials in emails.\n" +
      "- The same secret redaction rules that apply to Telegram apply to email — use [REDACTED] for any secret values.\n" +
      "- EMAIL FORMATTING: Emails are NOT Telegram messages. Do NOT use Telegram Markdown formatting or escape characters in emails. " +
      "Write emails in plain text with natural punctuation. No backslash escaping, no *bold*, no `code` — just normal text."
    );
  }

  // Skills: load installed SKILL.md files into prompt
  const skillsDir = "/home/praktor/.claude/skills";
  try {
    if (existsSync(skillsDir)) {
      const entries = readdirSync(skillsDir, { withFileTypes: true });
      for (const entry of entries) {
        if (!entry.isDirectory()) continue;
        const skillMd = join(skillsDir, entry.name, "SKILL.md");
        try {
          const content = readFileSync(skillMd, "utf-8");
          parts.push(`SKILL: ${entry.name}\n\n${content}`);
        } catch {
          // SKILL.md not found in this directory, skip
        }
      }
    }
  } catch {
    // skills directory not accessible, skip
  }

  return parts.join("\n\n---\n\n");
}

function buildQueryOptions(prompt: string, sessionId?: string) {
  const systemPrompt = loadSystemPrompt();
  const cwd = "/workspace/agent";
  const configuredTools = parseAllowedTools(ALLOWED_TOOLS_ENV);
  const allowedTools = configuredTools || [
    "Bash",
    "Read",
    "Write",
    "Edit",
    "Glob",
    "Grep",
    "WebSearch",
    "WebFetch",
    "Task",
    "TaskOutput",
    "mcp__praktor-*",
  ];

  return {
    prompt,
    options: {
      model: CLAUDE_MODEL,
      cwd,
      pathToClaudeCodeExecutable: "/usr/local/bin/claude",
      systemPrompt: systemPrompt || undefined,
      ...(sessionId ? { resume: sessionId } : {}),
      allowedTools,
      mcpServers: {
        "praktor-tasks": {
          type: "stdio",
          command: "node",
          args: ["/app/mcp-tasks.mjs"],
          env: { NATS_URL, AGENT_ID },
        },
        "praktor-profile": {
          type: "stdio",
          command: "node",
          args: ["/app/mcp-profile.mjs"],
          env: { NATS_URL, AGENT_ID },
        },
        "praktor-memory": {
          type: "stdio",
          command: "node",
          args: ["/app/mcp-memory.mjs"],
          env: {},
        },
        "praktor-nix": {
          type: "stdio",
          command: "node",
          args: ["/app/mcp-nix.mjs"],
          env: {},
        },
        "praktor-file": {
          type: "stdio",
          command: "node",
          args: ["/app/mcp-file.mjs"],
          env: { NATS_URL, AGENT_ID },
        },
        "praktor-history": {
          type: "stdio",
          command: "node",
          args: ["/app/mcp-history.mjs"],
          env: { NATS_URL, AGENT_ID },
        },
        ...(SWARM_CHAT_TOPIC ? {
          "praktor-swarm": {
            type: "stdio",
            command: "node",
            args: ["/app/mcp-swarm.mjs"],
            env: { NATS_URL, AGENT_ID, SWARM_CHAT_TOPIC },
          },
        } : {}),
        ...extensionMcpServers,
      },
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      stderr: (data: string) => {
        console.error(`[claude-stderr] ${data.trimEnd()}`);
      },
    },
  };
}

// Execute a scheduled task in parallel (fresh session, no resume)
async function executeTask(data: Record<string, unknown>): Promise<void> {
  const text = data.text as string;
  const msgId = data.msg_id as string | undefined;
  console.log(`[task] executing parallel task: ${text.substring(0, 100)}...`);

  try {
    const opts = buildQueryOptions(text);
    const result = query(opts);

    let fullResponse = "";
    const iter = result[Symbol.asyncIterator]();
    if (msgId) activeQueries.set(msgId, iter);

    try {
      for await (const event of { [Symbol.asyncIterator]: () => iter }) {
        if (event.type === "result" && event.subtype === "success") {
          fullResponse = event.result;
        } else if (event.type === "assistant") {
          for (const block of event.message.content) {
            if (block.type === "text") {
              await bridge.publishOutput(block.text, "text", msgId);
            } else if (block.type === "tool_use" || block.type === "server_tool_use") {
              console.log(`[task] tool: ${block.name}`);
            }
          }
        }
      }
    } catch (streamErr) {
      if (fullResponse) {
        console.warn(`[task] claude process exited with error after successful result, ignoring:`, streamErr);
      } else {
        throw streamErr;
      }
    }

    if (fullResponse && !(msgId && abortedTasks.has(msgId))) {
      await bridge.publishResult(fullResponse, msgId);
    }
    console.log(`[task] completed`);
  } catch (err) {
    if (msgId && abortedTasks.has(msgId)) {
      console.log("[task] aborted");
      return;
    }
    const errorMsg = err instanceof Error ? err.message : String(err);
    console.error(`[task] error:`, err);
    await bridge.publishResult(`Error: ${errorMsg}`, msgId);
  } finally {
    if (msgId) {
      activeQueries.delete(msgId);
      abortedTasks.delete(msgId);
    }
    activeTaskCount--;
    // Dequeue next pending task
    if (pendingTasks.length > 0) {
      const next = pendingTasks.shift()!;
      activeTaskCount++;
      console.log(`[task] dequeuing next task (${pendingTasks.length} remaining)`);
      executeTask(next);
    }
  }
}

async function handleMessage(data: Record<string, unknown>): Promise<void> {
  const text = data.text as string;
  if (!text) return;

  const chatID = (data.chat_id as string) || "_default";
  const agentName = (data.agent_name as string) || "";
  const userId = (data.user_id as string) || USER_ID;
  const sender = data.sender as string | undefined;
  const msgId = data.msg_id as string | undefined;

  // Scheduled tasks run in parallel with fresh sessions
  if (sender === "scheduler") {
    if (activeTaskCount >= MAX_PARALLEL_TASKS) {
      pendingTasks.push(data);
      console.log(`[task] at capacity (${activeTaskCount}/${MAX_PARALLEL_TASKS}), queued (${pendingTasks.length} pending)`);
      return;
    }
    activeTaskCount++;
    executeTask(data);
    return;
  }

  // Per-chat serialization: queue if this chat is already being processed
  if (processingChats.has(chatID)) {
    const queue = pendingByChat.get(chatID) || [];
    queue.push(data);
    pendingByChat.set(chatID, queue);
    console.log(`[agent] chat ${chatID} busy, queued (${queue.length} pending)`);
    return;
  }

  processingChats.add(chatID);
  abortedChats.delete(chatID);
  console.log(`[agent] processing message for agent ${agentName || AGENT_ID} chat ${chatID}: ${text.substring(0, 100)}...`);

  try {
    // Fetch agent config from host if we have a user_id and agent_name
    let agentConfig: AgentConfig = {};
    if (userId && agentName) {
      agentConfig = await getAgentConfig(agentName, userId);
    }

    // Extract meta fields from the incoming data for system prompt context
    const meta: Record<string, string | undefined> = {};
    for (const key of ["chat_id", "msg_id", "chat_type", "username", "first_name", "last_name", "chat_title", "reply_to_msg_id", "reply_to_text"]) {
      if (typeof data[key] === "string") meta[key] = data[key] as string;
    }

    // Set active project in meta
    const activeProject = activeProjects.get(chatID);
    if (activeProject) {
      meta.active_project = activeProject;
    }

    const systemPrompt = loadSystemPrompt(true, Object.keys(meta).length > 0 ? meta : undefined, agentConfig.system_prompt);

    // Determine cwd: per-project if active
    let cwd = "/workspace/agent";
    if (activeProject) {
      const projectDir = `/workspace/agent/projects/${activeProject}`;
      if (existsSync(projectDir)) {
        cwd = projectDir;
      }
    }

    // Prepend swarm chat context if in collaborative mode
    let augmentedText = text;
    if (SWARM_CHAT_TOPIC && chatHistory.length > 0) {
      const chatContext = chatHistory
        .map((m) => `[${m.from}]: ${m.content}`)
        .join("\n");
      augmentedText = `## Collaborative Chat History\n\n${chatContext}\n\n---\n\n${text}`;
      console.log(`[agent] prepended ${chatHistory.length} chat messages to prompt`);
    }

    console.log(`[agent] starting claude query, cwd=${cwd}, chat=${chatID}, agent=${agentName}`);

    // Determine model: agent config > env > default
    const model = agentConfig.model || CLAUDE_MODEL;

    // Determine allowed tools: agent config > env > default
    const configuredTools = agentConfig.allowed_tools
      ? agentConfig.allowed_tools
      : parseAllowedTools(ALLOWED_TOOLS_ENV);
    const allowedTools = configuredTools || [
      "Bash",
      "Read",
      "Write",
      "Edit",
      "Glob",
      "Grep",
      "WebSearch",
      "WebFetch",
      "Task",
      "TaskOutput",
      "mcp__praktor-*",
    ];

    // Session key: chatID:agentName for per-agent isolation
    const sessKey = sessionKey(chatID, agentName || AGENT_ID);
    const chatSessionId = sessionsByKey.get(sessKey);

    const result = query({
      prompt: augmentedText,
      options: {
        model,
        cwd,
        pathToClaudeCodeExecutable: "/usr/local/bin/claude",
        systemPrompt: systemPrompt || undefined,
        ...(chatSessionId ? { resume: chatSessionId } : {}),
        allowedTools,
        mcpServers: {
          "praktor-tasks": {
            type: "stdio",
            command: "node",
            args: ["/app/mcp-tasks.mjs"],
            env: { NATS_URL, AGENT_ID },
          },
          "praktor-profile": {
            type: "stdio",
            command: "node",
            args: ["/app/mcp-profile.mjs"],
            env: { NATS_URL, AGENT_ID },
          },
          "praktor-memory": {
            type: "stdio",
            command: "node",
            args: ["/app/mcp-memory.mjs"],
            env: {},
          },
          "praktor-nix": {
            type: "stdio",
            command: "node",
            args: ["/app/mcp-nix.mjs"],
            env: {},
          },
          "praktor-file": {
            type: "stdio",
            command: "node",
            args: ["/app/mcp-file.mjs"],
            env: { NATS_URL, AGENT_ID },
          },
          "praktor-history": {
            type: "stdio",
            command: "node",
            args: ["/app/mcp-history.mjs"],
            env: { NATS_URL, AGENT_ID },
          },
          "praktor-telegram": {
            type: "stdio",
            command: "node",
            args: ["/app/mcp-telegram.mjs"],
            env: { NATS_URL, AGENT_ID },
          },
          "praktor-projects": {
            type: "stdio",
            command: "node",
            args: ["/app/mcp-projects.mjs"],
            env: { NATS_URL, AGENT_ID },
          },
          ...(SWARM_CHAT_TOPIC ? {
            "praktor-swarm": {
              type: "stdio",
              command: "node",
              args: ["/app/mcp-swarm.mjs"],
              env: { NATS_URL, AGENT_ID, SWARM_CHAT_TOPIC },
            },
          } : {}),
          ...extensionMcpServers,
        },
        permissionMode: "bypassPermissions",
        allowDangerouslySkipPermissions: true,
        stderr: (stderrData: string) => {
          console.error(`[claude-stderr] ${stderrData.trimEnd()}`);
        },
      },
    });

    // Process streaming result
    let fullResponse = "";
    const iter = result[Symbol.asyncIterator]();
    currentQueryIters.set(chatID, iter);
    try {
      for await (const event of { [Symbol.asyncIterator]: () => iter }) {
        console.log(`[agent] [${chatID}] event: type=${event.type}${"subtype" in event ? ` subtype=${event.subtype}` : ""}`);
        if (event.type === "result" && event.subtype === "success") {
          fullResponse = event.result;
          sessionsByKey.set(sessKey, event.session_id);
          saveSessionMap();
        } else if (event.type === "assistant") {
          for (const block of event.message.content) {
            if (block.type === "text") {
              await bridge.publishOutput(block.text, "text", msgId);
            } else if (block.type === "tool_use" || block.type === "server_tool_use") {
              console.log(`[agent] [${chatID}] tool: ${block.name}`);
              await bridge.publishStatus(`🔧 ${block.name}`);
            }
          }
        }
      }
    } catch (streamErr) {
      if (fullResponse) {
        console.warn(`[agent] claude process exited with error after successful result, ignoring:`, streamErr);
      } else {
        throw streamErr;
      }
    }

    // Send final result (skip if aborted — orchestrator already notified the user)
    if (fullResponse && !abortedChats.has(chatID)) {
      await bridge.publishResult(fullResponse, msgId);
    }

    console.log(`[agent] completed processing for agent ${agentName || AGENT_ID} chat ${chatID} (session=${sessionsByKey.get(sessKey)})`);
  } catch (err) {
    if (abortedChats.has(chatID)) {
      console.log(`[agent] query aborted for chat ${chatID}`);
      return;
    }
    const errorMsg = err instanceof Error ? err.message : String(err);
    console.error(`[agent] error processing message:`, err);
    await bridge.publishResult(`Error: ${errorMsg}`, msgId);
  } finally {
    currentQueryIters.delete(chatID);
    processingChats.delete(chatID);

    // Process next queued message for this chat if any
    const chatQueue = pendingByChat.get(chatID);
    if (chatQueue && chatQueue.length > 0) {
      const next = chatQueue.shift()!;
      if (chatQueue.length === 0) pendingByChat.delete(chatID);
      console.log(`[agent] dequeuing next message for chat ${chatID} (${chatQueue?.length || 0} remaining)`);
      handleMessage(next);
    }
  }
}

async function handleRoute(
  data: Record<string, unknown>,
  msg: import("nats").Msg
): Promise<void> {
  const text = data.text as string;
  if (!text) {
    msg.respond(new TextEncoder().encode(JSON.stringify({ agent: AGENT_ID })));
    return;
  }

  console.log("[agent] routing query");

  try {
    const systemPrompt = loadSystemPrompt(false);
    const cwd = "/workspace/agent";

    // Build agent descriptions from environment if available
    const agentDescsEnv = process.env.AGENT_DESCRIPTIONS || "";
    let routingPrompt = `You are a message router. Given the user message below, respond with ONLY the name of the most appropriate agent to handle it. Do not include any other text.\n\n`;
    if (agentDescsEnv) {
      routingPrompt += `Available agents:\n${agentDescsEnv}\n\n`;
    }
    routingPrompt += `User message: ${text}`;

    const result = query({
      prompt: routingPrompt,
      options: {
        model: CLAUDE_MODEL,
        cwd,
        pathToClaudeCodeExecutable: "/usr/local/bin/claude",
        systemPrompt: systemPrompt || undefined,
        allowedTools: [],
        permissionMode: "bypassPermissions",
        allowDangerouslySkipPermissions: true,
      },
    });

    let agentName = "";
    for await (const event of result) {
      if (event.type === "result" && event.subtype === "success") {
        agentName = event.result.trim();
      }
    }

    msg.respond(new TextEncoder().encode(JSON.stringify({ agent: agentName })));
  } catch (err) {
    console.error(`[agent] routing error:`, err);
    msg.respond(new TextEncoder().encode(JSON.stringify({ agent: AGENT_ID })));
  }
}

async function handleControl(
  data: Record<string, unknown>,
  msg: import("nats").Msg
): Promise<void> {
  const command = data.command as string;

  switch (command) {
    case "shutdown":
      console.log("[agent] shutting down...");
      await bridge.close();
      process.exit(0);
      break;
    case "ping":
      msg.respond(new TextEncoder().encode(JSON.stringify({ status: "ok" })));
      break;
    case "set_active_project": {
      const chatId = data.chat_id as string;
      const projectName = data.project as string;
      if (chatId && projectName) {
        activeProjects.set(chatId, projectName);
        console.log(`[agent] active project set to ${projectName} for chat ${chatId}`);
      } else if (chatId && !projectName) {
        activeProjects.delete(chatId);
        console.log(`[agent] active project cleared for chat ${chatId}`);
      }
      msg.respond(new TextEncoder().encode(JSON.stringify({ status: "ok" })));
      break;
    }
    case "abort": {
      const abortChatID = data.chat_id as string;
      if (abortChatID) {
        // Per-chat abort
        console.log(`[agent] aborting run for chat ${abortChatID}...`);
        abortedChats.add(abortChatID);
        const iter = currentQueryIters.get(abortChatID);
        if (iter) {
          iter.return?.(undefined);
          currentQueryIters.delete(abortChatID);
        }
        const q = pendingByChat.get(abortChatID);
        if (q && q.length > 0) {
          console.log(`[agent] discarding ${q.length} queued message(s) for chat ${abortChatID}`);
          pendingByChat.delete(abortChatID);
        }
        processingChats.delete(abortChatID);
        console.log(`[agent] run aborted for chat ${abortChatID}`);
      } else {
        // Global abort — all chats + parallel tasks
        console.log("[agent] aborting all runs...");
        for (const cid of processingChats) abortedChats.add(cid);
        for (const [, iter] of currentQueryIters) iter.return?.(undefined);
        currentQueryIters.clear();
        // Abort all parallel task queries
        for (const [mid, iter] of activeQueries) {
          abortedTasks.add(mid);
          iter.return?.(undefined);
        }
        activeQueries.clear();
        activeTaskCount = 0;
        pendingByChat.clear();
        processingChats.clear();
        if (pendingTasks.length > 0) {
          console.log(`[agent] discarding ${pendingTasks.length} queued task(s)`);
          pendingTasks.length = 0;
        }
        try { execSync("pkill -f /usr/local/bin/claude", { timeout: 3000 }); } catch { /* ignore */ }
        console.log("[agent] all runs aborted");
      }
      msg.respond(new TextEncoder().encode(JSON.stringify({ status: "ok" })));
      break;
    }
    case "clear_session": {
      const clearChatID = data.chat_id as string;
      if (clearChatID) {
        // Per-chat session clear — clear all agent sessions for this chat
        for (const key of sessionsByKey.keys()) {
          if (key.startsWith(`${clearChatID}:`)) {
            sessionsByKey.delete(key);
          }
        }
        saveSessionMap();
        console.log(`[agent] session cleared for chat ${clearChatID}`);
      } else {
        // Global session clear
        sessionsByKey.clear();
        for (const dir of [
          "/home/praktor/.claude/projects",
          "/home/praktor/.claude/sessions",
          "/home/praktor/.claude/todos",
        ]) {
          try { rmSync(dir, { recursive: true, force: true }); } catch { /* ignore */ }
        }
        saveSessionMap();
        console.log("[agent] all sessions cleared");
      }
      msg.respond(new TextEncoder().encode(JSON.stringify({ status: "ok" })));
      break;
    }
    default:
      console.warn(`[agent] unknown control command: ${command}`);
      msg.respond(new TextEncoder().encode(JSON.stringify({ error: `unknown command: ${command}` })));
      break;
  }
}

async function main(): Promise<void> {
  console.log(`[agent] starting for agent ${AGENT_ID}${USER_ID ? ` (user ${USER_ID})` : ""}`);
  console.log(`[agent] NATS URL: ${NATS_URL}`);

  installGlobalInstructions();
  ensureAgentMd();
  ensureWorkspace();
  setupAgentBrowser();
  setupAgentMail();

  // Apply agent extensions (MCP servers, plugins, skills, settings)
  const extResult = await applyExtensions();
  extensionMcpServers = extResult.mcpServers;

  // Clean up Claude Code internal files that accumulate over time
  for (const dir of ["/home/praktor/.claude/todos"]) {
    try { rmSync(dir, { recursive: true, force: true }); } catch { /* ignore */ }
  }

  bridge = new NatsBridge(NATS_URL, AGENT_ID);
  await bridge.connect();

  // Report any extension errors via NATS
  if (extResult.errors.length > 0) {
    const errMsg = `Extension errors:\n${extResult.errors.map((e) => `- ${e}`).join("\n")}`;
    console.error(`[extensions] ${errMsg}`);
    await bridge.publishOutput(errMsg, "text");
  }

  bridge.subscribeInput(handleMessage);
  bridge.subscribeControl(handleControl);
  bridge.subscribeRoute(handleRoute);

  // Subscribe to swarm collaborative chat if in swarm mode
  if (SWARM_CHAT_TOPIC) {
    console.log(`[agent] swarm mode: subscribing to chat topic ${SWARM_CHAT_TOPIC}`);
    bridge.subscribeSwarmChat(SWARM_CHAT_TOPIC, (msg) => {
      // Don't echo own messages
      if (msg.from === AGENT_ID) return;
      chatHistory.push({
        from: msg.from,
        content: msg.content,
        timestamp: Date.now(),
      });
      console.log(`[agent] swarm chat from ${msg.from}: ${msg.content.substring(0, 80)}...`);
    });
  }

  // Flush to ensure subscriptions are registered with NATS server
  await bridge.flush();

  loadSessionMap();

  await bridge.publishReady();
  console.log(`[agent] ready and listening for messages`);

  // Keep process alive
  process.on("SIGTERM", async () => {
    console.log("[agent] SIGTERM received, shutting down...");
    await bridge.close();
    process.exit(0);
  });

  process.on("SIGINT", async () => {
    console.log("[agent] SIGINT received, shutting down...");
    await bridge.close();
    process.exit(0);
  });
}

main().catch((err) => {
  console.error("[agent] fatal error:", err);
  process.exit(1);
});
