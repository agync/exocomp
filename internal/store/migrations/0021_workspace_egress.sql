-- Per-workspace opt-in egress widening for the tofu sandbox tier. Empty by
-- default: a workspace with no declaration keeps the built-in tofu egress
-- (HTTPS plus the loopback state backend) exactly as before. The value is a
-- JSON array of TCP ports the operator has deliberately granted, e.g. [6443]
-- to let the kubernetes/helm providers reach a kubeadm API server.
ALTER TABLE workspace ADD COLUMN extra_egress_ports TEXT NOT NULL DEFAULT '[]';
