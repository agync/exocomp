# Scoring rubric

Score each per-tool reader against your private ground truth. Six axes; the
first five are comprehension, the sixth is the tie-break.

Grade each axis **pass / partial / fail** and keep the reader's own words as
evidence — a score without the quoted misread is not actionable.

| Axis | Pass | Fail |
|---|---|---|
| **Intent** (Q1) | names what the tool actually does | describes a different capability |
| **When-to-call** (Q2) | situation matches real use | would call it in the wrong situation, or can't say |
| **Side-effect / danger** (Q3) | read-vs-write correct, and enqueue/immediate/gated correct for writes | thinks a read mutates, a write is a read, or a queued/gated write runs immediately |
| **Argument construction** (Q4) | every required arg supplied with a plausibly-typed value; no invented args | omits a required arg, invents one, or mistypes (e.g. passes a value where an id is wanted) |
| **Return expectation** (Q5) | roughly right shape/meaning | expects something the tool never returns |
| **Brevity** (tie-break) | comprehension achieved on few description tokens | needs a long description to be understood |

## Severity

Rank findings for the report in this order:

1. **Dangerous misread** — any Q3 fail, or a Q4 answer that would pass a secret
   value as an argument. These can cause a wrong or unsafe call, not just a
   failed one. Highest priority regardless of the other axes.
2. **Would-misroute** — a catalog reader (kind B) picks the wrong tool for a
   task, or an intent/when-to-call fail on a tool that has a look-alike sibling.
3. **Would-fail-the-call** — an argument-construction fail: the agent tries the
   right tool but the call is malformed.
4. **Merely unclear** — partials and low-confidence Q6 answers with no wrong
   action implied. Worth a rewrite, lowest urgency.

## Tool-level verdict

- **Solid** — pass on intent, when-to-call, side-effect, and arguments.
- **Weak** — any comprehension fail that is not dangerous.
- **Dangerous** — any Q3 fail or secret-as-argument. Always rewrite, always
  re-audit.

## Scope and catalog verdict

- Per scope: fraction of tools Solid; count of Dangerous; disambiguation
  accuracy (kind-B correct picks / tasks).
- A scope is not "done" while it holds any Dangerous tool, however high its
  average.
