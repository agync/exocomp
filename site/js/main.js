// exocomp landing page interactive behaviors
(function () {
  'use strict';

  // 0. Mobile navigation drawer (<=820px)
  const siteHeader = document.querySelector('.site-header');
  const navToggle = document.getElementById('nav-toggle');
  const primaryNav = document.getElementById('primary-nav');
  const ICON_OPEN = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><line x1="3" y1="6" x2="21" y2="6"></line><line x1="3" y1="12" x2="21" y2="12"></line><line x1="3" y1="18" x2="21" y2="18"></line></svg>';
  const ICON_CLOSE = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><line x1="18" y1="6" x2="6" y2="18"></line><line x1="6" y1="6" x2="18" y2="18"></line></svg>';

  function setNav(open) {
    if (!siteHeader || !navToggle) return;
    siteHeader.classList.toggle('nav-open', open);
    navToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    navToggle.setAttribute('aria-label', open ? 'Close navigation' : 'Open navigation');
    navToggle.innerHTML = open ? ICON_CLOSE : ICON_OPEN;
  }

  if (navToggle && siteHeader) {
    navToggle.addEventListener('click', () => {
      setNav(!siteHeader.classList.contains('nav-open'));
    });
    if (primaryNav) {
      primaryNav.addEventListener('click', (e) => {
        if (e.target.closest('a')) setNav(false);
      });
    }
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') setNav(false);
    });
    document.addEventListener('click', (e) => {
      if (!siteHeader.classList.contains('nav-open')) return;
      if (e.target.closest('.site-header')) return;
      setNav(false);
    });
    window.addEventListener('resize', () => {
      if (window.innerWidth > 820) setNav(false);
    });
  }

  // Syntax highlighting for landing-page code snippets
  function escapeHtml(value) {
    return value
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;');
  }

  function highlightSnippet(value) {
    var html = escapeHtml(value);
    // comments
    html = html.replace(/(^|\n)(\s*#.*)/g, '$1<span class="hl-comment">$2</span>');
    // section headers [Unit]
    html = html.replace(/(\[([A-Za-z0-9 _-]+)\])/g, '<span class="hl-section">$1</span>');
    // strings
    html = html.replace(/('([^']*)')/g, '<span class="hl-string">$1</span>');
    html = html.replace(/(\"([^\"]*)\")/g, '<span class="hl-string">$1</span>');
    // ini / yaml keys at line start
    html = html.replace(/(^|\n)(\s*[A-Za-z_][\w-]*)(\s*[:=])/g, '$1<span class="hl-key">$2</span>$3');
    // variables
    html = html.replace(/(%[A-Za-z]+|%\{[^}]*\}|\$\{[^}]*\}|\$[A-Za-z_]\w*)/g, '<span class="hl-var">$1</span>');
    return html;
  }

  // 1. Theme Management (dark native / light warm lab notebook)
  const themeToggle = document.getElementById('theme-toggle');
  const storedTheme = localStorage.getItem('exocomp-theme');
  const systemPrefersDark = window.matchMedia('(prefers-color-scheme: dark)');

  function applyTheme(theme, persist) {
    document.documentElement.setAttribute('data-theme', theme);
    if (persist !== false) localStorage.setItem('exocomp-theme', theme);
    updateThemeIcon(theme);
  }

  function updateThemeIcon(theme) {
    if (!themeToggle) return;
    const isDark = theme === 'dark';
    themeToggle.setAttribute('aria-label', isDark ? 'Switch to light theme' : 'Switch to dark theme');
    themeToggle.title = isDark ? 'Switch to light theme (warm lab notebook)' : 'Switch to dark theme (native graphite)';
    themeToggle.innerHTML = isDark
      ? `<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="5"></circle><line x1="12" y1="1" x2="12" y2="3"></line><line x1="12" y1="21" x2="12" y2="23"></line><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"></line><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"></line><line x1="1" y1="12" x2="3" y2="12"></line><line x1="21" y1="12" x2="23" y2="12"></line><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"></line><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"></line></svg>`
      : `<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"></path></svg>`;
  }

  const initialTheme = storedTheme || (systemPrefersDark.matches ? 'dark' : 'light');
  applyTheme(initialTheme, !!storedTheme);

  if (themeToggle) {
    themeToggle.addEventListener('click', () => {
      const current = document.documentElement.getAttribute('data-theme');
      applyTheme(current === 'dark' ? 'light' : 'dark');
    });
  }

  systemPrefersDark.addEventListener('change', (e) => {
    if (!localStorage.getItem('exocomp-theme')) {
      applyTheme(e.matches ? 'dark' : 'light', false);
    }
  });

  // 2. Deployment Code Snippets (Host Service vs Container)
  const snippets = {
    systemd: `# Install as a user service on any Linux host
mkdir -p ~/.config/systemd/user
cat << 'EOF' > ~/.config/systemd/user/exocomp.service
[Unit]
Description=exocomp homelab control plane
After=network.target

[Service]
ExecStart=%h/.local/bin/exocompd --port 8080
Restart=always
RestartSec=5s
Environment=XDG_DATA_HOME=%h/.local/share

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload && systemctl --user enable --now exocomp`,

    binary: `# Download and run the standalone binary directly
curl -fsSL https://github.com/agync/exocomp/releases/latest/download/exocomp \\
  -o ~/.local/bin/exocomp && chmod +x ~/.local/bin/exocomp

# Run setup wizard & start the daemon
exocomp setup
exocomp serve --port 8080`,

    quadlet: `# Rootless Podman Quadlet on Fedora / RHEL
mkdir -p ~/.config/containers/systemd
cat << 'EOF' > ~/.config/containers/systemd/exocomp.container
[Unit]
Description=exocomp homelab control plane
After=network-online.target

[Container]
Image=ghcr.io/agync/exocomp:latest
ContainerName=exocomp
PublishPort=127.0.0.1:8080:8080
Volume=%h/.local/share/exocomp:/data:Z

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload && systemctl --user start exocomp`,

    compose: `# Docker Compose rootless container
services:
  exocomp:
    image: ghcr.io/agync/exocomp:latest
    container_name: exocomp
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - /var/srv/exocomp:/data:Z
    security_opt:
      - label:disable`
  };

  const quickTabs = document.querySelectorAll('.quickstart-tab');
  const quickCode = document.getElementById('quickstart-target');
  const copyBtn = document.getElementById('quickstart-copy');

  quickTabs.forEach((tab) => {
    tab.addEventListener('click', () => {
      quickTabs.forEach((t) => {
        t.classList.remove('active');
        t.setAttribute('aria-selected', 'false');
      });
      tab.classList.add('active');
      tab.setAttribute('aria-selected', 'true');
      const key = tab.dataset.snippet;
      if (snippets[key] && quickCode) {
        quickCode.innerHTML = highlightSnippet(snippets[key]);
      }
    });
  });

  if (quickCode) {
    quickCode.innerHTML = highlightSnippet(snippets.systemd);

    var quickHost = quickCode.parentElement;
    function setQuickstartHeight() {
      if (!quickHost) return;
      quickHost.style.minHeight = '';
      var codeStyle = window.getComputedStyle(quickCode);
      var tmp = document.createElement('pre');
      tmp.style.position = 'absolute';
      tmp.style.visibility = 'hidden';
      tmp.style.pointerEvents = 'none';
      tmp.style.whiteSpace = 'pre';
      tmp.style.fontFamily = codeStyle.fontFamily;
      tmp.style.fontSize = codeStyle.fontSize;
      tmp.style.lineHeight = codeStyle.lineHeight;
      tmp.style.width = quickCode.clientWidth + 'px';
      tmp.style.paddingRight = '88px';
      document.body.appendChild(tmp);
      var height = 0;
      Object.keys(snippets).forEach(function (key) {
        tmp.innerHTML = highlightSnippet(snippets[key]);
        height = Math.max(height, tmp.scrollHeight);
      });
      document.body.removeChild(tmp);
      quickHost.style.minHeight = (height + 32) + 'px';
    }
    setQuickstartHeight();
    var quickTimer = null;
    window.addEventListener('resize', function () {
      if (quickTimer) clearTimeout(quickTimer);
      quickTimer = setTimeout(setQuickstartHeight, 150);
    });
  }

  if (copyBtn && quickCode) {
    copyBtn.addEventListener('click', () => {
      navigator.clipboard.writeText(quickCode.textContent).then(() => {
        const orig = copyBtn.textContent;
        copyBtn.textContent = 'Copied!';
        setTimeout(() => { copyBtn.textContent = orig; }, 1800);
      });
    });
  }

  // 3. Surface Showcase Gallery
  const galleryTabs = document.querySelectorAll('.gallery-tab');
  const galleryImg = document.getElementById('gallery-image');
  const galleryCaption = document.getElementById('gallery-caption-text');
  const galleryLink = document.getElementById('gallery-caption-link');

  galleryTabs.forEach((tab) => {
    tab.addEventListener('click', () => {
      galleryTabs.forEach((t) => {
        t.classList.remove('active');
        t.setAttribute('aria-selected', 'false');
      });
      tab.classList.add('active');
      tab.setAttribute('aria-selected', 'true');

      const src = tab.dataset.src;
      const alt = tab.dataset.alt;
      const caption = tab.dataset.caption;
      const docLink = tab.dataset.link;
      const docLabel = tab.dataset.label;

      if (galleryImg) {
        galleryImg.src = src;
        galleryImg.alt = alt;
      }
      if (galleryCaption) {
        galleryCaption.textContent = caption;
      }
      if (galleryLink) {
        galleryLink.href = docLink;
        galleryLink.textContent = docLabel + ' →';
      }
    });
  });

  // 4. Interactive CLI Command Explorer
  const cliData = {
    adhoc_json: {
      cmd: "exocomp command --host 'web*' --output json -- hostname -f",
      output: `{"schema_version":1,"kind":"command.result","data":{"job_id":42,"state":"succeeded","exit_code":0,"hosts":{"web01":{"status":"CHANGED","rc":0,"changed":true,"stdout":"web01.internal.homelab"},"web02":{"status":"CHANGED","rc":0,"changed":true,"stdout":"web02.internal.homelab"}}}}`
    },
    empathy: {
      cmd: "exocomp command --host web01 -m setup",
      output: `Error: command: '-m' and '-a' are Ansible flags, not 'exocomp command' flags.
  - To run a command: exocomp command --host <pattern> [--shell] -- <cmd...>
  - To run an Ansible module: exocomp ansible --host <pattern> -m <module> [-a <args>]`
    },
    user_session: {
      cmd: "exocomp command --host workstation --user-session -- systemctl --user status podman",
      output: `workstation | CHANGED | rc=0 >>
● podman.service - Podman API Service
     Loaded: loaded (/usr/lib/systemd/user/podman.service; enabled; preset: disabled)
     Active: active (running) since Mon 2026-10-12 18:30:12 UTC; 10h ago
       Docs: man:podman-system-service(1)
   Main PID: 1422 (podman)
      Tasks: 11 (limit: 18882)`
    },
    progress: {
      cmd: "exocomp jobs get 89",
      output: `id             89
state          running
progress       PLAY [Site Deploy] · TASK [nginx : install package] · 14.2s elapsed
runbook        deploy/site.yml
runbook (raw)  deploy/site.yml
source         homelab
kind           ansible
trigger        schedule
exit           -
queued         2026-10-13 02:40:00Z`
    },
    dr_verify: {
      cmd: "./recovery.sh --verify-only",
      output: `[ok] age private key decrypted from envelope
[ok] restic repository accessible (nas01.ts.net:22)
[ok] latest snapshot 8e1d07cccccc verified (clean, 48MB)
[ok] litestream replica in sync with WAL sequence 4902
[ok] all recovery prerequisites satisfied; cold restore ready`
    }
  };

  const cliPills = document.querySelectorAll('.cli-pill');
  const cliCmdElem = document.getElementById('cli-cmd-display');
  const cliOutputElem = document.getElementById('cli-output-display');
  const cliScreen = document.querySelector('.cli-screen');

  if (cliScreen) {
    var cliScreenEl = cliScreen;
    function setCliHeight() {
      if (!cliScreenEl.isConnected) return;
      var cs = window.getComputedStyle(cliScreenEl);
      var t2 = document.createElement('div');
      t2.style.position = 'absolute';
      t2.style.visibility = 'hidden';
      t2.style.pointerEvents = 'none';
      t2.style.whiteSpace = 'pre-wrap';
      t2.style.fontFamily = cs.fontFamily;
      t2.style.fontSize = cs.fontSize;
      t2.style.lineHeight = cs.lineHeight;
      t2.style.width = cliScreenEl.clientWidth + 'px';
      t2.style.padding = cs.padding;
      document.body.appendChild(t2);
      var h = 0;
      Object.keys(cliData).forEach(function (key) {
        var dd = cliData[key];
        t2.textContent = '$ ' + dd.cmd + '\n' + dd.output;
        h = Math.max(h, t2.scrollHeight);
      });
      document.body.removeChild(t2);
      cliScreenEl.style.minHeight = (h + 8) + 'px';
    }
    var cliStyle = window.getComputedStyle(cliScreen);
    var tmp = document.createElement('div');
    tmp.style.position = 'absolute';
    tmp.style.visibility = 'hidden';
    tmp.style.pointerEvents = 'none';
    tmp.style.whiteSpace = 'pre-wrap';
    tmp.style.fontFamily = cliStyle.fontFamily;
    tmp.style.fontSize = cliStyle.fontSize;
    tmp.style.lineHeight = cliStyle.lineHeight;
    tmp.style.width = cliScreen.clientWidth + 'px';
    tmp.style.padding = '20px 24px';
    document.body.appendChild(tmp);
    var cliHeight = 0;
    Object.keys(cliData).forEach(function (key) {
      var d = cliData[key];
      tmp.textContent = '$ ' + d.cmd + '\\n' + d.output;
      cliHeight = Math.max(cliHeight, tmp.scrollHeight);
    });
    document.body.removeChild(tmp);
    cliScreen.style.minHeight = (cliHeight + 8) + 'px';
    setCliHeight();
    var cliTimer = null;
    window.addEventListener('resize', function () {
      if (cliTimer) clearTimeout(cliTimer);
      cliTimer = setTimeout(setCliHeight, 150);
    });
  }

  cliPills.forEach((pill) => {
    pill.addEventListener('click', () => {
      cliPills.forEach((p) => {
        p.classList.remove('active');
        p.setAttribute('aria-selected', 'false');
      });
      pill.classList.add('active');
      pill.setAttribute('aria-selected', 'true');

      const key = pill.dataset.key;
      const data = cliData[key];
      if (data && cliCmdElem && cliOutputElem) {
        cliCmdElem.textContent = data.cmd;
        cliOutputElem.textContent = data.output;
      }
    });
  });

})();
