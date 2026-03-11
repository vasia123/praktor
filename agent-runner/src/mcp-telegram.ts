import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import { readFileSync } from "fs";
import { basename } from "path";
import { sendIPC } from "./ipc.js";

const server = new McpServer({
  name: "praktor-telegram",
  version: "1.0.0",
});

// --- Messages ---

server.tool(
  "telegram_send_message",
  "Send a separate Telegram message. Use ONLY when you need to send an additional message or send to a different chat. Your normal text response is already sent automatically — do NOT use this for your main reply.",
  {
    text: z.string().describe("Message text"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ text, chat_id }) => {
    const resp = await sendIPC("tg_send_message", { text, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Message sent (id: ${resp.message_id})` }] };
  }
);

server.tool(
  "telegram_reply",
  "Reply to a specific message in Telegram (creates a reply thread).",
  {
    text: z.string().describe("Reply text"),
    message_id: z.number().describe("ID of the message to reply to"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ text, message_id, chat_id }) => {
    const resp = await sendIPC("tg_reply", { text, message_id, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Reply sent (id: ${resp.message_id})` }] };
  }
);

server.tool(
  "telegram_edit_message",
  "Edit a previously sent bot message.",
  {
    message_id: z.number().describe("ID of the message to edit"),
    text: z.string().describe("New message text"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ message_id, text, chat_id }) => {
    const resp = await sendIPC("tg_edit_message", { message_id, text, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Message ${message_id} edited` }] };
  }
);

server.tool(
  "telegram_delete_message",
  "Delete a message in the chat.",
  {
    message_id: z.number().describe("ID of the message to delete"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ message_id, chat_id }) => {
    const resp = await sendIPC("tg_delete_message", { message_id, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Message ${message_id} deleted` }] };
  }
);

server.tool(
  "telegram_forward_message",
  "Forward a message to another chat.",
  {
    message_id: z.number().describe("ID of the message to forward"),
    to_chat_id: z.string().describe("Destination chat ID"),
    from_chat_id: z.string().optional().describe("Source chat ID (defaults to current chat)"),
  },
  async ({ message_id, to_chat_id, from_chat_id }) => {
    const resp = await sendIPC("tg_forward_message", { message_id, to_chat_id, from_chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Message forwarded (id: ${resp.message_id})` }] };
  }
);

// --- Media ---

server.tool(
  "telegram_send_photo_url",
  "Send a photo by URL to the chat.",
  {
    url: z.string().describe("Photo URL"),
    caption: z.string().optional().describe("Photo caption"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ url, caption, chat_id }) => {
    const resp = await sendIPC("tg_send_photo_url", { url, caption, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Photo sent (id: ${resp.message_id})` }] };
  }
);

server.tool(
  "telegram_send_sticker",
  "Send a sticker to the chat.",
  {
    sticker: z.string().describe("Sticker file_id or URL"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ sticker, chat_id }) => {
    const resp = await sendIPC("tg_send_sticker", { sticker, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Sticker sent (id: ${resp.message_id})` }] };
  }
);

server.tool(
  "telegram_send_voice",
  "Send a voice message from a file in the container.",
  {
    path: z.string().describe("Absolute path to the audio file in the container"),
    caption: z.string().optional().describe("Voice message caption"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ path, caption, chat_id }) => {
    let data: string;
    try {
      data = readFileSync(path).toString("base64");
    } catch {
      return { content: [{ type: "text" as const, text: `Error: file not found: ${path}` }] };
    }
    const resp = await sendIPC("tg_send_voice", { data, caption, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Voice message sent (id: ${resp.message_id})` }] };
  }
);

server.tool(
  "telegram_send_video_note",
  "Send a video note (round video) from a file in the container.",
  {
    path: z.string().describe("Absolute path to the video file in the container"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ path, chat_id }) => {
    let data: string;
    try {
      data = readFileSync(path).toString("base64");
    } catch {
      return { content: [{ type: "text" as const, text: `Error: file not found: ${path}` }] };
    }
    const resp = await sendIPC("tg_send_video_note", { data, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Video note sent (id: ${resp.message_id})` }] };
  }
);

server.tool(
  "telegram_send_animation",
  "Send a GIF/animation by URL or from a file in the container.",
  {
    url: z.string().optional().describe("Animation URL"),
    path: z.string().optional().describe("Absolute path to the animation file in the container (used if url is not provided)"),
    caption: z.string().optional().describe("Animation caption"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ url, path: filePath, caption, chat_id }) => {
    let data: string | undefined;
    if (!url && filePath) {
      try {
        data = readFileSync(filePath).toString("base64");
      } catch {
        return { content: [{ type: "text" as const, text: `Error: file not found: ${filePath}` }] };
      }
    } else if (!url) {
      return { content: [{ type: "text" as const, text: "Error: url or path is required" }] };
    }
    const resp = await sendIPC("tg_send_animation", { url, data, caption, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Animation sent (id: ${resp.message_id})` }] };
  }
);

// --- Interactive ---

server.tool(
  "telegram_send_poll",
  "Create a poll in the chat.",
  {
    question: z.string().describe("Poll question"),
    options: z.array(z.string()).min(2).max(10).describe("Poll options (2-10)"),
    is_anonymous: z.boolean().optional().describe("Whether the poll is anonymous (default: true)"),
    type: z.enum(["regular", "quiz"]).optional().describe("Poll type (default: regular)"),
    allows_multiple_answers: z.boolean().optional().describe("Allow multiple answers"),
    correct_option_id: z.number().optional().describe("0-based index of correct answer (required for quiz type)"),
    explanation: z.string().optional().describe("Explanation shown after answering quiz"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ question, options, is_anonymous, type, allows_multiple_answers, correct_option_id, explanation, chat_id }) => {
    const resp = await sendIPC("tg_send_poll", {
      question, options, is_anonymous, type, allows_multiple_answers,
      correct_option_id, explanation, chat_id,
    });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Poll created (id: ${resp.message_id})` }] };
  }
);

server.tool(
  "telegram_set_reaction",
  "Set a reaction emoji on a message.",
  {
    message_id: z.number().describe("ID of the message to react to"),
    emoji: z.string().describe("Reaction emoji (e.g. \"\ud83d\udc4d\", \"\u2764\ufe0f\", \"\ud83d\udd25\", \"\ud83c\udf89\", \"\ud83d\ude22\", \"\ud83d\ude02\")"),
    is_big: z.boolean().optional().describe("Whether to show a big animation"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ message_id, emoji, is_big, chat_id }) => {
    const resp = await sendIPC("tg_set_reaction", { message_id, emoji, is_big, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Reaction ${emoji} set on message ${message_id}` }] };
  }
);

// --- Chat management ---

server.tool(
  "telegram_pin_message",
  "Pin a message in the chat.",
  {
    message_id: z.number().describe("ID of the message to pin"),
    disable_notification: z.boolean().optional().describe("Pin silently"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ message_id, disable_notification, chat_id }) => {
    const resp = await sendIPC("tg_pin_message", { message_id, disable_notification, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: `Message ${message_id} pinned` }] };
  }
);

server.tool(
  "telegram_unpin_message",
  "Unpin a message in the chat.",
  {
    message_id: z.number().optional().describe("ID of the message to unpin (omit to unpin most recent)"),
    chat_id: z.string().optional().describe("Target chat ID (defaults to current chat)"),
  },
  async ({ message_id, chat_id }) => {
    const resp = await sendIPC("tg_unpin_message", { message_id, chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    return { content: [{ type: "text" as const, text: "Message unpinned" }] };
  }
);

server.tool(
  "telegram_get_chat_info",
  "Get information about the current or specified chat.",
  {
    chat_id: z.string().optional().describe("Chat ID (defaults to current chat)"),
  },
  async ({ chat_id }) => {
    const resp = await sendIPC("tg_get_chat_info", { chat_id });
    if (resp.error) return { content: [{ type: "text" as const, text: `Error: ${resp.error}` }] };
    const info = resp.data ? JSON.stringify(resp.data, null, 2) : "{}";
    return { content: [{ type: "text" as const, text: info }] };
  }
);

async function main(): Promise<void> {
  const transport = new StdioServerTransport();
  await server.connect(transport);
}

main().catch((err) => {
  console.error("MCP telegram server error:", err);
  process.exit(1);
});
