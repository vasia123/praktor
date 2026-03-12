import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "fs";
import { join } from "path";

const PROJECTS_DIR = "/workspace/agent/projects";
const WORKSPACE_MD = "/workspace/agent/CLAUDE.md";

const server = new McpServer({
  name: "praktor-projects",
  version: "1.0.0",
});

server.tool(
  "project_list",
  "List all projects in the workspace. Returns project names and their descriptions from CLAUDE.md.",
  {},
  async () => {
    try {
      if (!existsSync(PROJECTS_DIR)) {
        return { content: [{ type: "text" as const, text: "No projects directory found. Create one with project_create." }] };
      }

      const entries = readdirSync(PROJECTS_DIR, { withFileTypes: true });
      const projects = entries.filter(e => e.isDirectory()).map(e => {
        const claudeMd = join(PROJECTS_DIR, e.name, "CLAUDE.md");
        let description = "";
        try {
          const content = readFileSync(claudeMd, "utf-8");
          // Extract first non-empty, non-heading line as description
          const lines = content.split("\n").filter(l => l.trim() && !l.startsWith("#"));
          description = lines[0] || "";
        } catch { /* no CLAUDE.md */ }
        return `- ${e.name}${description ? `: ${description}` : ""}`;
      });

      if (projects.length === 0) {
        return { content: [{ type: "text" as const, text: "No projects yet. Create one with project_create." }] };
      }

      return { content: [{ type: "text" as const, text: `Projects:\n${projects.join("\n")}` }] };
    } catch (err) {
      return { content: [{ type: "text" as const, text: `Error: ${err}` }] };
    }
  }
);

server.tool(
  "project_create",
  "Create a new project in the workspace. Creates directory with CLAUDE.md and updates the root workspace CLAUDE.md.",
  {
    name: z.string().describe("Project name (alphanumeric, hyphens, underscores)"),
    description: z.string().optional().describe("Project description for CLAUDE.md"),
  },
  async ({ name, description }) => {
    try {
      const projectDir = join(PROJECTS_DIR, name);
      if (existsSync(projectDir)) {
        return { content: [{ type: "text" as const, text: `Project "${name}" already exists.` }] };
      }

      mkdirSync(projectDir, { recursive: true });
      const claudeContent = `# ${name}\n\n${description || "Project description."}\n`;
      writeFileSync(join(projectDir, "CLAUDE.md"), claudeContent);

      // Update root CLAUDE.md with project list
      updateRootClaudeMd();

      return { content: [{ type: "text" as const, text: `Project "${name}" created at ${projectDir}` }] };
    } catch (err) {
      return { content: [{ type: "text" as const, text: `Error: ${err}` }] };
    }
  }
);

server.tool(
  "project_info",
  "Read the CLAUDE.md of a specific project to get its context and instructions.",
  {
    name: z.string().describe("Project name"),
  },
  async ({ name }) => {
    try {
      const claudeMd = join(PROJECTS_DIR, name, "CLAUDE.md");
      if (!existsSync(claudeMd)) {
        return { content: [{ type: "text" as const, text: `Project "${name}" not found or has no CLAUDE.md.` }] };
      }
      const content = readFileSync(claudeMd, "utf-8");
      return { content: [{ type: "text" as const, text: content }] };
    } catch (err) {
      return { content: [{ type: "text" as const, text: `Error: ${err}` }] };
    }
  }
);

server.tool(
  "project_switch",
  "Switch the active project for the current chat. This changes the working directory for subsequent queries. Pass empty name to switch to workspace root.",
  {
    name: z.string().describe("Project name to switch to (empty string for workspace root)"),
    chat_id: z.string().describe("Chat ID to switch project for"),
  },
  async ({ name, chat_id }) => {
    try {
      if (name && !existsSync(join(PROJECTS_DIR, name))) {
        return { content: [{ type: "text" as const, text: `Project "${name}" not found. Use project_create first.` }] };
      }

      // Send control command to set active project
      // The main index.ts handles the activeProjects map via control command
      const { sendIPC } = await import("./ipc.js");
      // We can't directly call the control handler from MCP, so we use IPC
      // The set_active_project is handled as a control command in index.ts
      // For now, we'll use a workaround: write a marker file
      const NATS_URL = process.env.NATS_URL || "nats://localhost:4222";
      const AGENT_ID = process.env.AGENT_ID || "default";

      // Use NATS to send control command
      const { connect, StringCodec } = await import("nats");
      const sc = StringCodec();
      const conn = await connect({ servers: NATS_URL });
      const topic = `agent.${AGENT_ID}.control`;
      const data = sc.encode(JSON.stringify({
        command: "set_active_project",
        chat_id,
        project: name,
      }));
      const resp = await conn.request(topic, data, { timeout: 5000 });
      await conn.drain();

      if (name) {
        return { content: [{ type: "text" as const, text: `Switched to project "${name}". Working directory: /workspace/agent/projects/${name}` }] };
      }
      return { content: [{ type: "text" as const, text: "Switched to workspace root." }] };
    } catch (err) {
      return { content: [{ type: "text" as const, text: `Error: ${err}` }] };
    }
  }
);

function updateRootClaudeMd(): void {
  try {
    const entries = readdirSync(PROJECTS_DIR, { withFileTypes: true });
    const projects = entries.filter(e => e.isDirectory()).map(e => e.name);

    let content = "# Workspace\n\n## Projects\n\n";
    if (projects.length === 0) {
      content += "No projects yet.\n";
    } else {
      content += projects.map(p => `- ${p}`).join("\n") + "\n";
    }

    writeFileSync(WORKSPACE_MD, content);
  } catch {
    // ignore errors updating root CLAUDE.md
  }
}

async function main(): Promise<void> {
  const transport = new StdioServerTransport();
  await server.connect(transport);
}

main().catch((err) => {
  console.error("MCP projects server error:", err);
  process.exit(1);
});
