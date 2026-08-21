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
designs, and a 22-gap adversarial critique. The curated pages are summaries; the archive
documents themselves were imported unchanged into `history/`.

**Naming.** The archive predates the rename from the working name "control" to **exocomp**
([ADR 0002](adr/0002-project-name-exocomp.md)). Curated pages use the current name
throughout. Imported history pages keep the working name and were deliberately **not**
rewritten — the reasoning is stated in [history/index.md](history/index.md).

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
