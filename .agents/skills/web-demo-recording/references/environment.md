# Environment: the TLS daemon + tailscale HTTPS chain

Why: the browser must see a **valid tailnet cert** with **no "not secure"**
banner. Exocomp's own banner ("This sign-in is not secure") only renders when
the daemon serves with `--web-insecure-http`; the TLS daemon shows the cert
fingerprint instead, and the tailscale `https+insecure://` proxy terminates a
real Let's Encrypt cert in the browser.

## 1. Build + clean

```sh
make build                      # fresh bin/exocomp from main
# if the demo touches the k3s cluster, clean it (provider refuses adopt):
kubectl delete namespace exocomp-e2e --ignore-not-found
kubectl delete configmap e2e-marker -n exocomp-e2e --ignore-not-found
```

## 2. Start the TLS daemon

```sh
EXOCOMP_E2E_TLS=1 EXOCOMP_BIN=bin/exocomp .contrib/tofu-e2e/lib/daemon.sh start
```
- `EXOCOMP_E2E_TLS=1` makes the harness serve `--web-addr` WITHOUT
  `--web-insecure-http` (real self-signed TLS, Decision 0009). Verify:
  `grep -E "listening|certificate" /tmp/exocomp-throwaway/tofu-e2e/daemon.log`
  → `web surface listening on https://[::]:8642` + a fingerprint.
- The matrix default stays `--web-insecure-http` for its `http://` curl/MCP
  legs; only the browser demo uses TLS.

## 3. The tailscale proxy

```sh
timeout 30 tailscale serve --https 8643 --bg --yes https+insecure://127.0.0.1:8642 </dev/null
```
- `--bg` is required — foreground exits when the command's stdout closes.
- stdin `< /dev/null` avoids the interactive Ctrl+C prompt.
- `https+insecure://` backend scheme because exocomp's cert is self-signed.
- Verify: `tailscale serve status --json | python3 -c "import sys,json; print(list(json.load(sys.stdin)['TCP'].keys()))"` shows `8643`.
- The URL is `https://nibbler.tailnet.ts.net:8643/`.
- Teardown: `tailscale serve --https=8643 off` (leaves existing 443/8443 alone).

## 4. Seed your data

Whatever the demo shows (a tofu workspace, an ansible source, a form), seed it
before recording so the surface is populated. For the k3s demo:

```sh
export XDG_DATA_HOME=/tmp/exocomp-throwaway/tofu-e2e
WS=$XDG_DATA_HOME/ws/k3s
rm -rf $WS; mkdir -p $WS
cp -R .contrib/tofu-e2e/projects/b-k8s/. $WS/
cp ~/.lima/exocomp-k3s/copied-from-guest/kubeconfig.yaml $WS/kubeconfig.yaml
bin/exocomp workspace add k3s --dir $WS
bin/exocomp workspace egress k3s 6443
```

## 5. The token

`.contrib/tofu-e2e/lib/daemon.sh` writes `tokens.env`:
```sh
export EXOCOMP_OPERATOR_TOKEN=exo_<id>_<secret>
export EXOCOMP_READONLY_TOKEN=exo_<id>_<secret>
```
Paste the whole `exo_<id>_<secret>` into the login box. The token NAME
(`sandbox-ui`) is NOT the secret.

## Why the pieces matter

- TLS daemon kills exocomp's own insecure banner (it renders only under
  `--web-insecure-http`).
- Tailscale kills the browser's cert warning (real cert vs self-signed).
- Both together: the login page shows "certificate fingerprint", and the wire
  is valid from the browser to exocomp.
