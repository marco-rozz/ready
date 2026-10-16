---
name: telegram-formatter
description: "Formatting rules for Telegram output (HTML mode, emoji, structured responses)."
version: "1.2"
---

# Telegram Formatter Rules

These rules apply to all responses intended for the Telegram channel. You MUST follow the rules below for your entire response. All other channel rules (PETSCII, CLI, etc.) are irrelevant and must be ignored.

Telegram messages are rendered in **HTML parse mode** and are limited to 4096
characters per message.

## HTML Formatting
- Use `<b>bold</b>` for emphasis and device names (e.g. <b>Luce cucina</b>).
- Use `<i>italic</i>` for status descriptions ("acceso", "spento", "in pausa").
- Use `<code>inline code</code>` for entity IDs, numeric values, units.
- Use `<pre>…</pre>` blocks for tabular data or lists of raw values.
- **NEVER use Markdown syntax**: no `**bold**`, no `*italic*`, no `_italic_`,
  no backticks, no `#` headers. Telegram renders HTML mode only — raw
  Markdown asterisks will appear as literal characters in the chat.
- Escape literal `<`, `>`, and `&` only when they appear in plain text (not as
  tag delimiters). Prefer rephrasing over manual escaping.

## Emoji Conventions
Use a single status emoji at the start of the response to signal outcome:

- ✅ Successful action / state confirmed
- ❌ Failed action / error
- ⚠️ Warning or partial success
- 💡 Light-related actions
- 🌡️ Temperature / climate
- 🔌 Switches, plugs, generic power
- 🏠 General home information
- 🔒 Locks, security, alarms

Avoid stacking more than one emoji per line; use them as semantic markers, not decoration.

## Structure
- Start with `<emoji> <b>summary</b>` on the first line.
- Use blank lines between logical sections.
- Use `• ` (U+2022) for bullet lists; do not use `*` or `-`.
- For ordered sequences (multi-step actions) use `1.` / `2.` / `3.` prefixes.
- Keep the total response under 4096 characters; favour concise wording over
  truncation.

## Examples

✅ <b>Luce cucina</b> spenta

🌡️ Temperatura in <b>camera</b>: <code>21.5°C</code> (target <code>22°C</code>)

❌ Impossibile raggiungere <b>termostato soggiorno</b>
<i>Riprova tra qualche secondo o verifica la connessione del dispositivo.</i>

✅ Sequenza serale completata
• <b>Luci salotto</b> spente
• <b>Tapparelle</b> chiuse
• <b>Termostato</b> impostato a <code>19°C</code>
