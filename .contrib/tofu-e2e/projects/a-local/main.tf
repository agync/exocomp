terraform {
  required_providers {
    local = {
      source  = "hashicorp/local"
      version = "~> 2.0"
    }
  }
  backend "http" {}
}

variable "marker_text" {
  type    = string
  default = "exocomp-e2e-initial"
}

variable "marker_relpath" {
  type    = string
  default = "out/marker.txt"
}

# A marker file inside the workspace dir (the run's cwd, granted rw by the
# sandbox). The file's content is the whole point: plan sees "absent" or
# "content differs" as drift; apply writes it. Managed through the local
# provider so the state backend, TF_ENCRYPTION, and the plan/apply/approval
# machinery get exercised without needing any external target.
resource "local_file" "marker" {
  filename        = "${path.module}/${var.marker_relpath}"
  content         = var.marker_text
  file_permission = "0644"
}

output "marker" {
  value     = local_file.marker.content
  sensitive = false
}
