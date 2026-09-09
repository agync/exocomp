terraform {
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.0"
    }
  }
  backend "http" {}
}

variable "namespace_name" {
  type    = string
  default = "exocomp-e2e"
}

variable "marker_value" {
  type    = string
  default = "hello-exocomp"
}

variable "kubeconfig_path" {
  type    = string
  default = "./kubeconfig.yaml"
}

provider "kubernetes" {
  config_path = var.kubeconfig_path
}

resource "kubernetes_namespace" "e2e" {
  metadata {
    name = var.namespace_name
  }
}

resource "kubernetes_config_map" "marker" {
  depends_on = [kubernetes_namespace.e2e]
  metadata {
    name      = "e2e-marker"
    namespace = var.namespace_name
  }

  data = {
    marker = var.marker_value
  }
}
