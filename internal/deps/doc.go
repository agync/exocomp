// Package deps resolves a source's declared dependency manifests into
// hash-gated, offline-restorable caches (ADR 0011). It discovers the Python
// manifest a project ships the ordinary way — a repo-root pyproject.toml or
// requirements.txt, with .exocomp/ as the explicit override — computes a
// content hash over it, and ensures a uv-managed venv keyed to that hash so an
// unchanged manifest reuses its venv with no uv invocation and no network.
//
// The boundary this package holds (ADR 0006/0011): it MAY invoke uv for a
// project's declared dependencies, but never to switch the ansible-core
// toolchain (an operator-shell act) and never on a DR-path runbook. Callers
// keep DR runs on the image toolchain by not resolving a venv for them.
package deps
