# Docker Bake configuration for unbounded-net-controller

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

variable "PLATFORMS" {
  default = "linux/amd64,linux/arm64"
}

# Additional tags (comma-separated) to apply alongside the default tag.
# Used by CI to add version/latest/SHA tags. Empty means default tag only.
variable "EXTRA_TAGS" {
  default = ""
}

group "default" {
  targets = ["controller"]
}

# Controller image -- built per-platform, pushed to per-arch tags
target "controller" {
  context = "."
  dockerfile = "images/net-controller/Dockerfile"
  target = "controller"
  platforms = split(",", PLATFORMS)
  tags = concat(
    ["${REGISTRY}/unbounded-net-controller:${VERSION}-buildscratch"],
    EXTRA_TAGS != "" ? [for t in split(",", EXTRA_TAGS) : "${REGISTRY}/unbounded-net-controller:${t}"] : []
  )
  args = {
    VERSION = VERSION
    COMMIT = COMMIT
    BUILD_TIME = BUILD_TIME
    CNI_PLUGINS_VERSION = CNI_PLUGINS_VERSION
  }
  output = ["type=registry"]
  attest = []
}
