# Agent: a Sol-native, general-purpose assistant

Status: design draft for APV to review (2026-11-26). Not built. Answers so far: pluggable model;
a shell plus Sol actions, where the shell runs is the person's choice; synced skill files; v1 does
chat everywhere, scheduled jobs, opt-in event triggers and talking to contacts' agents.

Sol's apps were built so an agent could use them: every operation is an action with a schema,
`--json` and `--dry-run`. This app is the agent that makes that pay off. It is an ordinary Sol
app (app API only, AGENTS.md rule 2) in its own repo, like Raypass.

## Shape

- **One brain, many screens.** The agent runs on one device of mine, the *host*: the one that
  holds the model settings and API key, usually an always-on desktop or anchor. Every other
  device (the phone, a laptop) shows the same chats and talks to the host over Sol. If the host
  is offline, the other devices say so and queue the message. A second host can be set as a
  fallback.
- **Pluggable model.** A small provider interface: Anthropic's Messages API by default, and any
  OpenAI-compatible endpoint (llama.cpp, Ollama, vLLM on my own machines, reached over Sol port
  sharing if it lives on another device). Per chat or per job the person can pick the model. The
  API key is stored by the agent on the host only (0600) or, if Raypass is installed, in Raypass
  through its actions.
- **Tools.**
  1. **Sol actions.** The agent has its own agent key (scoped per the person's choice, default
     `*` on their own person) and calls actions like `sol do` does. It reads the action catalog
     (names, descriptions, input schemas) from the app API and offers it to the model as tools,
     with a search tool when the catalog is large. Approvals work as for any agent: actions that
     declare approval land in my Inbox, and secret arguments stay out of disk (API.md §8).
  2. **A shell.** Commands run through a **runner** that the person picks. Sol builds no sandbox:
     - `host`: directly on the host, as the Sol user (the default; like a terminal).
     - `container`: `podman exec <name> sh -c …` (or docker), a rootless container with a
       mounted folder.
     - `vm` / `ssh`: `ssh <target> …` into a VM or another machine (a Proxmox VM, a Lima or
       microVM guest), or any command template the person writes (`limactl shell agent …`).
     - Several runners can be defined; a skill or job names the one it uses.
     Shell policy per runner: `ask` (each command is an Inbox approval showing the exact
     command), `allow` (listed command prefixes run, others ask), or `auto`. The default for
     `host` is `ask`.
  3. **Files.** Reading and writing in the synced folder goes through the Files and Notes
     actions, not the shell, so it works from any device and syncs.
- **Skills (Hermes-like).** A skill is a Markdown file: when to use it, the steps, which actions
  to call, optionally a script and the runner it needs. Skills live in a synced feed, so every
  device has them and the person can read, edit or delete them like notes (and export them as
  `.md`). When the agent works out how to do something new, it proposes a skill; the person
  approves it (Inbox) before it is saved. Skills can be shared with contacts like any feed.
- **Memory.** Facts and preferences the agent should keep, also in a synced feed, each visible
  and deletable. Nothing is remembered silently.
- **Chats.** Each chat is a feed (messages, tool calls and results, with secrets redacted), so
  history is on every device and survives the host changing.
- **Jobs.** Scheduled runs on the host ("every morning at 8, summarise my Inbox and today's
  calendar into a note"): a prompt, a schedule, a model, allowed tools and runner. One timer to
  the next run; results go to a chat or a note, and failures to the Inbox.
- **Event triggers (opt-in).** A trigger is a Sol event filter plus a prompt or skill ("when a
  file lands in Inbox/receipts, file it and add it to the expenses note"). Each trigger is off
  until the person switches it on, and each has its own tool limits.
- **Contacts' agents.** My agent can ask a contact's agent a question or a favour ("find an hour
  we're both free next week"). It goes through the contact's `agent.ask` action over Sol: their
  agent answers only within what they allowed for me, and by default each request is an Inbox
  approval on their side. Agents never get each other's keys or raw data, only answers.

## Agent-first, as every app

Every operation is an action: `agent.chat`, `agent.send`, `agent.skills`, `agent.skill_save`,
`agent.jobs`, `agent.job_add`, `agent.triggers`, `agent.trigger_on`, `agent.memory`,
`agent.runners`, `agent.ask` (for contacts), and so on. Another agent (Claude Code, OpenCode)
can drive this one through `sol do`.

## Safety, in one place

- Everything the agent does is in Sol's activity log under its agent key; revoking the key stops
  it at once.
- Approvals are Sol's own (Inbox); shell commands show exactly what will run.
- A "stop" button on every screen cancels the running turn and all jobs.
- Prompt injection: content from files, web pages, contacts' agents or tool results is marked as
  data, never as instructions. A contact's agent can't make mine call tools it wasn't allowed.

## Open questions for APV

1. Name and repo (e.g. `apv022/<name>`), and whether it ships with Sol or is installed from its
   own channel (design/CHANNELS.md).
2. Web access in v1 (fetch and search tools), or only through the shell?
3. Default agent-key scope: everything of mine, or a chosen set of apps?
4. Voice (speech in and out) on the phone: later, or part of v1?
