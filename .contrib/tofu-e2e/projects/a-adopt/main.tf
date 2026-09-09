terraform {
  required_providers {
    local = {
      source  = "hashicorp/local"
      version = "~> 2.0"
    }
  }
  # No backend block: used by the S04 adoption scenario to establish local
  # state (backend "local" {} added by the runbook), then adopted onto the
  # HTTP backend exocomp's driver injects.
}

variable "marker_text" {
  type    = string
  default = "exocomp-e2e-adopt"
}

variable "marker_relpath" {
  type    = string
  default = "out/marker.txt"
}

resource "local_file" "marker" {
  filename        = "${path.module}/${var.marker_relpath}"
  content         = var.marker_text
  file_permission = "0644"
}

output "marker" {
  value     = local_file.marker.content
  sensitive = false
}
