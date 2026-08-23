# Naive-reader protocol

How to spawn and question a context-free reader. Two reader kinds; both are
`haiku` subagents with no exocomp context.

## What a reader may see

Only what an MCP client surfaces to a model:

- the tool `name`;
- its `description` string;
- its `inputSchema` (JSON Schema, verbatim);
- for a **catalog reader** only: the full list of `{name, description}` for the
  tools visible in that scope (so it can pick among them).

Nothing else. Do not mention exocomp, homelab, ansible, terraform, this repo,
or that an audit is happening. The reader believes it is an agent deciding
whether and how to call an unfamiliar tool.

## Reader kind A — per-tool comprehension (one reader per tool)

Prompt shape:

> You are an AI agent connected to a tool server. Below is one tool exactly as
> the server advertised it. You have no other documentation. Answer only from
> this text.
>
> ```json
> { "name": "...", "description": "...", "inputSchema": { ... } }
> ```
>
> Answer each, briefly:
> 1. In one sentence, what does calling this tool do?
> 2. When would you choose to call it — what user request or situation?
> 3. Does it change anything, or only read? If it changes something, does the
>    change happen immediately, or is it requested/queued/subject to approval?
> 4. List each argument you would supply and a concrete example value.
> 5. What do you expect back?
> 6. Anything you are unsure of, or that could go wrong if you guessed?

Question 3 is the danger probe; question 6 surfaces the ambiguity the author
cannot see because they know the answer.

## Reader kind B — catalog disambiguation (one reader per scope)

Give the reader the full scope-visible `{name, description}` list and a set of
plausible tasks (derive tasks from ground truth — e.g. "check whether the last
apply for workspace X succeeded", "roll out a config change to the web tier and
require a human to approve it before it runs"). For each task ask:

> Which single tool would you call first, and what arguments would you pass?
> If none fits, say so.

This measures whether descriptions are *distinct* enough to route correctly —
two tools that read alike will steal each other's calls even when each is clear
in isolation.

## Isolation rules

- One tool per reader for kind A; readers never see each other's answers.
- Fresh readers for the re-audit — a reader that saw the old text is no longer
  naive about the new text.
- Vary nothing but the tool text between a tool's before and after readers, so
  the score delta is attributable to the rewrite.
- Fan the readers out in parallel; they are independent.
