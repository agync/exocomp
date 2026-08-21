# Knowledge base evolution log

Structural history of this bundle: pages added, deprecated, moved, merged, or split, and
corrections of fact. **This is not a software changelog** — releases, features, and bug
fixes belong to the project's release notes, not here.

Newest first.

## 2026-08-21 — Bundle created

The `docs/` bundle was created as an OKF v0.2 knowledge base, curated from the
pre-repository design archive that produced the project. Nothing existed before this entry.

**Added — sections and pages**

| Path | Contents |
|---|---|
| [index.md](index.md) | Root index and section map |
| [log.md](log.md) | This log |
| [adr/](adr/index.md) | Index plus ADRs 0001–0006, all Accepted |
| [architecture/](architecture/index.md) | Index, `overview.md`, `platform-contract.md`, `execution.md` |
| [design/](design/index.md) | Index, `requirements.md`, `amendments.md`, `delivery.md` |
| [operations/](operations/index.md) | Index, `bootstrap.md`, `disaster-recovery.md` |
| [sbom/](sbom/index.md) | Index, `bill-of-materials.md` |
| [history/](history/index.md) | Index plus 13 provenance records imported byte-identical |

**Sources.** The authoritative source was the archive's accepted technical design document.
Supporting sources were eight research notes, a post-decision addendum, three candidate
designs, and a 30-gap adversarial critique. The curated pages are summaries; the archive
documents themselves were imported unchanged into `history/`.

**Naming.** Most of the archive predates the rename from the working name "control" to
**exocomp** ([ADR 0002](adr/0002-project-name-exocomp.md)); the post-decision addendum was
written after it and mixes both names. Curated pages use the current name throughout.
Imported history pages were deliberately **not** rewritten — the reasoning is stated in
[history/index.md](history/index.md).

**Import fidelity.** All 13 history files are byte-identical to the archive originals. They
were checked for absolute filesystem paths, home directories, and local file URLs before
import; none were present, so no substitutions were needed. The archive's `design/` and
`research/` split is preserved under `history/` so the one relative cross-reference in the
set still resolves.

**Not imported.** The archive's rendered HTML dossier, which is a presentation of the same
material rather than a source, and the accepted design document itself, whose content is
distributed across the curated pages rather than duplicated.

**Precedence established.** Where a curated page and a history page disagree, the curated
page wins — history documents were not edited to stay consistent with later decisions. The
twelve [amendments](design/amendments.md) override the accepted candidate design wherever
they differ.

## 2026-08-21 — M0 maintenance corrections

Adversarial review of the integrated bundle corrected these facts (code and the
critique source win over curated prose):

- The critique's gap count is **30 enumerated gaps across sections A–H**, not 22;
  the wrong count appeared on seven pages and traced to trusting a summary of the
  review rather than the review itself. All seven corrected.
- The naming claim over `history/` was over-broad: the post-decision addendum was
  written *after* the rename and mixes both names. `history/index.md`, `index.md`,
  and this log now say so.
- `platform-contract.md` now states its CI enforcement is designed, not yet built.
- `install.sh` gained the documented `--restore` / `--rollback --image-tag` forms,
  resolving the contradiction with `operations/disaster-recovery.md`,
  `operations/bootstrap.md`, and `design/amendments.md` in code's favor by making
  the docs true.
- Effort figures: ADR 0001's comparison row now carries its units caveat;
  `delivery.md` explains the ~22 vs ~23 FTE-week split.

## 2026-08-21 — M1a implementation landed

The secrets envelope and the store/queue core now exist in code; pages describing
them are design-accurate but the implementation is authoritative:

- The recovery card's derivation block is executable text enforced by test
  (`internal/secrets/kit_test.go` runs the card's age + openssl lines verbatim and
  the derived password opens a real restic repository). The card uses `od`/`tr`
  instead of `xxd` for bare-host portability.
- The HKDF domain-separation salt is live: `exocomp/v1`
  (see [ADR 0003](adr/0003-secrets-envelope.md); the archive's `control/v1` was
  never used by any artifact).
- The job state machine, append-only `job_event` (trigger-enforced), per-key
  serialization, idempotency dedup, and the interrupted-job boot sweep are
  implemented and race-tested as designed in
  [architecture/execution.md](architecture/execution.md).
