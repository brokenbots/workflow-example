schema_version = 1
adapter "shell" "reviewer" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.2"
  version              = "0.5.2"
  resolved_digest      = "sha256:38f23c92a11548ce57c54e9312c567558d3ad017fd632adfba305b058988703d"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-shell/.github/workflows/publish.yml@refs/tags/v0.5.2"
    }
  }
}
adapter "shell" "workflow" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.2"
  version              = "0.5.2"
  resolved_digest      = "sha256:38f23c92a11548ce57c54e9312c567558d3ad017fd632adfba305b058988703d"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-shell/.github/workflows/publish.yml@refs/tags/v0.5.2"
    }
  }
}
