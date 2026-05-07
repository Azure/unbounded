# Docker Bake configuration for unbounded-net-node

variable "REGISTRY" {
  default = "unboundedcnitme.azurecr.io"
}

variable "VERSION" {
  default = "dev"
}

variable "COMMIT" {
  default = "unknown"
}

variable "BUILD_TIME" {
  default = "unknown"
}

variable "CNI_PLUGINS_VERSION" {
  default = "v1.9.0"
}

variable "REACT_DEV" {
  default = "false"
}

variable "PLATFORMS" {
  default = "linux/amd64,linux/arm64"
}

# Additional tags (comma-separated) to apply alongside the default tag.
# Used by CI to add version/latest/SHA tags. Empty means default tag only.
variable "EXTRA_TAGS" {
  default = ""
}

group "default" {
  targets = ["node"]
}

# Node image -- built per-platform, pushed to per-arch tags
target "node" {
  context = "."
  dockerfile = "images/net-node/Dockerfile"
  target = "node"
  platforms = split(",", PLATFORMS)
  tags = concat(
    ["${REGISTRY}/unbounded-net-node:${VERSION}-buildscratch"],
    EXTRA_TAGS != "" ? [for t in split(",", EXTRA_TAGS) : "${REGISTRY}/unbounded-net-node:${t}"] : []
  )
  args = {
    VERSION = VERSION
    COMMIT = COMMIT
    BUILD_TIME = BUILD_TIME
    CNI_PLUGINS_VERSION = CNI_PLUGINS_VERSION
    REACT_DEV = REACT_DEV
  }
  output = ["type=registry"]
  attest = []
}
