// exocomp landing page
(function () {
  'use strict';

  /* Theme ------------------------------------------------------------------ */

  const themeToggle = document.getElementById('theme-toggle');
  const ICON_SUN = '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="5"></circle><line x1="12" y1="1" x2="12" y2="3"></line><line x1="12" y1="21" x2="12" y2="23"></line><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"></line><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"></line><line x1="1" y1="12" x2="3" y2="12"></line><line x1="21" y1="12" x2="23" y2="12"></line><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"></line><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"></line></svg>';
  const ICON_MOON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"></path></svg>';
  const darkQuery = window.matchMedia('(prefers-color-scheme: dark)');

  const heroShot = document.getElementById('hero-shot');
  const heroTheme = document.getElementById('hero-theme');

  function applyTheme(theme, persist) {
    document.documentElement.setAttribute('data-theme', theme);
    if (persist) {
      try { localStorage.setItem('exocomp-theme', theme); } catch (e) {}
    }
    if (heroShot) {
      const next = theme === 'dark' ? heroShot.dataset.dark : heroShot.dataset.light;
      if (next && heroShot.getAttribute('src') !== next) heroShot.setAttribute('src', next);
    }
    if (heroTheme) {
      heroTheme.textContent = theme === 'dark' ? 'Obsidian · dark' : 'Lab notebook · light';
    }
    if (!themeToggle) return;
    const isDark = theme === 'dark';
    themeToggle.innerHTML = isDark ? ICON_SUN : ICON_MOON;
    themeToggle.setAttribute('aria-label', isDark ? 'Switch to light theme' : 'Switch to dark theme');
    themeToggle.title = isDark ? 'Switch to light theme' : 'Switch to dark theme';
  }

  let stored = null;
  try { stored = localStorage.getItem('exocomp-theme'); } catch (e) {}
  applyTheme(stored || (darkQuery.matches ? 'dark' : 'light'), false);

  if (themeToggle) {
    themeToggle.addEventListener('click', function () {
      const next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
      applyTheme(next, true);
    });
  }

  darkQuery.addEventListener('change', function (e) {
    let current = null;
    try { current = localStorage.getItem('exocomp-theme'); } catch (err) {}
    if (!current) applyTheme(e.matches ? 'dark' : 'light', false);
  });

  /* Mobile navigation ------------------------------------------------------ */

  const header = document.querySelector('.site-header');
  const navToggle = document.getElementById('nav-toggle');
  const nav = document.getElementById('primary-nav');

  function setNav(open, returnFocus) {
    if (!header || !navToggle) return;
    header.classList.toggle('nav-open', open);
    navToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    navToggle.setAttribute('aria-label', open ? 'Close navigation' : 'Open navigation');
    if (open && nav) {
      const first = nav.querySelector('a');
      if (first) first.focus();
    } else if (returnFocus) {
      navToggle.focus();
    }
  }

  if (navToggle && header) {
    navToggle.addEventListener('click', function () {
      const open = !header.classList.contains('nav-open');
      setNav(open, !open);
    });
    if (nav) {
      nav.addEventListener('click', function (e) {
        if (e.target.closest('a')) setNav(false);
      });
    }
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape' || !header.classList.contains('nav-open')) return;
      setNav(false, true);
    });
    document.addEventListener('click', function (e) {
      if (!header.classList.contains('nav-open')) return;
      if (e.target.closest('.site-header')) return;
      setNav(false, false);
    });
    window.addEventListener('resize', function () {
      if (window.innerWidth > 820) setNav(false, false);
    });
  }

  /* Tabs ------------------------------------------------------------------- */

  // One roving-tabindex handler per tablist: arrow keys move focus and
  // selection, Home and End jump to the ends.
  function wireTabs(tablist, activate) {
    const tabs = Array.prototype.slice.call(tablist.querySelectorAll('[role="tab"]'));
    if (!tabs.length) return;

    function select(tab, focus) {
      tabs.forEach(function (t) {
        const on = t === tab;
        t.classList.toggle('is-active', on);
        t.setAttribute('aria-selected', on ? 'true' : 'false');
        t.tabIndex = on ? 0 : -1;
      });
      if (focus) tab.focus();
      activate(tab);
    }

    tabs.forEach(function (tab, i) {
      tab.addEventListener('click', function () { select(tab, false); });
      tab.addEventListener('keydown', function (e) {
        let next = null;
        if (e.key === 'ArrowRight') next = tabs[(i + 1) % tabs.length];
        else if (e.key === 'ArrowLeft') next = tabs[(i - 1 + tabs.length) % tabs.length];
        else if (e.key === 'Home') next = tabs[0];
        else if (e.key === 'End') next = tabs[tabs.length - 1];
        if (next) {
          e.preventDefault();
          select(next, true);
        }
      });
    });
  }

  /* Gallery ---------------------------------------------------------------- */

  const gallery = document.querySelector('[data-gallery]');
  if (gallery) {
    const img = document.getElementById('gallery-image');
    const fig = document.getElementById('gallery-fig');
    const meta = document.getElementById('gallery-meta');
    const caption = document.getElementById('gallery-caption');
    const link = document.getElementById('gallery-link');
    const panel = document.getElementById('gallery-panel');

    wireTabs(gallery.querySelector('.tabbar'), function (tab) {
      if (img) {
        img.src = tab.dataset.src;
        img.alt = tab.dataset.alt;
        img.width = tab.dataset.w;
        img.height = tab.dataset.h;
      }
      if (fig) fig.textContent = tab.dataset.fig;
      if (meta) meta.textContent = tab.dataset.meta;
      if (caption) caption.textContent = tab.dataset.caption;
      if (link) {
        link.href = tab.dataset.href;
        link.innerHTML = tab.dataset.link + ' <span aria-hidden="true">→</span>';
      }
      if (panel) panel.setAttribute('aria-labelledby', tab.id);
    });
  }

  /* Command line examples -------------------------------------------------- */

  const cliData = {
    json: {
      cmd: "exocomp command --host 'web*' --output json -- hostname -f | jq",
      out: `{
  "schema_version": 1,
  "kind": "command.result",
  "data": {
    "job_id": 42,
    "state": "succeeded",
    "exit_code": 0,
    "hosts": {
      "web01": {
        "status": "CHANGED",
        "rc": 0,
        "changed": true,
        "stdout": "web01.internal.homelab"
      },
      "web02": {
        "status": "CHANGED",
        "rc": 0,
        "changed": true,
        "stdout": "web02.internal.homelab"
      }
    }
  }
}`
    },
    drift: {
      cmd: 'exocomp playbook run playbooks/site.yml --source homelab --check',
      out: `PLAY RECAP *********************************************************************
web01  : ok=14  changed=3  unreachable=0  failed=0  skipped=2
web02  : ok=14  changed=0  unreachable=0  failed=0  skipped=2

ansible check: 1 host(s) clean, host web01: 3 task(s) would change

{"ansible_check": {"operation": "check", "changes": true,
  "clean_hosts": 1, "drift": [{"host": "web01", "changed": 3}]}}`
    },
    live: {
      cmd: 'exocomp jobs get 89',
      out: 'id             89\nstate          running\nprogress       PLAY [Site Deploy] · TASK [nginx : install package] · 14.2s elapsed\nrunbook        deploy/site.yml\nrunbook (raw)  deploy/site.yml\nsource         homelab\nkind           ansible\ntrigger        schedule\nexit           -\nqueued         2026-11-01 02:40:00Z'
    }
  };

  const cliPanel = document.getElementById('cli-panel');
  const cliCmd = document.getElementById('cli-cmd');
  const cliOut = document.getElementById('cli-out');
  const cliTabbar = document.querySelector('.cli-term .tabbar');

  if (cliTabbar && cliCmd && cliOut) {
    wireTabs(cliTabbar, function (tab) {
      const data = cliData[tab.dataset.key];
      if (!data) return;
      cliCmd.textContent = data.cmd;
      cliOut.textContent = data.out;
      if (cliPanel) cliPanel.setAttribute('aria-labelledby', tab.id);
    });
  }

  /* Install snippets ------------------------------------------------------- */

  const installTabbar = document.querySelector('.install .tabbar');
  const installPanel = document.getElementById('install-panel');
  const installSnippets = Array.prototype.slice.call(document.querySelectorAll('.install-snippet'));
  const copyBtn = document.getElementById('install-copy');

  if (installTabbar && installSnippets.length) {
    wireTabs(installTabbar, function (tab) {
      installSnippets.forEach(function (pre) {
        pre.hidden = pre.dataset.snippet !== tab.dataset.snippet;
      });
      if (installPanel) installPanel.setAttribute('aria-labelledby', tab.id);
      if (copyBtn) copyBtn.textContent = 'Copy';
    });
  }

  if (copyBtn) {
    copyBtn.addEventListener('click', function () {
      const pre = installSnippets.filter(function (p) { return !p.hidden; })[0];
      if (!pre) return;
      const text = pre.textContent;
      const done = function () {
        copyBtn.textContent = 'Copied';
        window.setTimeout(function () { copyBtn.textContent = 'Copy'; }, 1800);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function () {
          copyBtn.textContent = 'Copy failed';
          window.setTimeout(function () { copyBtn.textContent = 'Copy'; }, 1800);
        });
      } else {
        copyBtn.textContent = 'Copy failed';
        window.setTimeout(function () { copyBtn.textContent = 'Copy'; }, 1800);
      }
    });
  }
})();
