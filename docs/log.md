# Knowledge base evolution log

Structural history of this bundle: pages added, deprecated, moved, merged, or split, and
corrections of fact. **This is not a software changelog** — releases, features, and bug
fixes belong to the project's release notes, not here.

Newest first.

## 2026-10-22

- **Litestream in the image.** [Decision 0012](decisions/0012-litestream.md) gains an
  addendum for shipping the binary while the sidecar stays the operator's to run.
  [Disaster recovery](operations/disaster-recovery.md#fast-rpo-restore-litestream) says where
  `restore --from-litestream` finds it, and [containers](operations/containers.md), [the bill
  of materials](sbom/bill-of-materials.md) and [the decision index](decisions/index.md) follow.

- **zstd in the image documented.** [Disaster recovery](operations/disaster-recovery.md)
  says the git leg compresses with zstd when it is on PATH and with gzip otherwise, and that
  restoring a `.tar.zst` snapshot needs `zstd`, by hand or through `exocomp restore`.
  [Containers](operations/containers.md) and [the bill of materials](sbom/bill-of-materials.md)
  list it.

- **Ping checks on by default in the units.** [Containers](operations/containers.md#ping-checks)
  says the shipped Quadlet and compose units set `ping_group_range` to the image's gid, why
  that value holds under `UserNS=keep-id`, and that `Network=host` drops the line.
  [Check-triggered runs](design/check-triggered-runs.md) points there.

- **Ping in the image documented.** [Containers](operations/containers.md) lists `ping` among
  what the base image carries and says which network setups let the daemon send ICMP, and
  [the bill of materials](sbom/bill-of-materials.md) adds `iputils`. [Check-triggered
  runs](design/check-triggered-runs.md) no longer names `fping` or a doctor probe for ICMP,
  neither of which exists.

## 2026-10-21

- **Job HOME documented as scratch.** [Running a playbook](operations/playbook-run.md) says
  which controller-side constructs resolve `~` to a sandboxed job's private home, which
  target-side module arguments do not, and to build target paths from a variable naming the
  target's home.

- **Container host management documented.** [Containers](operations/containers.md) says how
  a run started from the host differs from one started in the container, how the daemon
  manages the host it runs on through a loopback sshd and a key mounted read-only outside the
  data dir, and the order for moving the daemon to another host.

- **Remote identity grants documented.** [The serve page](operations/serve.md) documents
  `--allow-ssh-identity` and how a `--remote` run names a key on the daemon, [running a
  playbook](operations/playbook-run.md), [the API page](operations/api.md), [remote
  access](operations/remote-access.md) and [the sandbox guide](guide/sandbox.md) follow, and
  [the threat model](security/threat-model.md) records
  the reach a listed key gives operator-scope tokens.

- **Local-run notice documented.** [Remote access](operations/remote-access.md) shows the
  line a bare `playbook run` or `workspace plan`, `apply` or `destroy` prints when it runs
  locally while a default remote is stored.

- **Identity grant guard corrected.** [The serve page](operations/serve.md) and
  [sources](operations/sources.md) said a `--ssh-identity` path covering the data dir is
  refused. The guard refuses the data dir, its ancestors and its named state files, so a key
  elsewhere inside the data dir is granted. The serve page now says so and says where to
  keep a key instead.

- **Remote temp corrected.** [Running a playbook](operations/playbook-run.md) no longer
  says remote temp is redirected into the per-job tree. It says the value the sandbox sets,
  `$HOME/.ansible/tmp`, and where each connection type expands it, and
  [containers](operations/containers.md) says why the image's user needs it.

- **Container name resolution documented.** [Containers](operations/containers.md) says
  why sandboxed jobs cannot resolve host names in the image and how to give them the host
  resolver, [the threat model](security/threat-model.md) records the unix socket that
  exposes, and [Decision 0005](decisions/0005-job-sandboxing-nono.md) and [the execution
  page](architecture/execution.md) note the exception to socket placement.

- **Resolver egress documented.** [The sandbox guide](guide/sandbox.md) says the
  dependency resolvers reach only named index hosts through nono's proxy instead of a port
  grant, [the serve page](operations/serve.md) documents `--resolver-allow-domain` and
  corrects its claim that nono has no host filtering short of an HTTPS-terminating proxy,
  and [the threat model](security/threat-model.md) records the egress as an accepted
  residual. [Decision 0005](decisions/0005-job-sandboxing-nono.md) and [Decision
  0015](decisions/0015-per-source-collections.md) note the change.

- **Container and scrape-token docs corrected.** [Containers](operations/containers.md)
  now mints the first token with `--userns keep-id`, says that a published port does not
  reach the loopback-only state backend and where config writes have to run, and names the
  Quadlet's generated volume. [Notifications](operations/notifications.md) stops pointing
  at `doctor` for the scrape token and gives the hand derivation. [The sandbox
  guide](guide/sandbox.md) says the resolvers ignore `--privileged`, and [the threat
  model](security/threat-model.md) records the shared uv cache as an accepted residual.

## 2026-10-18

- **Effective-egress surfaces completed and the docs corrected.** The rules page now
  renders a playbook rule's effective egress — its declared ports unioned with the default
  set of the source it names, the same union the queue applies at admission — so a rule
  that declares nothing no longer reads as having no egress guard while its unattended runs
  open a port. [Running the daemon](operations/serve.md) documents the source's **Egress
  default** card and its `POST /sources/{name}/egress` form beside the per-run and per-rule
  grants. [Job sandboxing](guide/sandbox.md) states the real egress shape (the kind's
  built-in port ∪ an operator-set source default ∪ the run's declaration) instead of kind
  alone. [The design system](design/DESIGN.md) corrects "a run with no declaration carries
  neither grant tile" and records the approval page's egress block and the rules row's
  effective-egress guard. [The architecture map](contribution/architecture.md) corrects
  "network egress is blocked unless `--privileged`": egress is port-scoped, and
  `--privileged` opts out of the sandbox rather than granting egress. [The bundle
  index](index.md), [user guide index](guide/index.md), and [sources guide](guide/sources.md)
  name the operator-set source default beside `--egress`.

- **New page** [decisions/0023](decisions/0023-source-egress-defaults.md): a source-scoped
  default egress set for ansible runs, merged at queue admission. The
  [decision index](decisions/index.md) gains its row, and the threat model's port-only egress
  residual notes the extended reach.

- **Correction of fact**: [Ansible automation](guide/ansible.md) pointed a control-plane
  playbook at `--privileged` for API egress. The sandbox scopes egress by port, and a run
  declares a port with `--egress <port>` (on `playbook run`, `run`, and a playbook rule),
  so the page now leads with the scoped grant and reserves `--privileged` — a per-run,
  unsandboxed opt-out — for work that must run unsandboxed on the controller itself. The
  bundle [index](index.md) described the page the same stale way and carries the same fix.

- **Extended** [Ansible automation](guide/ansible.md) and [Sources, inventories, and the
  credential plane](operations/sources.md) with the source-scoped default egress set:
  `exocomp source egress <name> [port...]` records an operator-set default that every run
  naming the source inherits at queue admission, unioned with the run's `--egress` and the
  kind's SSH port 22. The source detail page reads it and, for an operator, sets it through
  the same verb (`POST /sources/{name}/egress`).

## 2026-10-17

- **Correction of fact**: [Landing site](operations/landing-site.md) named
  `npx wrangler versions upload` as the Worker's Preview command. The pull request builds
  run `npx wrangler preview`, which is Cloudflare's Previews mechanism: a branch build is
  served at a Preview URL, inherits none of the production settings, and never reaches
  `exocomp.dev`. It reads its configuration from a `previews` table and refuses to run
  without one, so `site/wrangler.toml` now declares an empty one, above `[assets]` so the
  keys cannot parse into that table. The page states what a Preview is and is not.
- **Correction of fact**: [Containers](operations/containers.md) listed the published
  manifest list as "decided, lands with the packaging work". It
  ships: `.github/workflows/publish.yml` builds each architecture on its own native
  runner and assembles the list, so the page records the workflow, the tags it pushes,
  and how the job logs in.
- **Correction of fact**: [Bill of materials](sbom/bill-of-materials.md) quoted 592 MB for
  the image and 17 MB for the daemon's idle RSS. Re-measured with the same `make budgets`
  method, against the published image, they are 618 MB and 32 MB (CI's own run agrees: 618 MB and 32 MB); the binary row claimed a
  ~20 MB target the pinned toolchain does not meet and now states the measured 46 MB. The
  rows carry the date of measurement, and `make budgets` fails when a dated figure is older
  than 180 days, so a stale number is a failing check rather than a claim nobody re-read.
  published image ref is `ghcr.io/agync/exocomp`. Every exocomp reference follows it: the
  Quadlet unit, the compose file, the configuration reference, the platform contract, the
  SBOM, the published landing page, and the code that records and reports the ref. The
  package now sits in the repository owner's namespace, which is where a workflow's
  built-in `GITHUB_TOKEN` can push, so the publish job needs no secret.

## 2026-10-16

- **Correction of fact**: three `guide/` examples contradicted the running code.
  [Reacting to alerts](guide/reactive.md) posted events to `POST /api/v1/events` with a
  bearer token and a `{"source","kind","payload"}` body; the ingress is
  `POST /hook/<token>` — the path token is the credential, there is no bearer header —
  and it reads each sender's native body. The same page's CEL example read
  `event.payload.*`; a normalized event carries `event.name`, `event.severity`, and
  `event.labels`, never a payload. [Health checks](guide/checks.md) matched a check
  transition on `event.check` and `event.status == 'down'`; a check event carries
  `event.name` (the check) and `event.host` (the target), and `status` is `firing` on a
  target coming up and `resolved` on it going down, the polarity
  [Reacting to alerts](operations/reactive.md) and
  [check-triggered runs](design/check-triggered-runs.md) already state.
  [Quickstart](guide/quickstart.md) read as though `exocomp recovery-kit` reprints the
  age recovery key; the key is printed once at setup or rotation and never stored, so
  `recovery-kit` re-renders the recipe, recipients, and fingerprints but not the key.
- **Convention.** Bundle images are written as HTML `<img>` rather than Markdown
  `![alt](path)`. `okf validate` reads every Markdown link target as a concept id and
  cannot see a non-Markdown asset, so each image was reported as `okf/links/broken`
  (21 warnings); the renderer passes `<img>` through and copies the asset at the same
  path, so the page is unchanged. A standalone image keeps the `<p>` wrapper the
  Markdown form produced, so the paragraph margin is preserved; an in-table image is
  left bare, as it rendered before.

## 2026-10-15

- **Added**: [Landing site](operations/landing-site.md) — building `site/dist` with podman
  or with the toolchain `site/toolchain.sh` installs for a CI build image, and the Cloudflare
  build settings that publish it. The build script gains a standalone-renderer path so a host
  without a container runtime can still render this bundle.
- **Correction of fact**: the site publishes as a Worker with static assets, not a Pages
  project. `wrangler deploy` reads `site/wrangler.toml`, which now declares
  `[assets] directory = "./dist"` instead of `pages_build_output_dir`; a Pages config is
  rejected by the deploy command with no entry point and no assets directory.

## 2026-10-15

- **Added**: CI and release automation land with the org move. `.github/workflows/`
  gates every PR and push: the fast lane (`fmt`, `vet`, `tidy`, `lint`,
  `test -short`, `build`, pre-commit hygiene) runs in parallel on every change;
  the `-race` merge gate (invariant 12) is sharded across four runners and runs
  on Go changes plus every main push; image+budgets and `okf` bundle gates are
  path-filtered; and a separate `e2e` workflow runs the lima-backed acceptance
  gates on a schedule or manual dispatch, self-skipping when tools are absent.
  `release-please-config.json` + `.release-please-manifest.json`
  start the pre-1.0 release line at `v0.1.0` (Decision 0022) and release-please,
  authenticated as the org's **agync-bot** GitHub App, owns the changelog and tags.
  Per Decision 0022 the app mirrors `CHANGELOG.md` to `docs/operations/CHANGELOG.md`
  as part of the release setup, so the two stay byte-identical.

## 2026-10-15

- **Added**: [Decision 0022](decisions/0022-pre-1.0-release-line.md) withdraws the
  v1.0 release label. The release line is pre-1.0 and begins at `v0.1.0`;
  release-please owns later versions. The acceptance bar stays met and gate-tested —
  `test/e2e/v1-acceptance-gate.sh` is the evidence — so only the label moved. The
  table in [decisions/index.md](decisions/index.md) gains its row.
- **Removed**: the development-process pages, pruned as material that served the
  build rather than the operator. Gone from [design/](design/index.md):
  `delivery.md` (milestones with effort), `v1.1-roadmap.md` (the ranked backlog),
  `v1-acceptance.md` (the acceptance bar — its gate,
  `test/e2e/v1-acceptance-gate.sh`, stays as the evidence), `ui-brief.md` and
  `tui-brief.md` (superseded by `web-system.md` and `tui-system.md`),
  `web-ui-renders.md`, `first-run-walkthrough.html`, `personas.md`,
  `bootstrap-first.md` (the shipped half is [operations/ansible-first-bootstrap.md](operations/ansible-first-bootstrap.md)),
  `amendments.md` and `requirements.md` (their substance is the operations pages
  that implement it). The `design/reference/tui/` handoff inputs go with them;
  `tui-system.md` is the distilled page. Every reference in code, decisions, and
  operations was retargeted rather than left dangling.
- **Removed**: 30 screenshots under `design/assets/` (5.3 MB of the section's
  8.3 MB) that no page, README, or code referenced.
- **Correction of fact**: `okf validate` reports image links as `okf/links/broken`
  because it resolves every link target as a concept id, and an asset is not a
  concept. The 21 findings are [design/gallery.md](design/gallery.md) plus the
  guide pages' asset references; the renderer resolves both link forms to the same
  correct path, so no link is actually broken. The count fell from 29 to 21 only
  because `web-ui-renders.md` was pruned with the pages above.

## 2026-10-14

- **Added**: [Decision 0021](decisions/0021-host-request-polling.md) keeps the
  `host` scope write-only and makes the request response the polling surface.
  The table in [decisions/index.md](decisions/index.md) gains its row; the host
  request's parameters and response are now documented in
  [operations/api.md](operations/api.md).
- **Correction of fact**: the secret-management vocabulary now follows a secret's
  source. `exocomp secrets replace` is the canonical verb (`rotate` stays an
  alias), the guide's commands and list-example column use it (`CHANGED` for a
  mixed list), and the TUI header is `CHANGED`. exocomp rotates only a secret it
  minted; for an operator-supplied credential it replaces the sealed copy while
  the old value is revoked upstream.
- **Moved**: `docs/adr/` became [decisions/](decisions/index.md). The twenty records keep
  their numbers and meaning; the label `ADR NNNN` is now `Decision NNNN` and prose says
  "record" instead of "ADR". Every path, link, and pointer was retargeted; no decision's
  meaning changed — this renames the collection and its term only.
- **Correction**: The system-management design record's rule "server class
  (public / self-hosted) is a fact about the URL, not the URL" is superseded.
  The URL became an ordinary setting, not a sealed secret, in the modal round, so
  the ntfy card may name it (a `server` fact). The topic figure's
  `topic · {class} server` note still carries the private-vs-public fact.

## 2026-10-13

- **Reconciliation against code and runtime**:
  - Corrected CLI flags across `docs/guide/`: `source add` (`--url`/`--ref` and `--path`, not `--kind`/`--branch`), `schedule add` (play kind syntax `--kind play --target <inv> -- <playbook>`, 6-field cron), `check add` (`--kind`, target `name=spec`, dynamic netgroups), `token new` (`<name>` positional, scopes `operator`/`readonly`/`approve`), and `silence add` (`--for`, `--reason`, `--alert`/`--host`/`--global`).
  - Grounded OpenTofu HTTP state backend URL in reality (`/api/v1/tf/state/<workspace>` with `LOCK`/`UNLOCK` methods, not `/api/v1/tofu/state/` with subpaths).
  - Clarified MCP agent interface (`docs/guide/agent-mcp.md`): corrected token scopes and tool catalog matching `internal/mcp` and `docs/operations/mcp.md`.
  - Replaced non-existent `EXOCOMP_LISTEN` with real environment variables in `docs/reference/configuration.md`.
  - Reconciled `docs/operations/api.md` with maintenance restart routes (`GET/POST /api/v1/maintenance/restart`).
  - Documented Web UI capabilities in `docs/operations/serve.md` and `docs/operations/sources.md`: Git source sync ("Sync now"), schedule on-demand execution ("Run now"), contextual ntfy setup modal, and viewer-local timezone formatting.

## 2026-10-11

- **Restructure**: Reorganized `docs/` into a user-guide-led, design-rich, and contributor-forward OKF v0.2 bundle:
  - Created `docs/guide/`: [index.md](guide/index.md), [quickstart.md](guide/quickstart.md), [sources.md](guide/sources.md), [ansible.md](guide/ansible.md), [opentofu.md](guide/opentofu.md), [handoffs.md](guide/handoffs.md), [secrets.md](guide/secrets.md), [checks.md](guide/checks.md), [reactive.md](guide/reactive.md), [mobile.md](guide/mobile.md), [agent-mcp.md](guide/agent-mcp.md).
  - Promoted UI design & screenshots front and center in `docs/design/`: added [gallery.md](design/gallery.md) (full multi-theme and mobile showcase), [web-system.md](design/web-system.md), [tui-system.md](design/tui-system.md), and curated visual assets under `docs/design/assets/`.
  - Expanded contributor manual in `docs/contribution/`: added [dev-setup.md](contribution/dev-setup.md), [dev-workflow.md](contribution/dev-workflow.md), [testing.md](contribution/testing.md), [architecture.md](contribution/architecture.md), [worktrees.md](contribution/worktrees.md), and [migrations.md](contribution/migrations.md).
  - Established `docs/reference/`: [index.md](reference/index.md), [cli.md](reference/cli.md), [configuration.md](reference/configuration.md), and [exit-codes.md](reference/exit-codes.md).
  - Re-scoped `docs/operations/` to focus strictly on Day-2 cluster lifecycle, maintenance, disaster recovery, and failover runbooks.
  - Copied release changelog to repository root `CHANGELOG.md`.

## 2026-10-10

- **Moved**: `history/` left this bundle. The three candidate designs, the completeness
  critique, the accepted technical design for v1, and the nine research notes are now in
  the `exocomp` section of the lab wiki, which is where work kept for the record and not
  currently true belongs. This bundle now describes the shipped state only, with no
  rejected alternatives and no design-phase provenance.
- [index.md](index.md) loses its History section, and [design/index.md](design/index.md),
  [design/amendments.md](design/amendments.md),
  [decisions/0001-thin-core-architecture.md](decisions/0001-thin-core-architecture.md) and
  [decisions/0002-project-name-exocomp.md](decisions/0002-project-name-exocomp.md) state their
  conclusions inline instead of linking across to another wiki.
- [AGENTS.md](../AGENTS.md) and the docs-curator role card no longer list `history/` in the
  bundle layout.
- **Update**: Dropped the design handoff captures under
  [design/reference/](design/reference/): the two canvas PDFs, the `canvas/` and
  `realign/` artboard PNGs, and the standalone web and phone dashboard PNGs. The
  `realign/` captures were already stale against the shipped surface, so nothing
  the code or the web UI depends on is lost.
- **Update**: Dropped the seventeen `reference/tui/` frame files that were
  byte-identical to the golden fixtures under
  [../internal/tui/testdata/frames/](../internal/tui/testdata/frames/), which is
  the copy the tests read.
- Five design notes that are not fixtures stay: `keymap.txt`, `colors.txt`,
  `copy.txt`, `states.txt`, and `README.txt`. `internal/tui/render.go`,
  `internal/tui/empty.go` and `internal/tui/write.go` cite them as the source of
  the color rule, the empty-state copy, and the keymap contract.
- [design/index.md](design/index.md) and
  [design/ui-brief.md](design/ui-brief.md) no longer point at the dropped paths,
  and `.pre-commit-config.yaml` no longer excludes the emptied directory.

## 2026-10-06

- resolver reads local collection artifacts.

- [Decision 0015](decisions/0015-per-source-collections.md) gains an addendum: the
  collections resolver's sandbox grants read on the local `type: dir`/`type: file`
  artifact paths a requirements file names, so `ansible-galaxy` can read a
  sibling checkout's `galaxy.yml` under the enforcing tier. The
  [threat model](security/threat-model.md) records the read-only widening as an
  accepted residual.

## 2026-09-26

- aggregate check events removed.

- Removed the opt-in aggregate-check surfaces from
  [operations/reactive.md](operations/reactive.md), [operations/mcp.md](operations/mcp.md),
  and [operations/api.md](operations/api.md); the now-moot `--limit-hosts` residual is dropped
  from [security/threat-model.md](security/threat-model.md).
  [Decision 0018](decisions/0018-aggregate-check-events.md) is superseded by
  [Decision 0019](decisions/0019-aggregate-check-events-removed.md).
- `check_row.aggregate`/`min_up` and migration
  [0041](../internal/store/migrations/0041_check_aggregate.sql) remain as inert
  schema; no drop-column migration is added.

## 2026-09-26

- exocomp does not manage cert lifecycle.

- The v1.1 roadmap's R2 framed cert-expiry checks as including "exocomp's OWN web-UI
  cert". That premise was wrong: the LAN listener certificate is self-signed with a
  10-year validity and trust-on-first-use pinned by fingerprint
  (`internal/web/tls.go`), and a Tailscale tailnet cert or an ACME reverse proxy
  renews itself. R2 corrected in
  [design/v1.1-roadmap.md](design/v1.1-roadmap.md); the `tls` check kind stays as a
  general, unprioritized capability for certificates nothing else renews.

## 2026-09-17

- the deployment image ref in the recovery chain.

- **`RECOVERY.md` now carries `image-ref:`** at each target root: the ref the daemon read
  from the unit's `EXOCOMP_IMAGE` and stored as `deploy.image_ref`. An unset ref omits the
  line rather than printing an empty value. This is the gap
  [Decision 0016](decisions/0016-artifact-carrier-image.md) names — a local tag dies with the host, so
  the recovery chain must name the image to re-load.
- `install.sh` gains `--image-load FILE` (`podman load -i`, or `docker load -i` on the
  Docker tier, before install or rollback) and `--image-ref REF`. The Quadlet unit and the
  compose file set `EXOCOMP_IMAGE` to the image the unit runs, so the stored ref reflects
  reality. `exocomp doctor` prints the stored ref in the text report and folds it into the
  existing `optional_tools` JSON detail (the fixed subsystem list is unchanged).
- `operations/disaster-recovery.md` documents capture (`podman save`) and load;
  `operations/containers.md` and `operations/machine-output.md` state the ref.

## 2026-09-17

- the doctor toolchain-manifest diff.

- **`doctor` now diffs each resolved tool version against its `MANIFEST.json` pin.** The
  comparison folds into the existing `optional_tools` health entry, so the fixed subsystem
  list is unchanged. A version off its pin is `warn`, never `fail` — the Decision 0006 operator
  escape hatch is a supported state; a version the extraction rule cannot parse is named in
  the detail without moving the status; and no manifest env at all leaves the entry
  `unknown`. The text report marks each divergent line against its pin and prints the active
  manifest path once.
- `operations/machine-output.md` and `operations/containers.md` state the comparison.

## 2026-09-17

- Decision 0016 and the container page.

- **New [Decision 0016](decisions/0016-artifact-carrier-image.md):** the base image is a
  distributable artifact carrier; uv is bootstrapped when absent, the Python pin
  is resolved from ansible and frozen, and ansible-core is a PATH-delegated
  command rather than a forced per-venv dependency. Amends decision records 0006 and 0011.
- **New [operations/containers.md](operations/containers.md):** the base image and
  what it carries, running it as shipped, inheriting to add system packages or
  Python dependencies, offline and multi-arch builds, and capturing an operator's
  image for recovery. Copying artifacts into a foreign base is deferred.
- **Correction of fact:** `index.md` labelled its six-row record table "Six accepted
  decision records"; the bundle now carries sixteen, so the table is retitled a foundational
  subset and points at the full list at [decisions/index.md](decisions/index.md).
- `platform-contract.md` gains an operator-built-image tier row;
  `bill-of-materials.md` lists `uvx` and the base image as a digest-pinned
  distribution artifact; `design/byo-runner.md` cross-references Decision 0016 so the
  deferred in-daemon tier is not confused with the supported image story.

## 2026-09-15

- TUI copy deck reconciled to shipped output.

- **`design/reference/tui/copy.txt` recorded two strings the shipped TUI never
  renders.** It now matches the binary and its own frames (the frames are the
  implementation contract — "text IS the implementation"): the footer signal line
  is `dead-man {age} · notify {loud|muted} · {version} ({commit}) built {date}`
  (the deck's `ntfy {age} · disk {pct}% · {version}↗` predates the shipped
  signal), and the SILENCED banner variant (`banner.go`, present in
  `frames/banners-80.txt`) is declared beside paused/muted.

## 2026-09-12

- base-path round: correction of fact.

- **The plan-review's claim that `http.StripPrefix` leaves `RequestURI`
  unrewritten is wrong for this toolchain.** A probe against go1.26.8 shows
  StripPrefix rewrites both `URL.Path` and `URL.RawPath` (net/http/server.go
  `StripPrefix`), so the handler-visible `RequestURI()` is the STRIPPED path.
  The base-path spine therefore prepends the base path exactly once to
  `RequestURI`-derived values (`url()`/`urlWithBase`), and the "no double
  prefix" requirement is met by idempotence, not by assuming the prefix
  survives the strip. `docs/log.md` records the fact; code is the source of
  truth.

## 2026-09-07

- responsive round (v0.9).

- **The design pages codify the responsive principles.** The web surfaces
  gained a single 719px phone breakpoint and the rules landed as a new
  `## Responsive (v0.9)` section in `design/INVARIANTS.md` (one breakpoint,
  rows-not-tables on phone, pre logs, the 56px phone shell bar, size-agnostic
  markup, the 48px floor with its 36px compact-secondary exception, the app-bar
  meta staying visible on phone); the `## Layout` sections of the root
  `DESIGN.md` and `design/DESIGN.md` were rewritten to the same wording, and
  the breakpoint tokens collapsed to the one phone breakpoint. Structural: the
  invariants page is extended.

## 2026-09-06

- degraded job state + job detail v2.

- **Software change, no structural edit needed here**: the job state machine
  gained the terminal `degraded` state (tolerated unreachable hosts, migration
  0034), and the web jobs surface was rebuilt (run card, verdict band, host
  table, two-line list, rerun/drawer tolerate toggle). Recorded here for
  bundle-fact coherence: "job state" now includes `degraded`; the web and TUI
  render it in wait (amber) and it is not a failure to CLI callers.

## 2026-09-06

- Decision 0015 page added.

- **A new `decisions/0015-per-source-collections.md` page records the per-source
  ansible collections isolation decision**, superseding Decision 0011's collections
  decision; the venv, tofu-provider, tofu-modules, and system-package decisions
  stand. Structural: a page added.

## 2026-09-02

- agent brain made local-only.

- **Correction of earlier framing.** The 2026-08-31 entry "`brain/` lands in
  `.agents/`" claimed `.agents/brain/` is tracked like the rest of `.agents/`
  content. It is not: the directory is gitignored and holds local-only working
  notes (specs, the design handoff, review inbox/outbox), so a reader of this
  repo cannot open it. [contribution/index.md](contribution/index.md) and the
  CHANGELOG now say plainly the path is local-only; the spec store is gone from
  the repo.

## 2026-09-02

- design invariants page added.

- **A new `design/INVARIANTS.md` page codifies the design philosophy, hard
  rules, and component vocabulary** into the bundle (previously held only in
  the untracked design-handoff canon). The repo-root `DESIGN.md` and the wiki
  `design/DESIGN.md` cross-reference it; `AGENTS.md` and both design indexes
  list it. Structural: a page added.

## 2026-09-01

- Netgroups page merged into /checks as a tab.

- **The standalone Netgroups page was removed; netgroups are now a tab on
  /checks** (`?tab=netgroups`). The `/netgroups` route 302s to the tab; the
  write redirects follow. This is the one route change of the design-handoff
  realignment (F7).

## 2026-09-01

- software changelog split from the bundle log.

- **The release narrative moved out of this log.** This page now records only
  bundle-structural history and corrections of fact; software releases,
  features, and fixes live in
  [operations/CHANGELOG.md](operations/CHANGELOG.md), linked from the bundle
  index, and this page's header says so. The split itself is structural, so it
  is recorded here; the CHANGELOG is the newest-first release narrative.

## 2026-09-01

- memfd run-secret staging rejected, recorded.

- **A `memfd_create(2)` staging experiment was revisited and rejected.** A
  long-dormant branch staged each run-secret into an anonymous memfd (no path in
  any mount, `F_ADD_SEALS`, mlock) instead of the shipped `/dev/shm` file
  staging (`--secret-staging {auto,tmpfs,disk}`). The branch was 265 commits
  behind and never merged; the shipped design already absorbed its rationale and
  then superseded it.
- **Why rejected (code wins).** A memfd's fd does not survive a sandboxed run:
  real `nono` closes every inherited descriptor but stdio before exec, and a
  memfd has no path a `--read-file` grant can name. A sandboxed tofu child can
  read a staged file under `/dev/shm` through the granted path — the exact case
  the memfd design could not support (`internal/executor/sandbox_e2e_test.go`).
- The branch is deleted; the `/dev/shm` staging with its loud disk fallback
  stands.


## 2026-09-01

- list-surface refinements.

- **Docs' UI-stack references corrected.** The vendored htmx include is gone; the docs now name the vanilla JS the app actually uses.

## 2026-09-01

- approval surface refinements.

- **Log self-correction.** Three back-dated entries (list-surface refinements, write-form errors, check-event traceability) now carry their true 09-01 dates; newest-first order restored.

## 2026-08-31

- sweep error policy recorded.

- **[architecture/reactive.md](architecture/reactive.md) states the shared
  sweep error policy** — the checks, drift, and job-failures sweeps now say
  in-place that a per-item failure is logged and the sweep continues (a failed
  check is still marked due and keeps its interval cadence; a broken check
  target is skipped so its siblings still probe), and only a structural failure
  (the sweep's list query) or shutdown stops it.


## 2026-08-31

- docs-review pass on checks, rules, and the web.

- **Corrected a false claim about `doctor`.** [security/threat-model.md](security/threat-model.md)
  T33 and [design/check-triggered-runs.md](design/check-triggered-runs.md)
  §3.2.1 said `doctor` asserts the LAN-only-routing precondition; it has no
  such probe. Both now record the precondition as documentary, not enforced.
- **[operations/reactive.md](operations/reactive.md) "Checks as an event"
  states the consequences the feature ships with:** a check's interval is
  floored at the `--sweep-interval` tick (30s default); a host home at daemon
  start is missed once, fail-safe; hysteresis is fixed at N=3 and a departure
  withdraws a pending approval only after 3 consecutive down observations
  (~N·interval lag on the resolve path); the persisted `check_state` row
  (status/consecutive/emitted/last_change) survives a restart and backs the
  /checks "changed N ago" line; the two §3.2.1 preconditions (static lease,
  no overlay route) are stated where an operator sets up; and the http
  dial-time guard plus the no-netgroup-on-url caveat are on the page.
- **The web paragraph and [operations/serve.md](operations/serve.md) browser
  list gain the shipped surfaces** — /checks netgroup chip + recency, the
  dashboard Checks card, job detail "fired by rule" → /rules + the triggering
  event, the events `?source=` filter, and the read-only /rules page.
  [operations/api.md](operations/api.md) adds `GET /api/v1/netgroups` and
  `GET /api/v1/checks` to the read endpoints.
- **[design/check-triggered-runs.md](design/check-triggered-runs.md) §4.2
  check_state DDL reconciled to migration 0030** (up/down, consecutive,
  emitted; no `unknown` — a missing row is the unobserved state), §5.1 http
  is 2xx/3xx, §9/Status note rules read-only on the web, and the Open
  questions close: hysteresis N fixed at 3, retention shipped last-status-only,
  the http-tailnet caveat, and cooldown keying via `0031_rule_fire_dedup.sql`;
  set-event / `--limit-hosts` stays open.


## 2026-08-31

- CLI/web check, rule, netgroup surfaces consolidated.

- **Correction of earlier framing.** The "store remains the shared validator" claim is corrected: the CLI and web parsers now unify in store.Parse* / internal/reactive.

## 2026-08-31

- threat model: checks and peer stamping rows.

- [security/threat-model.md](security/threat-model.md) gains a **Checks, netgroups, and peer
  stamping** subsection under the reactive section — T30–T35 record the shipped mitigations
  (peer-stamped presence, transition-only + hysteresis + per-target cooldown, the static-lease
  and LAN-only-routing preconditions, cold-start seeding, http status-only) that the code
  already carried, closing the invariant-8 gap the feature's design §10 owed. New rows are
  numbered T30+ to avoid renumbering the existing T1–T29 catalog.
- [index.md](index.md) summary rows for the Reactive layer and Reacting to alerts now mention
  check probes, netgroups, and peer stamping.


## 2026-08-31

- contribution guide and specs store.

- **New page** [contribution/index.md](contribution/index.md): the working
  procedure behind [AGENTS.md](../AGENTS.md) — the DCO, commit conventions,
  worktree workflow, verification, and design/review. Linked from the bundle
  index and from AGENTS.md's "Working here".
- **`brain/` lands in `.agents/`** — `.agents/brain/specs/` holds specs for
  in-flight work (the UI spec for check-triggered runs is the first). It is
  tracked, like the rest of `.agents/` content (skills, role cards,
  workflows); the git-excluded half of `.agents/` stays the generated workflow
  scripts. AGENTS.md's layout block names the new directory.


## 2026-08-31

- web surface for netgroups and checks.

- **Design reconciled.** check-triggered-runs.md §9 drops the in-browser rule-builder promise and records the shipped-subset Status; architecture/reactive.md and operations/reactive.md gained checks/netgroup sections.

## 2026-08-30

- check-triggered runs design.

- **New page** [design/check-triggered-runs.md](design/check-triggered-runs.md):
  proposal (no code) for running governed jobs on a reachability or health
  transition instead of only a wall clock. Three primitives: **checks** (named
  probe-sets — `tcp`/`ping`/`http`, transition-only events, per-target dedup),
  **network groups** (named CIDR sets giving the operator's own vocabulary and
  classifying a probe-target or webhook-peer address), and a governed
  **`playbook` rule action** (source checkout, inventory, run-secrets, sandbox)
  replacing the shell-argv escape hatch. Checks enter the existing reactive
  pipeline the way drift does (`SweepDrift` precedent), so cron stays on the
  schedule row and a check is a rule listening on `--source check`; both
  converge on the one queue.
- **Revised after review.** Address = location now states its two preconditions
  (static lease per target; LAN-only routing, never an overlay subnet route) and
  both get a threat-model row. Transition state lives in a `check_state` table
  with cold-start seeding that never emits on first observation, so a restart
  does not fire every presence rule. Phase 2 ships per-target events only; the
  set-event / `--limit-hosts` expansion is deferred. Peer stamping is framed as
  a network-path signal (LAN vs overlay), not a location verdict. Check
  transitions require N-consecutive-same-result hysteresis. Cooldown stays
  per-rule for webhooks (uniform per-dedup-key would let a body-varying webhook
  bypass the throttle) and is per-target for sweep sources.


## 2026-08-25

- QR pairing, schedule detail, sidebar logo.

- **New page** [operations/pairing.md](operations/pairing.md) documents the QR device-pairing payload contract.

## 2026-08-25

[security/threat-model.md](security/threat-model.md) corrects the token-mint boundary: a
job can reach the `exocomp` CLI via an ansible `command`/`local_action` or a tofu
`local-exec`; under the sandboxed tier the data-dir/keys denial (T6) fails it closed, but
under `TierLocal` (explicit or auto-degraded) it can mint an operator token or read
`host.key` — documented as a trusted-playbook tier. Also notes the web `POST /tokens/new`
mint route exists, gated by cookie-session + same-origin + reauth; `/api/v1` and `/mcp`
expose no mint.


## 2026-08-25

- sandbox tiers renamed, secure-by-default.

- **Tiers renamed and the default flipped.** The job-sandbox tiers are now `opportunistic` /
  `enforcing` / `disabled` (was `auto` / `local-sandboxed` / `local`), and the default — an
  empty `--sandbox`/`$EXOCOMP_SANDBOX` — is now `enforcing`: a host that cannot enforce the
  sandbox fails closed at `serve`/`doctor` startup rather than running jobs unsandboxed.
  `opportunistic` is the opt-in that degrades loudly to `disabled` instead, and `disabled` is
  the explicit unsandboxed tier. The three old names stay accepted as silent back-compat
  aliases. Terminology reconciled across [architecture/execution.md](architecture/execution.md),
  [operations/serve.md](operations/serve.md), [design/byo-runner.md](design/byo-runner.md),
  [security/threat-model.md](security/threat-model.md),
  [decisions/0005-job-sandboxing-nono.md](decisions/0005-job-sandboxing-nono.md), and the index, runners,
  cli-run, provision-converge, and SBOM pages.


## 2026-08-25

- access-token and inventory hardening.

- **Threat model renumbered.** The E/S/R/D/B/U/AI finding-id families were replaced by a sequential T1–T29 catalog in reading order.

## 2026-08-25

- source kinds collapsed; runs move to the source page.

- **Decision 0010 addendum** and operations/sources.md updated for the git/path (+writable) model; serve.md reflects the source-page launchers.

## 2026-08-25

[operations/serve.md](operations/serve.md) gains a "The browser surface" section: the web UI
is no longer view-only — an operator runs playbooks/commands, plans/applies workspaces, and
manages sources, secrets, tokens, schedules, policies, and runners from the browser. Sensitive
writes re-confirm with the operator's access token; these session form posts stay distinct
from the bearer-only `/api/v1`.


## 2026-08-24

- design system page and top-level README.

- **New page** [design/DESIGN.md](design/DESIGN.md): the web UI design system — color,
  typography, spacing, and component tokens — sourced from `internal/web`. The architecture
  technical-design that previously held this name moves to
  [history/design/DESIGN.md](history/design/DESIGN.md) as provenance.
- **New** repo-root `README.md`, current to shipped reality (OpenTofu, the single
  MCP-over-HTTP surface, remote CLI, sandbox tiers).
- **Voice.** Internal build codenames — milestone tags, finding and review ids, process
  jargon — are removed across the docs and decision records; pages describe current behaviour and read
  harness-neutral.


## 2026-08-24

The four target-user personas now live as one crisp definitions page,
[design/personas.md](design/personas.md). The internal persona-review, persona-scenario,
and practitioner-gap-review planning pages are removed; their surviving conclusions live in
[design/v1.1-roadmap.md](design/v1.1-roadmap.md) and
[design/v1-acceptance.md](design/v1-acceptance.md).


## 2026-08-24

- failover page and cross-host lease.

- **New page** [operations/failover.md](operations/failover.md): the cold-standby
  procedure (restic restore, optional Litestream fast-forward, deliberate `serve`, re-drive
  of interrupted work) and the cross-host active lease that fences it above the host-local
  `serve.lock`.
- **Correction.** The lease fences only two daemons opening the *same* `exocomp.db` (a
  shared volume or network filesystem); it does not fence the Litestream-replica or
  restic-restore copies, each of which is a separate file. The runbook's restore path relies
  on the operator confirming the primary is stopped.
- [operations/serve.md](operations/serve.md) gained the cross-host half of "one daemon per
  data dir".


## 2026-08-24

[design/byo-runner.md](design/byo-runner.md) and
[operations/runners.md](operations/runners.md): per-job memory/CPU caps are enforced by
cgroup v2 direct writes (the container image ships no systemd, so `systemd-run` is not the
path), opt-in and fail-closed — the default rootless deployment refuses a capped job rather
than running it uncapped. A job selects a runner's caps with `--runner`. The
container-image tier stays deferred.


## 2026-08-24

[design/bootstrap-first.md](design/bootstrap-first.md) and the operations pages describe
run-secret file staging on `/dev/shm`: per-instance session directories, a
`--secret-staging {auto,tmpfs,disk}` backend with a loud disk fallback, best-effort mlock,
and unlink-on-exit. `exocomp doctor` reports the resolved backend and residual posture.


## 2026-08-24

[security/threat-model.md](security/threat-model.md):

- DEK reseal (recipient revocation) is crash-safe — staged-then-promoted, with a leftover
  `keyring.age.next` reconciled on the next `Open`. The residual (a transit-sealed store
  cannot reconcile without the host key) is recorded open.
- The auth-timing decoy KDF is removed; the per-source/per-token fail counter remains the
  guard. The accepted residual timing gap is recorded, owned by `internal/web/auth.go`.


## 2026-08-24

- durable approval and scheduler corrections.

- [operations/ansible-first-bootstrap.md](operations/ansible-first-bootstrap.md) and the
  bootstrap/handoff/provision pages: a composed-chain `--require-approval` step is held
  mid-run as a durable per-step approval, not a CLI confirmation.
- [operations/serve.md](operations/serve.md): schedule catch-up now dedups every fire on
  the occurrence's scheduled instant, and `last_run` advances only once a fire is enqueued,
  so a crash no longer double-fires and a failed enqueue is not silently dropped.
- [operations/reactive.md](operations/reactive.md): `approval link` needs its own
  `--public-url`/`EXOCOMP_PUBLIC_URL`; it cannot read the running daemon's value back.


## 2026-08-23

- agent-facing surface pages.

- **New pages**: [operations/remote-access.md](operations/remote-access.md) (remote CLI,
  certificate pinning, `--remote`/`--token`), [operations/api.md](operations/api.md) (the
  `/api/v1` read and write surface, wire envelope, error-code table),
  [operations/machine-output.md](operations/machine-output.md) (`--output json`, the error
  taxonomy, and the honestly partial exit-code wiring), and
  [operations/runners.md](operations/runners.md) (the runner registry as definitions).
- **Corrections.** [architecture/overview.md](architecture/overview.md): `internal/service`
  owns the write verbs, not "all business logic" (reads stay store-direct); the loopback
  state backend and the operator `/api/v1` surface are separate rows.
- [operations/sources.md](operations/sources.md) documents `secrets rotate`/`rm`,
  `--generate`, `--expires`, and that expiry is a hygiene marker that never gates decryption
  or injection.


## 2026-08-23

The stdio MCP server documentation is removed. [operations/mcp.md](operations/mcp.md) now
describes one programmatic surface — MCP over HTTP on the `serve` listener, bearer-scoped
([Decision 0009](decisions/0009-lan-access-tokens.md)) — with an off-the-shelf http-to-stdio bridge for
stdio-only clients and the agent-aware CLI as the local path.
[decisions/0014-agent-facing-surface.md](decisions/0014-agent-facing-surface.md) and
[design/agent-interface.md](design/agent-interface.md) are reconciled to this; several pages
(threat model, architecture overview, operations index) were caught up to the removal.


## 2026-08-23

- bootstrap and handoff pages.

- **New page** [operations/ansible-first-bootstrap.md](operations/ansible-first-bootstrap.md):
  the `exocomp bootstrap run <playbook> <workspace>` ansible-first chain, with the
  `--capture` (env) and `--capture-file` (KUBECONFIG-shaped file) credential handoffs added
  in later passes.
- **New page** [design/bootstrap-first.md](design/bootstrap-first.md): the proposed
  ansible-first-then-tofu design for the Kubernetes persona, linked from the v1.1 roadmap.
- **New page** [operations/sealed-tofu-outputs.md](operations/sealed-tofu-outputs.md): the
  credential half of the Terraform→Ansible handoff (`tf-output/<workspace>/<output>` sealed
  and bound under `tf:<workspace>`).
- **New page** [operations/cli-run.md](operations/cli-run.md): the governed `cli` run kind —
  an allow-listed binary under the queue, sandbox, run-secret injection, and approval gate.
- **Corrections.** [operations/terraform-ansible-handoff.md](operations/terraform-ansible-handoff.md)
  and [operations/provision-converge.md](operations/provision-converge.md) updated as the
  daemon's apply began sealing sensitive outputs on every path; `bootstrap-first.md` Status
  updated as its phases shipped (code wins over the design page).


## 2026-08-22

- reactive, policy, and run pages.

- **New pages**: [architecture/reactive.md](architecture/reactive.md) (webhook ingress,
  normalized event shape, sandboxed CEL, guardrails, the approval token),
  [operations/reactive.md](operations/reactive.md) (wiring senders, `rule`/`silence`/
  `approval`/`event`, the `tofu-plan` schedule), [operations/plan-policy.md](operations/plan-policy.md)
  (the CEL gate over `tofu show -json`), [operations/playbook-run.md](operations/playbook-run.md)
  (ansible passthrough flags and check-mode drift), and
  [operations/provision-converge.md](operations/provision-converge.md) (the
  provision→onboard→converge chain and the `tf:<workspace>` standing inventory).
- The operations index gained rows for the v1.0 pages that had shipped unlinked:
  [run-lifecycle.md](operations/run-lifecycle.md),
  [terraform-ansible-handoff.md](operations/terraform-ansible-handoff.md), and
  [upgrade.md](operations/upgrade.md).


## 2026-08-22

- design handoffs imported.

- Web design handoff under [design/reference/](design/reference/) (canvas PDF v0.1, artboard
  captures); design tokens live at `internal/web/tokens.css`.
- TUI design handoff under [design/reference/tui/](design/reference/tui/) (text frames,
  keymap, colors, copy, states); the frames are golden fixtures under
  `internal/tui/testdata/frames/`. [design/tui-brief.md](design/tui-brief.md) and
  [design/ui-brief.md](design/ui-brief.md) record the settled decisions, including the phone
  approval-surface contract.
- **Correction.** The printed recovery card's derivation block renders from code, not from
  the mock: the shipped envelope uses the constant salt `exocomp/v1` with `info = purpose:id`
  ([Decision 0003](decisions/0003-secrets-envelope.md)); the mock's `exo` command shorthand is not used
  ([Decision 0002](decisions/0002-project-name-exocomp.md)).


## 2026-08-22

- Decision records 0008 and 0009 recorded.

- [Decision 0008](decisions/0008-step-ledger-over-dbos.md): durable execution is a step-ledger extension of the
  job store; DBOS is assessed and deferred (young SQLite path, single maintainer, second
  runtime).
- [Decision 0009](decisions/0009-lan-access-tokens.md): LAN exposure is secured by local-minted bearer
  tokens with default self-signed TLS and fingerprint pinning.


## 2026-08-22

- keyring-generation and v0.5 reconciliation.

- **Correction.** The keyring generation counter lives beside the keyring in
  `keyring.version`, not inside the armored `keyring.age` (age rejects added framing).
  [operations/disaster-recovery.md](operations/disaster-recovery.md) documents
  `keyring.version` and `MANIFEST.mac`, what a restore does with them, and
  `exocomp kit derive`/`kit verify`/`identity migrate`.
- v0.5 reconciliation against code across [operations/serve.md](operations/serve.md),
  [architecture/execution.md](architecture/execution.md), and
  [sbom/bill-of-materials.md](sbom/bill-of-materials.md): queue semantics, executor tiers as
  actually shipped, stale-lock recovery, and drifted module rows corrected.


## 2026-08-21

The `docs/` bundle was created as an OKF v0.2 knowledge base, curated from the
pre-repository design archive. The curated pages summarize; the archive's source documents
were imported byte-identical under [history/](history/index.md) — thirteen files, checked
for absolute paths and home directories (none present).

- **Sections and pages**: `index.md`, this `log.md`, `decisions/` (0001–0006, Accepted),
  `architecture/` (`overview.md`, `platform-contract.md`, `execution.md`), `design/`
  (`requirements.md`, `amendments.md`, `delivery.md`), `operations/` (`bootstrap.md`,
  `disaster-recovery.md`), `sbom/` (`bill-of-materials.md`), and `history/`.
- **Naming.** Most of the archive predates the rename from the working name "control" to
  **exocomp** ([Decision 0002](decisions/0002-project-name-exocomp.md)); curated pages use the current
  name throughout, and imported history was deliberately not rewritten (see
  [history/index.md](history/index.md)).
- **Precedence.** Where a curated page and a history page disagree, the curated page wins;
  the twelve [amendments](design/amendments.md) override the accepted candidate design.


## 2026-08-21

- early corrections against code and source.

- The design critique enumerates **30 gaps across sections A–H**, not 22; the wrong count
  was corrected on seven pages.
- `install.sh` gained the documented `--restore` and `--rollback --image-tag` forms,
  resolving contradictions with the disaster-recovery, bootstrap, and amendments pages in
  code's favor.
- v0.1 pages reconciled against the shipped code: the run engine is a direct
  `ansible-playbook` subprocess (not `ansible-runner`), the disaster-recovery staging tree
  and failure-mode tables match the shipped CLI, and `exocomp backup run`/`restore` are
  documented as direct invocations outside the queue until the scheduler exists.
- **New page** [sbom/minio-assessment.md](sbom/minio-assessment.md): the S3 test fixture
  moved to SeaweedFS; the operator-store row no longer suggests MinIO for new deployments.
